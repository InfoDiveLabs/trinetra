// Package trinetra: fleet_routing.go is the master's routing/escalation config store:
// validated, versioned persistence of AlertingConfig to alerting.json, and resolveRoute.
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

// defaultPolicyName is the name of the built-in policy used when alerting.json has never
// been saved (or was saved as a totally empty config).
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

// defaultAlertingConfig is what FleetAPI.Alerting reports, and what resolveRoute falls back
// to, when the store has no saved config (Version 0, never persisted).
func defaultAlertingConfig() core.AlertingConfig {
	return core.AlertingConfig{
		Version:       0,
		Policies:      []core.Policy{builtinDefaultPolicy()},
		DefaultPolicy: defaultPolicyName,
	}
}

// --- validation ----------------------------------------------------------

// validGlob reports whether pattern is a syntactically valid path.Match glob, checked
// against "".
func validGlob(pattern string) bool {
	_, err := path.Match(pattern, "")
	return err == nil
}

// validDuration reports whether s parses with time.ParseDuration.
func validDuration(s string) bool {
	_, err := time.ParseDuration(s)
	return err == nil
}

// validateAlertingConfig checks cfg atomically: every error it can find is worth reporting,
// but the first one found is returned.
func validateAlertingConfig(cfg core.AlertingConfig, validChannel func(name string) bool) error {
	// A totally empty ROUTING config (no routes, no policies, no default) is the explicit
	// "reset to the built-in default" case -- always valid.
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
		// Route names are REQUIRED and unique: the web editor's inline field-error matching needs
		// a stable, unambiguous key per row.
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

// validGroupByField reports whether g is one of the incident-grouping GroupBy fields a
// route may list: "node", "rule", "severity", or "tag:<key>" for any non-empty key.
func validGroupByField(g string) bool {
	switch g {
	case "node", "rule", "severity":
		return true
	}
	tag, ok := strings.CutPrefix(g, "tag:")
	return ok && tag != ""
}

// --- store -----------------------------------------------------------------

// alertingFileV1 is alerting.json's on-disk shape -- identical to core.AlertingConfig.
type alertingFileV1 = core.AlertingConfig

// alertingStore is the master's routing/escalation config store, persisted atomically (temp
// + rename + fsync, via writeFileAtomicSynced) to one JSON file, 0600.
type alertingStore struct {
	path string

	mu  sync.Mutex
	cfg core.AlertingConfig // Version 0 and unset (zero value) until first loaded/saved
	set bool                // true once cfg has been loaded from disk or saved at least once
}

// loadAlertingStore opens (or creates) the store at path.
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

// Get returns the current config: defaultAlertingConfig() if nothing has ever been saved.
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

// Set validates cfg (atomically: nothing is saved on any error) and, if cfg.Version is 0
// (unconditional) or matches the currently stored version.
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

// routeResolution is resolveRoute's result: which route matched (if any) and EVERY policy
// that applies.
type routeResolution struct {
	Route    string
	Policies []core.Policy
	// GroupBy is the FIRST matched route's own GroupBy, exactly mirroring how Route itself is
	// only ever set from the first match: an empty GroupBy.
	GroupBy []string
}

// resolveRoute evaluates cfg's routes against an alert on the given node (id/display
// name/tags, "" / nil for a master-own alert) with the given rule.
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

// unionStepChannels returns the deduplicated, order-preserving union of step-`step`'s
// Channels across every policy that has that many steps.
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
