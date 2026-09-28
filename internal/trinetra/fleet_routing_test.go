package trinetra

import (
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func allChannelsValid(string) bool { return true }

func noChannelsValid(string) bool { return false }

// --- resolveRoute --------------------------------------------------------

func TestResolveRouteDefaultConfigMatchesTodaysBehaviour(t *testing.T) {
	res := resolveRoute(defaultAlertingConfig(), "n1", "web1", []string{"web"}, "cpu", "critical")
	if res.Route != "" {
		t.Fatalf("Route = %q, want empty (no routes at all)", res.Route)
	}
	if len(res.Policies) != 1 || res.Policies[0].Name != "default" {
		t.Fatalf("Policies = %+v, want exactly [default]", res.Policies)
	}
	steps := res.Policies[0].Steps
	if len(steps) != 1 || steps[0].After != "0s" || len(steps[0].Channels) != 1 || steps[0].Channels[0] != "*" {
		t.Fatalf("Steps = %+v, want a single immediate step to every channel", steps)
	}
	if !sendResolvedOf(res.Policies[0]) {
		t.Fatal("SendResolved = false, want true by default")
	}
}

func TestResolveRouteFirstMatchWins(t *testing.T) {
	cfg := core.AlertingConfig{
		Routes: []core.Route{
			{Name: "web-cpu", Matchers: []core.Matcher{{Node: "web*", Rule: "cpu*"}}, Policy: "quiet"},
			{Name: "catch-all", Matchers: []core.Matcher{{Rule: "*"}}, Policy: "loud"},
		},
		Policies: []core.Policy{
			{Name: "quiet", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}}},
			{Name: "loud", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"pager"}}}},
		},
		DefaultPolicy: "loud",
	}
	res := resolveRoute(cfg, "n1", "web1", nil, "cpu_pct", "critical")
	if res.Route != "web-cpu" || len(res.Policies) != 1 || res.Policies[0].Name != "quiet" {
		t.Fatalf("got route=%q policies=%+v, want the FIRST matching route (web-cpu/quiet) only", res.Route, res.Policies)
	}
	if res.Policies[0].Steps[0].Channels[0] != "slack" {
		t.Fatalf("Steps = %+v, want quiet's own step", res.Policies[0].Steps)
	}
}

func TestResolveRouteNoMatchUsesDefaultPolicy(t *testing.T) {
	cfg := core.AlertingConfig{
		Routes: []core.Route{
			{Name: "web-only", Matchers: []core.Matcher{{Node: "web*"}}, Policy: "quiet"},
		},
		Policies: []core.Policy{
			{Name: "quiet", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}}},
			{Name: "loud", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"pager"}}}},
		},
		DefaultPolicy: "loud",
	}
	res := resolveRoute(cfg, "n1", "db1", nil, "cpu_pct", "critical")
	if res.Route != "" {
		t.Fatalf("Route = %q, want empty (nothing matched)", res.Route)
	}
	if len(res.Policies) != 1 || res.Policies[0].Name != "loud" {
		t.Fatalf("Policies = %+v, want the DefaultPolicy %q only", res.Policies, "loud")
	}
}

// TestResolveRouteContinueChainsKeepsEachPolicySeparate covers the B5 fix
// round 1 ruling: Continue:true keeps evaluating later routes too, and every
// matched route's policy is returned SEPARATELY (never merged) -- each one
// escalates independently.
func TestResolveRouteContinueChainsKeepsEachPolicySeparate(t *testing.T) {
	cfg := core.AlertingConfig{
		Routes: []core.Route{
			{Name: "first", Matchers: []core.Matcher{{Rule: "cpu*"}}, Policy: "slack-only", Continue: true},
			{Name: "second", Matchers: []core.Matcher{{Rule: "*"}}, Policy: "pager-only"},
		},
		Policies: []core.Policy{
			{Name: "slack-only", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}}},
			{Name: "pager-only", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"pager"}}}},
		},
		DefaultPolicy: "slack-only",
	}
	res := resolveRoute(cfg, "n1", "web1", nil, "cpu_pct", "critical")
	if res.Route != "first" {
		t.Fatalf("Route = %q, want the FIRST matched route's name", res.Route)
	}
	if len(res.Policies) != 2 || res.Policies[0].Name != "slack-only" || res.Policies[1].Name != "pager-only" {
		t.Fatalf("Policies = %+v, want [slack-only pager-only], each kept distinct", res.Policies)
	}
}

func TestResolveRouteWithoutContinueStopsAtFirstMatch(t *testing.T) {
	cfg := core.AlertingConfig{
		Routes: []core.Route{
			{Name: "first", Matchers: []core.Matcher{{Rule: "cpu*"}}, Policy: "slack-only"},
			{Name: "second", Matchers: []core.Matcher{{Rule: "*"}}, Policy: "pager-only"},
		},
		Policies: []core.Policy{
			{Name: "slack-only", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}}},
			{Name: "pager-only", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"pager"}}}},
		},
		DefaultPolicy: "slack-only",
	}
	res := resolveRoute(cfg, "n1", "web1", nil, "cpu_pct", "critical")
	if len(res.Policies) != 1 || res.Policies[0].Name != "slack-only" {
		t.Fatalf("Policies = %+v, want ONLY the first matched route's policy (no continue)", res.Policies)
	}
}

// TestUnionStepChannelsAcrossPolicies covers the B5 fix round 1 ruling for
// the fire leg's single physical dispatch: step 0's channels are the union
// across every matched policy that has that many steps.
func TestUnionStepChannelsAcrossPolicies(t *testing.T) {
	policies := []core.Policy{
		{Name: "a", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}, {After: "5m", Channels: []string{"pager"}}}},
		{Name: "b", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"email", "slack"}}}},
	}
	got := unionStepChannels(policies, 0)
	set := map[string]bool{}
	for _, c := range got {
		set[c] = true
	}
	if len(set) != 2 || !set["slack"] || !set["email"] {
		t.Fatalf("unionStepChannels(step 0) = %v, want deduped [slack email]", got)
	}
	if got := unionStepChannels(policies, 1); len(got) != 1 || got[0] != "pager" {
		t.Fatalf("unionStepChannels(step 1) = %v, want [pager] (only policy a has a step 1)", got)
	}
}

// --- validateAlertingConfig ------------------------------------------------

func TestValidateAlertingConfigEmptyIsValid(t *testing.T) {
	if err := validateAlertingConfig(core.AlertingConfig{}, noChannelsValid); err != nil {
		t.Fatalf("an entirely empty config (reset to built-in default) must be valid: %v", err)
	}
}

func TestValidateAlertingConfigUnknownChannel(t *testing.T) {
	cfg := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"bogus"}}}}},
		DefaultPolicy: "p",
	}
	err := validateAlertingConfig(cfg, noChannelsValid)
	if err == nil {
		t.Fatal("want an error for an unknown channel")
	}
}

func TestValidateAlertingConfigWildcardChannelAlwaysValid(t *testing.T) {
	cfg := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
		DefaultPolicy: "p",
	}
	if err := validateAlertingConfig(cfg, noChannelsValid); err != nil {
		t.Fatalf("the literal \"*\" must always be valid: %v", err)
	}
}

func TestValidateAlertingConfigUnknownDefaultPolicy(t *testing.T) {
	cfg := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
		DefaultPolicy: "does-not-exist",
	}
	if err := validateAlertingConfig(cfg, allChannelsValid); err == nil {
		t.Fatal("want an error for an unknown default_policy")
	}
}

func TestValidateAlertingConfigUnknownRoutePolicy(t *testing.T) {
	cfg := core.AlertingConfig{
		Routes:        []core.Route{{Name: "r", Matchers: []core.Matcher{{Rule: "*"}}, Policy: "does-not-exist"}},
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
		DefaultPolicy: "p",
	}
	if err := validateAlertingConfig(cfg, allChannelsValid); err == nil {
		t.Fatal("want an error for a route naming an unknown policy")
	}
}

func TestValidateAlertingConfigBadDuration(t *testing.T) {
	cfg := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "not-a-duration", Channels: []string{"*"}}}}},
		DefaultPolicy: "p",
	}
	if err := validateAlertingConfig(cfg, allChannelsValid); err == nil {
		t.Fatal("want an error for a bad step duration")
	}

	cfg2 := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}, RepeatEvery: "nope"}},
		DefaultPolicy: "p",
	}
	if err := validateAlertingConfig(cfg2, allChannelsValid); err == nil {
		t.Fatal("want an error for a bad repeat_every duration")
	}
}

func TestValidateAlertingConfigBadGlob(t *testing.T) {
	cfg := core.AlertingConfig{
		Routes:        []core.Route{{Name: "r", Matchers: []core.Matcher{{Node: "["}}, Policy: "p"}},
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
		DefaultPolicy: "p",
	}
	if err := validateAlertingConfig(cfg, allChannelsValid); err == nil {
		t.Fatal("want an error for a bad node glob")
	}
}

func TestValidateAlertingConfigDuplicateNames(t *testing.T) {
	cfg := core.AlertingConfig{
		Policies: []core.Policy{
			{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}},
			{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}},
		},
		DefaultPolicy: "p",
	}
	if err := validateAlertingConfig(cfg, allChannelsValid); err == nil {
		t.Fatal("want an error for duplicate policy names")
	}

	cfg2 := core.AlertingConfig{
		Routes: []core.Route{
			{Name: "r", Matchers: []core.Matcher{{Rule: "a*"}}, Policy: "p"},
			{Name: "r", Matchers: []core.Matcher{{Rule: "b*"}}, Policy: "p"},
		},
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
		DefaultPolicy: "p",
	}
	if err := validateAlertingConfig(cfg2, allChannelsValid); err == nil {
		t.Fatal("want an error for duplicate route names")
	}
}

// TestValidateAlertingConfigRouteNameRequired pins the C3 fix round 1
// ruling: a blank route name is rejected outright ("every route needs a
// name"), removing the old "(unnamed)" ambiguity the web editor's inline
// field-error matching used to have to live with.
func TestValidateAlertingConfigRouteNameRequired(t *testing.T) {
	cfg := core.AlertingConfig{
		Routes:        []core.Route{{Matchers: []core.Matcher{{Rule: "*"}}, Policy: "p"}},
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
		DefaultPolicy: "p",
	}
	err := validateAlertingConfig(cfg, allChannelsValid)
	if err == nil || err.Error() != "every route needs a name" {
		t.Fatalf("err = %v, want %q", err, "every route needs a name")
	}
}

// TestValidateAlertingConfigRejectsZeroStepPolicy is the B5 fix round 1
// minor: a policy with no steps at all is rejected (it would silently
// deliver nowhere, forever, for every incident routed to it).
func TestValidateAlertingConfigRejectsZeroStepPolicy(t *testing.T) {
	cfg := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: nil}},
		DefaultPolicy: "p",
	}
	if err := validateAlertingConfig(cfg, allChannelsValid); err == nil {
		t.Fatal("want an error for a policy with zero steps")
	}
}

// --- alertingStore ---------------------------------------------------------

func TestAlertingStoreGetDefaultsWhenNeverSaved(t *testing.T) {
	dir := t.TempDir()
	s, err := loadAlertingStore(filepath.Join(dir, "alerting.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.Get()
	if cfg.Version != 0 || cfg.DefaultPolicy != "default" || len(cfg.Policies) != 1 {
		t.Fatalf("Get() = %+v, want the built-in default (Version 0)", cfg)
	}
}

func TestAlertingStoreSetIsAtomicOnValidationError(t *testing.T) {
	dir := t.TempDir()
	s, err := loadAlertingStore(filepath.Join(dir, "alerting.json"))
	if err != nil {
		t.Fatal(err)
	}
	bad := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"bogus"}}}}},
		DefaultPolicy: "p",
	}
	if _, err := s.Set(bad, noChannelsValid); err == nil {
		t.Fatal("want a validation error")
	}
	// Nothing was saved: Get() still reports the built-in default.
	if got := s.Get(); got.Version != 0 {
		t.Fatalf("Get() after a rejected Set = %+v, want the config unchanged", got)
	}
}

func TestAlertingStoreVersioning(t *testing.T) {
	dir := t.TempDir()
	s, err := loadAlertingStore(filepath.Join(dir, "alerting.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
		DefaultPolicy: "p",
	}

	// Version 0 is unconditional and succeeds from a fresh store.
	saved, err := s.Set(cfg, allChannelsValid)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 1 {
		t.Fatalf("Version after first save = %d, want 1", saved.Version)
	}

	// A stale (non-zero, non-matching) Version is rejected.
	stale := cfg
	stale.Version = 1
	if _, err := s.Set(stale, allChannelsValid); err != nil {
		t.Fatalf("Set with the CURRENT version should succeed: %v", err)
	}
	if got := s.Get(); got.Version != 2 {
		t.Fatalf("Version after second save = %d, want 2", got.Version)
	}
	stale.Version = 1 // now stale again, since the store is at 2
	if _, err := s.Set(stale, allChannelsValid); err != core.ErrConflict {
		t.Fatalf("Set with a stale version = %v, want core.ErrConflict", err)
	}

	// Version 0 remains unconditional even once the store has a real version.
	cfg.Version = 0
	if _, err := s.Set(cfg, allChannelsValid); err != nil {
		t.Fatalf("Set with Version 0 must always succeed (CLI apply): %v", err)
	}

	// Persisted across a reload.
	s2, err := loadAlertingStore(filepath.Join(dir, "alerting.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get(); got.Version != 3 || len(got.Policies) != 1 {
		t.Fatalf("reloaded Get() = %+v, want the persisted config", got)
	}
}
