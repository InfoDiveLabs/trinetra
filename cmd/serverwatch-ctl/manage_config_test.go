package main

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// TestSettingsGroupsFromCatalog asserts settingsGroups returns the distinct
// groups from config.Keys(), in the catalog's own first-seen order, with no
// duplicates -- the "all settings" screen's top level group list.
func TestSettingsGroupsFromCatalog(t *testing.T) {
	groups := settingsGroups()
	if len(groups) == 0 {
		t.Fatal("settingsGroups() returned none")
	}
	seen := map[string]bool{}
	for _, g := range groups {
		if seen[g] {
			t.Errorf("settingsGroups() lists %q more than once", g)
		}
		seen[g] = true
	}
	// Every group named in the catalog must appear exactly once here.
	for _, ki := range config.Keys() {
		if !seen[ki.Group] {
			t.Errorf("settingsGroups() is missing group %q (from key %q)", ki.Group, ki.Name)
		}
	}
	// "Intervals" is the catalog's first group (config.go's keyCatalog
	// starts with sample_interval/fast_interval/heartbeat_interval); pin
	// the ordering guarantee so a screen can rely on catalog order rather
	// than resorting.
	if groups[0] != "Intervals" {
		t.Errorf("settingsGroups()[0] = %q, want Intervals (catalog order)", groups[0])
	}
}

// TestSettingsGroupKeysFiltersByGroup asserts settingsGroupKeys returns
// exactly the catalog entries for the requested group, in catalog order.
func TestSettingsGroupKeysFiltersByGroup(t *testing.T) {
	keys := settingsGroupKeys("Intervals")
	want := []string{"sample_interval", "fast_interval", "heartbeat_interval", "exec_timeout"}
	if len(keys) != len(want) {
		t.Fatalf("settingsGroupKeys(Intervals) = %d keys, want %d", len(keys), len(want))
	}
	for i, ki := range keys {
		if ki.Name != want[i] {
			t.Errorf("settingsGroupKeys(Intervals)[%d] = %q, want %q", i, ki.Name, want[i])
		}
		if ki.Group != "Intervals" {
			t.Errorf("settingsGroupKeys(Intervals)[%d].Group = %q, want Intervals", i, ki.Group)
		}
	}
}

// TestSettingsGroupKeysUnknownGroupIsEmpty asserts an unrecognized group
// name returns no rows rather than panicking or returning the full catalog.
func TestSettingsGroupKeysUnknownGroupIsEmpty(t *testing.T) {
	if keys := settingsGroupKeys("NoSuchGroup"); len(keys) != 0 {
		t.Errorf("settingsGroupKeys(NoSuchGroup) = %v, want empty", keys)
	}
}

// TestApplyConfigKeySetsValidValue asserts applyConfigKey sets exactly the
// named key via config.Set, the same validated setter every other manage
// screen ultimately uses.
func TestApplyConfigKeySetsValidValue(t *testing.T) {
	cfg := &config.Config{}
	if err := applyConfigKey(cfg, "sample_interval", "30"); err != nil {
		t.Fatalf("applyConfigKey() error = %v, want nil", err)
	}
	if cfg.SampleInterval != 30 {
		t.Errorf("SampleInterval = %d, want 30", cfg.SampleInterval)
	}
}

// TestApplyConfigKeyRejectsInvalidValue asserts an invalid value surfaces
// config.Set's own validation error and leaves cfg unmutated.
func TestApplyConfigKeyRejectsInvalidValue(t *testing.T) {
	cfg := &config.Config{}
	err := applyConfigKey(cfg, "sample_interval", "not-a-number")
	if err == nil {
		t.Fatal("applyConfigKey() error = nil, want a validation error")
	}
	if cfg.SampleInterval != 0 {
		t.Errorf("SampleInterval = %d, want left at 0 on a rejected value", cfg.SampleInterval)
	}
}

// TestApplyConfigKeyUnknownKeyErrors asserts a key outside the Set switch
// (and so outside the catalog) errors rather than silently no-op-ing.
func TestApplyConfigKeyUnknownKeyErrors(t *testing.T) {
	cfg := &config.Config{}
	if err := applyConfigKey(cfg, "bogus_key", "x"); err == nil {
		t.Fatal("applyConfigKey() error = nil, want an unknown-key error")
	}
}

// --- Bubble Tea glue: group list -> key list -> value input ---

// openSettingsGroupList drives 'm', moves the menu cursor down to "all
// settings" (the last row manageItems appends), opens it, and runs the
// resulting fetchSettingsConfigCmd through to completion -- the pre-fill
// round trip every screen does on open, mirroring manage_ui_test.go's
// openManageScreen for the schedule/quiet-hours/healthchecks screens (which
// this screen does not share state with, since it uses its own mgr.set*
// fields rather than mgr.configLoading/mgr.valueIn).
func openSettingsGroupList(t *testing.T, api core.API) tea.Model {
	t.Helper()
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(keyRunes('m'))
	for i := 0; i < len(manageItems)-1; i++ {
		mm, _ = mm.Update(keyType(tea.KeyDown))
	}
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	if got := mm.(model).mgr.screen; got != manageSettingsGroups {
		t.Fatalf("mgr.screen = %v, want manageSettingsGroups", got)
	}
	if cmd == nil {
		t.Fatal("expected fetchSettingsConfigCmd on opening the settings screen, got nil")
	}
	msg := runCmd(t, cmd)
	mm, _ = mm.Update(msg)
	return mm
}

// TestManageSettingsMenuEntryOpensGroupList asserts "all settings" is the
// last row of the management menu and opening it lands on the group list,
// populated from settingsGroups().
func TestManageSettingsMenuEntryOpensGroupList(t *testing.T) {
	mm := openSettingsGroupList(t, &fakeAPI{cfg: config.Default()})
	got := mm.(model)
	if got.mgr.setLoading {
		t.Fatal("setLoading should be false once the config msg lands")
	}
	if len(got.mgr.setGroups) == 0 {
		t.Fatal("mgr.setGroups is empty, want the settingsGroups() catalog")
	}
	if got.mgr.setGroups[0] != "Intervals" {
		t.Errorf("mgr.setGroups[0] = %q, want Intervals", got.mgr.setGroups[0])
	}
}

// TestManageSettingsGroupSelectOpensKeyListWithCurrentValues asserts
// selecting a group opens its key list, and that the list can report each
// key's CURRENT value via the freshly fetched config (mgr.setCfg.Get).
func TestManageSettingsGroupSelectOpensKeyListWithCurrentValues(t *testing.T) {
	cfg := config.Default()
	cfg.FastInterval = 7
	mm := openSettingsGroupList(t, &fakeAPI{cfg: cfg})

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select "Intervals" (first group)
	got := mm.(model)
	if got.mgr.screen != manageSettingsKeys {
		t.Fatalf("mgr.screen = %v, want manageSettingsKeys", got.mgr.screen)
	}
	if len(got.mgr.setKeys) != 4 {
		t.Fatalf("mgr.setKeys = %d entries, want 4 (Intervals group)", len(got.mgr.setKeys))
	}
	if got.mgr.setKeys[1].Name != "fast_interval" {
		t.Fatalf("mgr.setKeys[1].Name = %q, want fast_interval", got.mgr.setKeys[1].Name)
	}
	val, ok := got.mgr.setCfg.Get("fast_interval")
	if !ok || val != "7" {
		t.Errorf("mgr.setCfg.Get(fast_interval) = %q, %v, want 7, true", val, ok)
	}
}

// TestManageSettingsKeySelectPrefillsCurrentValue asserts selecting a key
// opens the value input pre-filled with its current value, not blank.
func TestManageSettingsKeySelectPrefillsCurrentValue(t *testing.T) {
	cfg := config.Default()
	cfg.SampleInterval = 45
	cfg.FastInterval = 9 // keep sample_interval (45) a multiple of fast_interval
	mm := openSettingsGroupList(t, &fakeAPI{cfg: cfg})

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select "Intervals"
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select "sample_interval" (first key)
	got := mm.(model)
	if got.mgr.screen != manageSettingsValue {
		t.Fatalf("mgr.screen = %v, want manageSettingsValue", got.mgr.screen)
	}
	if got.mgr.setValueIn.Value() != "45" {
		t.Errorf("setValueIn = %q, want prefilled with the current value 45", got.mgr.setValueIn.Value())
	}
}

// TestManageSettingsValueApplyValidChangesOnlyThatKey drives a full edit
// (group -> key -> new value -> enter) and asserts ApplyConfig received a
// config with EXACTLY that key changed and nothing else -- the fetch-fresh/
// mutate-one-key/apply shape every other manage screen uses.
func TestManageSettingsValueApplyValidChangesOnlyThatKey(t *testing.T) {
	cfg := config.Default()
	api := &fakeAPI{cfg: cfg}
	mm := openSettingsGroupList(t, api)

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // "Intervals"
	mm, _ = mm.Update(keyType(tea.KeyDown))  // -> fast_interval
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select fast_interval
	if mm.(model).mgr.setValueIn.Value() != "5" {
		t.Fatalf("setValueIn = %q, want prefilled with the default fast_interval 5", mm.(model).mgr.setValueIn.Value())
	}
	// Replace the prefilled value with a new one that still divides
	// sample_interval (60): a real user backspaces it out first.
	mo := mm.(model)
	mo.mgr.setValueIn.SetValue("10")
	mo.mgr.setValueIn.CursorEnd()
	mm = mo

	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("expected applyConfigKeyCmd, got nil")
	}
	msg := runCmd(t, cmd)
	applied, ok := msg.(settingsAppliedMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want settingsAppliedMsg", msg)
	}
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applyN != 1 {
		t.Fatalf("ApplyConfig called %d times, want 1", api.applyN)
	}
	if api.applied.FastInterval != 10 {
		t.Errorf("applied.FastInterval = %d, want 10", api.applied.FastInterval)
	}
	// Nothing else should have moved: compare every other field against
	// the original default by round-tripping through the Get/Set-visible
	// keys, using SampleInterval as the representative untouched sibling.
	if api.applied.SampleInterval != cfg.SampleInterval {
		t.Errorf("applied.SampleInterval = %d, want unchanged %d", api.applied.SampleInterval, cfg.SampleInterval)
	}
	if api.applied.HeartbeatInterval != cfg.HeartbeatInterval {
		t.Errorf("applied.HeartbeatInterval = %d, want unchanged %d", api.applied.HeartbeatInterval, cfg.HeartbeatInterval)
	}

	mm, _ = mm.Update(applied)
	if mm.(model).mgr.screen != manageResult {
		t.Fatalf("mgr.screen = %v, want manageResult", mm.(model).mgr.screen)
	}
	if mm.(model).mgr.applyErr != nil {
		t.Errorf("mgr.applyErr = %v, want nil", mm.(model).mgr.applyErr)
	}
}

// TestManageSettingsValueApplyInvalidDoesNotCallApplyConfig asserts a value
// config.Set rejects surfaces the error on the result screen and never
// reaches ApplyConfig, so nothing invalid is ever persisted.
func TestManageSettingsValueApplyInvalidDoesNotCallApplyConfig(t *testing.T) {
	api := &fakeAPI{cfg: config.Default()}
	mm := openSettingsGroupList(t, api)

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // "Intervals"
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select sample_interval
	mo := mm.(model)
	mo.mgr.setValueIn.SetValue("not-a-number")
	mo.mgr.setValueIn.CursorEnd()
	mm = mo

	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	msg := runCmd(t, cmd)
	applied := msg.(settingsAppliedMsg)
	if applied.err == nil {
		t.Fatal("apply err = nil, want a validation error")
	}
	if api.applyN != 0 {
		t.Errorf("ApplyConfig called %d times, want 0 (invalid value must not be persisted)", api.applyN)
	}

	mm, _ = mm.Update(applied)
	got := mm.(model)
	if got.mgr.screen != manageResult {
		t.Fatalf("mgr.screen = %v, want manageResult", got.mgr.screen)
	}
	if got.mgr.applyErr == nil {
		t.Error("mgr.applyErr = nil, want the rejected value's error surfaced")
	}
}

// TestManageSettingsRestartRequiredCaveatShown asserts a RestartRequired
// key (storage.backend) shows the restart caveat on its value screen, the
// same style the guided web-setup wizard already uses for its own
// restart-required keys.
func TestManageSettingsRestartRequiredCaveatShown(t *testing.T) {
	api := &fakeAPI{cfg: config.Default()}
	mm := openSettingsGroupList(t, api)

	// "Storage" is not the first group; walk down the group list until the
	// cursor lands on it rather than hard-coding an index.
	for mm.(model).mgr.setGroups[mm.(model).mgr.setGroupCur] != "Storage" {
		mm, _ = mm.Update(keyType(tea.KeyDown))
	}
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // open "Storage"
	got := mm.(model)
	if got.mgr.setKeys[0].Name != "storage.backend" {
		t.Fatalf("mgr.setKeys[0].Name = %q, want storage.backend", got.mgr.setKeys[0].Name)
	}
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select storage.backend

	view := mm.(model).manageView()
	if !containsFold(view, "restart") {
		t.Errorf("value screen for a RestartRequired key should mention a restart; view =\n%s", view)
	}
}

// TestManageSettingsGroupFetchErrorIsGraceful asserts a failing Config()
// fetch on screen-open surfaces mgr.setErr rather than crashing or showing
// a stale/blank screen, mirroring TestManageConfigFetchErrorIsGraceful for
// the schedule/quiet-hours/healthchecks screens.
func TestManageSettingsGroupFetchErrorIsGraceful(t *testing.T) {
	wantErr := errors.New("config fetch failed")
	api := &fakeAPI{cfg: config.Default(), configErr: wantErr}
	mm := openSettingsGroupList(t, api)
	got := mm.(model)
	if got.mgr.setErr == nil || got.mgr.setErr.Error() != wantErr.Error() {
		t.Fatalf("mgr.setErr = %v, want %v", got.mgr.setErr, wantErr)
	}
}

// containsFold is a tiny case-insensitive substring check, avoiding a
// strings.ToLower allocation dance inline at each call site above.
func containsFold(s, substr string) bool {
	return len(s) >= len(substr) && indexFold(s, substr) >= 0
}

func indexFold(s, substr string) int {
	sl, subl := len(s), len(substr)
	if subl == 0 {
		return 0
	}
	for i := 0; i+subl <= sl; i++ {
		if eqFold(s[i:i+subl], substr) {
			return i
		}
	}
	return -1
}

func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
