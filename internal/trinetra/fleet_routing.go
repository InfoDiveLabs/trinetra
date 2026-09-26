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
	// A totally empty config (no routes, no policies, no default) is the
	// explicit "reset to the built-in default" case -- always valid.
	if len(cfg.Routes) == 0 && len(cfg.Policies) == 0 && cfg.DefaultPolicy == "" {
		return nil
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
		if r.Name != "" {
			if routeNames[r.Name] {
				return fmt.Errorf("duplicate route name %q", r.Name)
			}
			routeNames[r.Name] = true
		}
		if strings.TrimSpace(r.Policy) == "" {
			return fmt.Errorf("route %q: policy is required", routeLabel(r))
		}
		if !policyNames[r.Policy] {
			return fmt.Errorf("route %q: unknown policy %q", routeLabel(r), r.Policy)
		}
		if len(r.Matchers) == 0 {
			return fmt.Errorf("route %q: must match something", routeLabel(r))
		}
		for _, m := range r.Matchers {
			if m.Empty() {
				return fmt.Errorf("route %q: must match something", routeLabel(r))
			}
			if m.Node != "" && !validGlob(m.Node) {
				return fmt.Errorf("route %q: invalid node glob %q", routeLabel(r), m.Node)
			}
			if m.Rule != "" && !validGlob(m.Rule) {
				return fmt.Errorf("route %q: invalid rule glob %q", routeLabel(r), m.Rule)
			}
		}
	}
	return nil
}

func routeLabel(r core.Route) string {
	if r.Name != "" {
		return r.Name
	}
	return "(unnamed)"
}

// --- store -----------------------------------------------------------------

// alertingFileV1 is alerting.json's on-disk shape -- identical to
// core.AlertingConfig; kept as its own type only so a future on-disk
// migration has somewhere to hang a V2 without touching the wire/API type.
type alertingFileV1 = core.AlertingConfig

// alertingStore is the master's routing/escalation config store, persisted
// atomically (temp + rename + fsync, via writeFileSynced) to one JSON file,
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
	if err := writeFileSynced(s.path, b); err != nil {
		return core.AlertingConfig{}, err
	}
	s.cfg, s.set = cfg, true
	return cloneAlertingConfig(cfg), nil
}

// --- route selection ---------------------------------------------------

// routeResolution is resolveRoute's result: everything the engine needs to
// deliver, escalate and repeat-notify one alert.
type routeResolution struct {
	RouteName    string
	PolicyName   string
	Steps        []core.PolicyStep
	RepeatEvery  string
	SendResolved bool
}

// resolveRoute evaluates cfg's routes against an alert on the given node
// (id/display name/tags, "" / nil for a master-own alert) with the given
// rule (the alert's Key) and severity: routes are evaluated in order, the
// FIRST match sets RouteName, and Continue:true on a matched route keeps
// evaluating LATER routes too, each further match's policy also
// contributing its steps (fan-out, see mergeSteps) -- exactly Alertmanager's
// well-known "continue" semantics. No match at all uses cfg.DefaultPolicy
// (or the built-in default if that isn't set either, e.g. cfg is the
// zero/empty value).
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
	var matchedPolicies []core.Policy
	for _, r := range cfg.Routes {
		if !matchersApply(r.Matchers, nodeID, nodeName, tags, rule, severity) {
			continue
		}
		if routeName == "" {
			routeName = r.Name
		}
		if p, ok := byName[r.Policy]; ok {
			matchedPolicies = append(matchedPolicies, p)
		}
		if !r.Continue {
			break
		}
	}

	if len(matchedPolicies) == 0 {
		routeName = ""
		dp := cfg.DefaultPolicy
		if dp == "" {
			dp = defaultPolicyName
		}
		if p, ok := byName[dp]; ok {
			matchedPolicies = []core.Policy{p}
		} else {
			matchedPolicies = []core.Policy{builtinDefaultPolicy()}
		}
	}

	primary := matchedPolicies[0]
	return routeResolution{
		RouteName:    routeName,
		PolicyName:   primary.Name,
		Steps:        mergeSteps(matchedPolicies),
		RepeatEvery:  primary.RepeatEvery,
		SendResolved: sendResolvedOf(primary),
	}
}

func sendResolvedOf(p core.Policy) bool {
	if p.SendResolved == nil {
		return true
	}
	return *p.SendResolved
}

// mergeSteps merges several matched policies' steps (Continue fan-out) by
// index: step i's After is the first policy's that has one, and its
// Channels are the union (deduplicated, order-preserving) of every policy's
// step i Channels. A single matched policy (the overwhelmingly common case)
// passes through unchanged.
func mergeSteps(policies []core.Policy) []core.PolicyStep {
	if len(policies) == 1 {
		return policies[0].Steps
	}
	maxLen := 0
	for _, p := range policies {
		if len(p.Steps) > maxLen {
			maxLen = len(p.Steps)
		}
	}
	merged := make([]core.PolicyStep, 0, maxLen)
	for i := 0; i < maxLen; i++ {
		after := ""
		var channels []string
		for _, p := range policies {
			if i >= len(p.Steps) {
				continue
			}
			if after == "" {
				after = p.Steps[i].After
			}
			channels = append(channels, p.Steps[i].Channels...)
		}
		merged = append(merged, core.PolicyStep{After: after, Channels: dedupStrings(channels)})
	}
	return merged
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

// firstStepChannels returns steps[0].Channels, or every channel ("*") if
// steps is empty (should not happen for a validated config, but a resolved
// policy with zero steps must still not silently deliver nowhere in a way
// that looks like a bug rather than a deliberate empty policy).
func firstStepChannels(steps []core.PolicyStep) []string {
	if len(steps) == 0 {
		return nil
	}
	return steps[0].Channels
}
