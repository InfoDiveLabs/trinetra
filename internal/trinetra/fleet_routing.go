// Package trinetra: fleet_routing.go is the master's routing/escalation
// config store (spec 6/task 5): validated, versioned persistence of
// AlertingConfig to alerting.json, and resolveRoute -- the ONE function both
// the alerting engine's real delivery (fleet_engine.go) and
// FleetAPI.RouteTest (fleet_provider.go) call to decide which policy applies
// to a given (node, tags, rule, severity). Keeping route selection in this
// single function is what makes RouteTest a trustworthy dry run: it can never
// diverge from what the engine itself would do for the same input.
package trinetra

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// defaultPolicyName is the name of the built-in policy used when
// alerting.json has never been saved (or was saved as a totally empty
// config): a single immediate step delivering to every enabled channel,
// resolved always sent -- today's pre-routing behaviour, byte for byte.
const defaultPolicyName = "default"

// builtinDefaultPolicy returns the policy defaultAlertingConfig uses when no
// alerting config has ever been saved.
func builtinDefaultPolicy() core.Policy {
	t := true
	return core.Policy{
		Name:         defaultPolicyName,
		Steps:        []core.PolicyStep{{After: "0s", Channels: []string{"*"}}},
		SendResolved: &t,
	}
}

// defaultAlertingConfig is what FleetAPI.Alerting reports, and what
// resolveRoute falls back to, when the store has no saved config (Version 0,
// never persisted).
func defaultAlertingConfig() core.AlertingConfig {
	return core.AlertingConfig{
		Version:       0,
		Policies:      []core.Policy{builtinDefaultPolicy()},
		DefaultPolicy: defaultPolicyName,
	}
}

// --- validation ----------------------------------------------------------

// validGlob reports whether pattern is a syntactically valid path.Match
// glob, checked against "" per the task-5 ruling (path.Match's error, not
// its match result, is what a bad pattern like "[" produces).
func validGlob(pattern string) bool {
	_, err := path.Match(pattern, "")
	return err == nil
}

// validDuration reports whether s parses with time.ParseDuration. "" is
// valid wherever a duration is optional (Policy.RepeatEvery); callers that
// require a duration (PolicyStep.After) reject "" themselves first.
func validDuration(s string) bool {
	_, err := time.ParseDuration(s)
	return err == nil
}

// validateAlertingConfig checks cfg atomically: every error it can find is
// worth reporting, but the first one found is returned (SetAlerting never
// saves a partially-valid config either way). validChannel reports whether a
// channel name exists in the master's current config.Channels.
func validateAlertingConfig(cfg core.AlertingConfig, validChannel func(name string) bool) error {
	// A totally empty ROUTING config (no routes, no policies, no default) is
	// the explicit "reset to the built-in default" case -- always valid, and
	// skips every routing-specific check below. Rules (task 7) are a
	// separate concern validated unconditionally further down: a config that
	// only sets Rules, leaving routing untouched/default, must not be forced
	// to also supply a DefaultPolicy just to save its rules.
	if len(cfg.Routes) == 0 && len(cfg.Policies) == 0 && cfg.DefaultPolicy == "" {
		return validateRules(cfg.Rules)
	}

	policyNames := map[string]bool{}
	for _, p := range cfg.Policies {
		if strings.TrimSpace(p.Name) == "" {
			return errors.New("every policy needs a name")
		}
		if policyNames[p.Name] {
			return fmt.Errorf("duplicate policy name %q", p.Name)
		}
		policyNames[p.Name] = true
		if len(p.Steps) == 0 {
			return fmt.Errorf("policy %q: needs at least one step", p.Name)
		}
		for i, st := range p.Steps {
			if st.After == "" || !validDuration(st.After) {
				return fmt.Errorf("policy %q step %d: invalid duration %q", p.Name, i, st.After)
			}
			for _, ch := range st.Channels {
				if ch == "*" {
					continue
				}
				if !validChannel(ch) {
					return fmt.Errorf("policy %q step %d: unknown channel %q", p.Name, i, ch)
				}
			}
		}
		if p.RepeatEvery != "" && !validDuration(p.RepeatEvery) {
			return fmt.Errorf("policy %q: invalid repeat_every %q", p.Name, p.RepeatEvery)
		}
	}

	if strings.TrimSpace(cfg.DefaultPolicy) == "" {
		return errors.New("default_policy is required")
	}
	if !policyNames[cfg.DefaultPolicy] {
		return fmt.Errorf("default_policy: unknown policy %q", cfg.DefaultPolicy)
	}

	routeNames := map[string]bool{}
	for _, r := range cfg.Routes {
		// Route names are REQUIRED and unique (fleet-ui-c task C3 fix round
		// 1): the web editor's inline field-error matching needs a stable,
		// unambiguous key per row, and an unnamed route's daemon-side
		// messages used to all collapse onto the same synthetic
		// "(unnamed)" label (routeLabel's old fallback), which made two
		// unnamed routes indistinguishable. Nothing has shipped a routing
		// config yet, so this is not a breaking migration.
		if strings.TrimSpace(r.Name) == "" {
			return errors.New("every route needs a name")
		}
		if routeNames[r.Name] {
			return fmt.Errorf("duplicate route name %q", r.Name)
		}
		routeNames[r.Name] = true
		if strings.TrimSpace(r.Policy) == "" {
			return fmt.Errorf("route %q: policy is required", r.Name)
		}
		if !policyNames[r.Policy] {
			return fmt.Errorf("route %q: unknown policy %q", r.Name, r.Policy)
		}
		if len(r.Matchers) == 0 {
			return fmt.Errorf("route %q: must match something", r.Name)
		}
		for _, m := range r.Matchers {
			if m.Empty() {
				return fmt.Errorf("route %q: must match something", r.Name)
			}
			if m.Node != "" && !validGlob(m.Node) {
				return fmt.Errorf("route %q: invalid node glob %q", r.Name, m.Node)
			}
			if m.Rule != "" && !validGlob(m.Rule) {
				return fmt.Errorf("route %q: invalid rule glob %q", r.Name, m.Rule)
			}
		}
		for _, g := range r.GroupBy {
			if !validGroupByField(g) {
				return fmt.Errorf("route %q: invalid group_by field %q", r.Name, g)
			}
		}
	}
	return validateRules(cfg.Rules)
}

// validGroupByField reports whether g is one of the incident-grouping
// (task 6 part 2) GroupBy fields a route may list: "node", "rule",
// "severity", or "tag:<key>" for any non-empty key.
func validGroupByField(g string) bool {
	switch g {
	case "node", "rule", "severity":
		return true
	}
	tag, ok := strings.CutPrefix(g, "tag:")
	return ok && tag != ""
}

// --- store -----------------------------------------------------------------

// alertingFileV1 is alerting.json's on-disk shape -- identical to
// core.AlertingConfig; kept as its own type only so a future on-disk
// migration has somewhere to hang a V2 without touching the wire/API type.
type alertingFileV1 = core.AlertingConfig

// alertingStore is the master's routing/escalation config store, persisted
// atomically (temp + rename + fsync, via writeFileAtomicSynced) to one JSON file,
// 0600. A missing file behaves exactly like defaultAlertingConfig -- see
// Get.
type alertingStore struct {
	path string

	mu  sync.Mutex
	cfg core.AlertingConfig // Version 0 and unset (zero value) until first loaded/saved
	set bool                // true once cfg has been loaded from disk or saved at least once
}

// loadAlertingStore opens (or creates) the store at path. A missing file is
// not an error: a fresh master has no custom routing yet, and Get reports
// defaultAlertingConfig until the first SetAlerting.
func loadAlertingStore(path string) (*alertingStore, error) {
	s := &alertingStore{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg alertingFileV1
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("fleet: parse %s: %w", path, err)
	}
	s.cfg, s.set = cfg, true
	return s, nil
}

// Get returns the current config: defaultAlertingConfig() if nothing has
// ever been saved.
func (s *alertingStore) Get() core.AlertingConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set {
		return defaultAlertingConfig()
	}
	return cloneAlertingConfig(s.cfg)
}

func cloneAlertingConfig(cfg core.AlertingConfig) core.AlertingConfig {
	cfg.Routes = append([]core.Route(nil), cfg.Routes...)
	cfg.Policies = append([]core.Policy(nil), cfg.Policies...)
	cfg.Rules = append([]core.AggregateRule(nil), cfg.Rules...)
	return cfg
}

// Set validates cfg (atomically: nothing is saved on any error) and, if
// cfg.Version is 0 (unconditional) or matches the currently stored version,
// persists it with Version bumped to storedVersion+1. A non-zero,
// non-matching cfg.Version is rejected with core.ErrConflict, unchanged.
func (s *alertingStore) Set(cfg core.AlertingConfig, validChannel func(name string) bool) (core.AlertingConfig, error) {
	if err := validateAlertingConfig(cfg, validChannel); err != nil {
		return core.AlertingConfig{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := int64(0)
	if s.set {
		stored = s.cfg.Version
	}
	if cfg.Version != 0 && cfg.Version != stored {
		return core.AlertingConfig{}, core.ErrConflict
	}
	cfg.Version = stored + 1
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return core.AlertingConfig{}, err
	}
	if err := writeFileAtomicSynced(s.path, b, 0o600); err != nil {
		return core.AlertingConfig{}, err
	}
	s.cfg, s.set = cfg, true
	return cloneAlertingConfig(cfg), nil
}

// --- route selection ---------------------------------------------------

// routeResolution is resolveRoute's result: which route matched (if any) and
// EVERY policy that applies. B5 fix round 1 ruling: a Continue chain that
// matches several routes escalates each matched policy independently (its
// own steps, its own RepeatEvery, its own SendResolved) rather than merging
// them into one synthetic policy -- so there is nothing left to compute here
// beyond the ordered list of policies themselves; the engine and RouteTest
// both iterate Policies directly.
type routeResolution struct {
	Route    string
	Policies []core.Policy
	// GroupBy is the FIRST matched route's own GroupBy (task 6 part 2),
	// exactly mirroring how Route itself is only ever set from the first
	// match: an empty GroupBy (no route matched, or the matched route didn't
	// set one) means "use the default (rule, severity) group key" -- see
	// groupKeyFor.
	GroupBy []string
}

// resolveRoute evaluates cfg's routes against an alert on the given node
// (id/display name/tags, "" / nil for a master-own alert) with the given
// rule (the alert's Key) and severity: routes are evaluated in order, the
// FIRST match sets Route, and Continue:true on a matched route keeps
// evaluating LATER routes too, each further match's policy appended to
// Policies too -- exactly Alertmanager's well-known "continue" semantics.
// No match at all uses cfg.DefaultPolicy (or the built-in default if that
// isn't set either, e.g. cfg is the zero/empty value); either way exactly
// one policy is returned in that case.
//
// This is the ONE function both the alerting engine's real delivery and
// FleetAPI.RouteTest call: RouteTest can never disagree with what the engine
// would actually do for the same input.
func resolveRoute(cfg core.AlertingConfig, nodeID, nodeName string, tags []string, rule, severity string) routeResolution {
	byName := map[string]core.Policy{}
	for _, p := range cfg.Policies {
		byName[p.Name] = p
	}

	routeName := ""
	var groupBy []string
	firstMatch := true
	var matched []core.Policy
	for _, r := range cfg.Routes {
		if !matchersApply(r.Matchers, nodeID, nodeName, tags, rule, severity) {
			continue
		}
		if routeName == "" {
			routeName = r.Name
		}
		if firstMatch {
			groupBy = r.GroupBy
			firstMatch = false
		}
		if p, ok := byName[r.Policy]; ok {
			matched = append(matched, p)
		}
		if !r.Continue {
			break
		}
	}

	if len(matched) == 0 {
		routeName = ""
		groupBy = nil
		dp := cfg.DefaultPolicy
		if dp == "" {
			dp = defaultPolicyName
		}
		if p, ok := byName[dp]; ok {
			matched = []core.Policy{p}
		} else {
			matched = []core.Policy{builtinDefaultPolicy()}
		}
	}

	return routeResolution{Route: routeName, Policies: matched, GroupBy: groupBy}
}

func sendResolvedOf(p core.Policy) bool {
	if p.SendResolved == nil {
		return true
	}
	return *p.SendResolved
}

// unionStepChannels returns the deduplicated, order-preserving union of
// step-`step`'s Channels across every policy that has that many steps
// (policies with fewer steps simply don't contribute at that index). Used
// for the FIRE leg's single physical dispatch to step 0 across every matched
// policy at once (B5 fix round 1 ruling: "in one dispatch, as now").
func unionStepChannels(policies []core.Policy, step int) []string {
	var all []string
	for _, p := range policies {
		if step < len(p.Steps) {
			all = append(all, p.Steps[step].Channels...)
		}
	}
	return dedupStrings(all)
}

func dedupStrings(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
