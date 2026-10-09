package main

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// TestHomeKeyMOpensManageMenu asserts 'm' on Home enters the management
// menu on its top level list screen.
func TestHomeKeyMOpensManageMenu(t *testing.T) {
	m := newModel(&fakeAPI{})
	var mm tea.Model = m
	mm, _ = mm.Update(keyRunes('m'))
	got := mm.(model)
	if got.step != stepManage {
		t.Fatalf("step = %v, want stepManage", got.step)
	}
	if got.mgr.screen != manageMenuList {
		t.Fatalf("mgr.screen = %v, want manageMenuList", got.mgr.screen)
	}
}

// TestManageMenuEscReturnsHome asserts esc on the top level management menu
// backs all the way out to Home (not just one screen back).
func TestManageMenuEscReturnsHome(t *testing.T) {
	var mm tea.Model = newModel(&fakeAPI{})
	mm, _ = mm.Update(keyRunes('m'))
	mm, cmd := mm.Update(keyType(tea.KeyEsc))
	if cmd != nil {
		t.Error("esc on the menu should not issue a command")
	}
	if mm.(model).step != stepHome {
		t.Errorf("step = %v, want stepHome", mm.(model).step)
	}
}

// openManageScreen drives 'm' plus downCount "down" presses plus enter.
func openManageScreen(t *testing.T, api core.API, downCount int) tea.Model {
	t.Helper()
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(keyRunes('m'))
	for i := 0; i < downCount; i++ {
		mm, _ = mm.Update(keyType(tea.KeyDown))
	}
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("expected a fetch*ConfigCmd on opening the screen, got nil")
	}
	if !mm.(model).mgr.configLoading {
		t.Fatal("mgr.configLoading = false, want true right after opening the screen")
	}
	msg := runCmd(t, cmd)
	mm, _ = mm.Update(msg)
	if mm.(model).mgr.configLoading {
		t.Fatal("mgr.configLoading should be false once the config msg lands")
	}
	return mm
}

// TestManageScheduleDefaultsCursorToCurrentMode asserts opening the Schedule screen
// positions the mode cursor on whatever schedule.daily/ schedule.weekly is ACTUALLY active.
func TestManageScheduleDefaultsCursorToCurrentMode(t *testing.T) {
	cfg := &config.Config{}
	cfg.Schedule.Daily = "03:30"
	mm := openManageScreen(t, &fakeAPI{cfg: cfg}, 0) // menu cursor 0 = "schedule"

	got := mm.(model)
	if got.mgr.screen != manageScheduleMode {
		t.Fatalf("mgr.screen = %v, want manageScheduleMode", got.mgr.screen)
	}
	if got.mgr.schedModeCursor != 1 {
		t.Fatalf("schedModeCursor = %d, want 1 (daily)", got.mgr.schedModeCursor)
	}
	if got.mgr.schedAns.Daily != "03:30" {
		t.Errorf("schedAns.Daily = %q, want 03:30 (pre-filled from the current config)", got.mgr.schedAns.Daily)
	}
}

// TestManageScheduleDefaultsCursorToWeeklyMode is
// TestManageScheduleDefaultsCursorToCurrentMode's weekly counterpart.
func TestManageScheduleDefaultsCursorToWeeklyMode(t *testing.T) {
	cfg := &config.Config{}
	cfg.Schedule.Weekly = "mon@09:00"
	mm := openManageScreen(t, &fakeAPI{cfg: cfg}, 0)

	got := mm.(model)
	if got.mgr.schedModeCursor != 2 {
		t.Fatalf("schedModeCursor = %d, want 2 (weekly)", got.mgr.schedModeCursor)
	}
	if got.mgr.schedAns.Weekly != "mon@09:00" {
		t.Errorf("schedAns.Weekly = %q, want mon@09:00", got.mgr.schedAns.Weekly)
	}

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept the pre-selected "weekly" mode
	if mm.(model).mgr.valueIn.Value() != "mon@09:00" {
		t.Errorf("valueIn = %q, want pre-filled with mon@09:00", mm.(model).mgr.valueIn.Value())
	}
}

// TestManageScheduleStrayEnterKeepsCurrentDailyValue pins that with schedule.daily already
// set, opening the Schedule screen and pressing Enter twice.
func TestManageScheduleStrayEnterKeepsCurrentDailyValue(t *testing.T) {
	cfg := &config.Config{}
	cfg.Schedule.Daily = "03:30"
	api := &fakeAPI{cfg: cfg}
	mm := openManageScreen(t, api, 0)

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept the pre-selected "daily" mode
	got := mm.(model)
	if got.mgr.screen != manageScheduleValue {
		t.Fatalf("mgr.screen = %v, want manageScheduleValue", got.mgr.screen)
	}
	if got.mgr.valueIn.Value() != "03:30" {
		t.Fatalf("valueIn = %q, want pre-filled with the current daily time 03:30", got.mgr.valueIn.Value())
	}

	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // accept the pre-filled value
	msg := runCmd(t, cmd)
	applied := msg.(manageAppliedMsg)
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied.Schedule.Daily != "03:30" {
		t.Errorf("applied schedule.daily = %q, want 03:30 (unchanged, not wiped)", api.applied.Schedule.Daily)
	}
}

// TestManageScheduleOffWhenNothingConfigured asserts the off path still works, and is
// harmless.
func TestManageScheduleOffWhenNothingConfigured(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	mm := openManageScreen(t, api, 0)
	if mm.(model).mgr.schedModeCursor != 0 {
		t.Fatalf("schedModeCursor = %d, want 0 (off, nothing configured)", mm.(model).mgr.schedModeCursor)
	}

	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // accept "off"
	if !mm.(model).mgr.applying {
		t.Fatal("mgr.applying = false, want true")
	}
	msg := runCmd(t, cmd)
	applied := msg.(manageAppliedMsg)
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied.Schedule.Daily != "" || api.applied.Schedule.Weekly != "" {
		t.Errorf("applied schedule = %+v, want both cleared", api.applied.Schedule)
	}
}

// TestManageScheduleDailySetsValue drives the daily path when nothing was previously
// configured.
func TestManageScheduleDailySetsValue(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	mm := openManageScreen(t, api, 0)

	mm, _ = mm.Update(keyType(tea.KeyDown))  // mode cursor -> "daily"
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select "daily"
	got := mm.(model)
	if got.mgr.screen != manageScheduleValue {
		t.Fatalf("mgr.screen = %v, want manageScheduleValue", got.mgr.screen)
	}
	if got.mgr.valueIn.Value() != "" {
		t.Fatalf("valueIn = %q, want blank (nothing previously configured)", got.mgr.valueIn.Value())
	}

	mm = typeString(t, mm, "03:30")
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	msg := runCmd(t, cmd)
	applied := msg.(manageAppliedMsg)
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied.Schedule.Daily != "03:30" {
		t.Errorf("applied schedule.daily = %q, want 03:30", api.applied.Schedule.Daily)
	}
	if api.applied.Schedule.Weekly != "" {
		t.Errorf("applied schedule.weekly = %q, want cleared", api.applied.Schedule.Weekly)
	}
}

// TestManageQuietHoursPrefillsCurrentValue asserts opening the Quiet hours screen pre-fills
// valueIn with the CURRENT quiet_hours, and that a stray Enter.
func TestManageQuietHoursPrefillsCurrentValue(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{QuietHours: "22-6"}}
	mm := openManageScreen(t, api, 1) // menu cursor 1 = "quiet hours"

	got := mm.(model)
	if got.mgr.screen != manageQuietValue {
		t.Fatalf("mgr.screen = %v, want manageQuietValue", got.mgr.screen)
	}
	if got.mgr.valueIn.Value() != "22-6" {
		t.Fatalf("valueIn = %q, want pre-filled with the current quiet_hours 22-6", got.mgr.valueIn.Value())
	}

	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // stray enter: no typing
	msg := runCmd(t, cmd)
	applied := msg.(manageAppliedMsg)
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied.QuietHours != "22-6" {
		t.Errorf("applied QuietHours = %q, want 22-6 (unchanged, not wiped)", api.applied.QuietHours)
	}
}

// TestManageQuietHoursPrefillsOffWhenUnset asserts the pre-fill falls back to the literal
// "off".
func TestManageQuietHoursPrefillsOffWhenUnset(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	mm := openManageScreen(t, api, 1)

	got := mm.(model)
	if got.mgr.valueIn.Value() != "off" {
		t.Fatalf("valueIn = %q, want \"off\" (nothing currently set)", got.mgr.valueIn.Value())
	}

	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	msg := runCmd(t, cmd)
	applied := msg.(manageAppliedMsg)
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied.QuietHours != "" {
		t.Errorf("applied QuietHours = %q, want still cleared", api.applied.QuietHours)
	}
}

// TestManageQuietHoursExplicitOffClears drives an EXPLICIT edit: the pre-filled current
// value is replaced (not just accepted) with "off", which must still clear quiet_hours.
func TestManageQuietHoursExplicitOffClears(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{QuietHours: "22-6"}}
	mm := openManageScreen(t, api, 1)

	// Replace the pre-filled "22-6" with "off": a real user would backspace it out; setting
	// the field directly is equivalent and keeps this test focused on the mutation.
	mo := mm.(model)
	mo.mgr.valueIn.SetValue("off")
	mo.mgr.valueIn.CursorEnd()
	mm = mo

	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	msg := runCmd(t, cmd)
	applied := msg.(manageAppliedMsg)
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied.QuietHours != "" {
		t.Errorf("applied QuietHours = %q, want cleared", api.applied.QuietHours)
	}
}

// TestManageHealthchecksPrefillsCurrentURL mirrors
// TestManageQuietHoursPrefillsCurrentValue for the Healthchecks screen.
func TestManageHealthchecksPrefillsCurrentURL(t *testing.T) {
	cfg := &config.Config{}
	cfg.Healthchecks.URL = "https://hc-ping.com/abc"
	api := &fakeAPI{cfg: cfg}
	mm := openManageScreen(t, api, 2) // menu cursor 2 = "healthchecks"

	got := mm.(model)
	if got.mgr.screen != manageHealthValue {
		t.Fatalf("mgr.screen = %v, want manageHealthValue", got.mgr.screen)
	}
	if got.mgr.valueIn.Value() != "https://hc-ping.com/abc" {
		t.Fatalf("valueIn = %q, want pre-filled with the current healthchecks.url", got.mgr.valueIn.Value())
	}

	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // stray enter: no typing
	msg := runCmd(t, cmd)
	applied := msg.(manageAppliedMsg)
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied.Healthchecks.URL != "https://hc-ping.com/abc" {
		t.Errorf("applied Healthchecks.URL = %q, want unchanged", api.applied.Healthchecks.URL)
	}
}

// TestManageHealthchecksSetsURL drives an explicit edit when nothing was previously
// configured: the pre-fill is "off" (blank URL).
func TestManageHealthchecksSetsURL(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	mm := openManageScreen(t, api, 2)
	if mm.(model).mgr.valueIn.Value() != "off" {
		t.Fatalf("valueIn = %q, want \"off\" (nothing currently set)", mm.(model).mgr.valueIn.Value())
	}

	mo := mm.(model)
	mo.mgr.valueIn.SetValue("https://hc-ping.com/abc")
	mo.mgr.valueIn.CursorEnd()
	mm = mo

	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	msg := runCmd(t, cmd)
	applied := msg.(manageAppliedMsg)
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied.Healthchecks.URL != "https://hc-ping.com/abc" {
		t.Errorf("applied Healthchecks.URL = %q, want https://hc-ping.com/abc", api.applied.Healthchecks.URL)
	}
}

// TestManageValueApplyErrorSurfaces asserts a failing ApplyConfig reaches manageResult's
// applyErr, mirroring TestWizardApplyErrorSurfaces for the web-setup wizard.
func TestManageValueApplyErrorSurfaces(t *testing.T) {
	wantErr := errors.New("quiet_hours rejected")
	api := &fakeAPI{cfg: &config.Config{QuietHours: "22-6"}, applyErr: wantErr}
	mm := openManageScreen(t, api, 1) // "quiet hours"

	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // stray enter re-applies the pre-filled value
	msg := runCmd(t, cmd)
	mm, _ = mm.Update(msg)

	got := mm.(model)
	if got.mgr.applyErr == nil || got.mgr.applyErr.Error() != wantErr.Error() {
		t.Fatalf("mgr.applyErr = %v, want %v", got.mgr.applyErr, wantErr)
	}
	if got.mgr.screen != manageResult {
		t.Fatalf("mgr.screen = %v, want manageResult", got.mgr.screen)
	}
}

// TestManageConfigFetchErrorIsGraceful asserts a failing Config() fetch on screen-open
// surfaces mgr.configErr.
func TestManageConfigFetchErrorIsGraceful(t *testing.T) {
	wantErr := errors.New("config fetch failed")
	api := &fakeAPI{cfg: &config.Config{}, configErr: wantErr}
	mm := openManageScreen(t, api, 1) // "quiet hours"

	got := mm.(model)
	if got.mgr.configErr == nil || got.mgr.configErr.Error() != wantErr.Error() {
		t.Fatalf("mgr.configErr = %v, want %v", got.mgr.configErr, wantErr)
	}
}

// TestManageResultAnyKeyReturnsToMenu asserts pressing a key on the result screen goes back
// to the menu list, not Home.
func TestManageResultAnyKeyReturnsToMenu(t *testing.T) {
	m := newModel(&fakeAPI{})
	m.step = stepManage
	m.mgr.screen = manageResult
	var mm tea.Model = m
	mm, _ = mm.Update(keyRunes('x'))
	if mm.(model).mgr.screen != manageMenuList {
		t.Errorf("mgr.screen = %v, want manageMenuList", mm.(model).mgr.screen)
	}
}

// TestMonitorTargetsMsgBuildsRows asserts monitorTargetsMsg (as
// discoverMonitorCmd produces) merges into mgr.monRows via buildMonitorRows.
func TestMonitorTargetsMsgBuildsRows(t *testing.T) {
	m := newModel(&fakeAPI{})
	m.step = stepManage
	m.mgr.screen = manageMonitorList
	m.mgr.monLoading = true

	cfg := &config.Config{}
	cfg.SetTarget("disk:/", false)
	targets := []core.TargetView{{ID: "disk:/", Kind: "disk", Display: "/", Available: true}}

	got, _ := m.Update(monitorTargetsMsg{targets: targets, cfg: cfg})
	gm := got.(model)
	if gm.mgr.monLoading {
		t.Error("monLoading should be false after monitorTargetsMsg")
	}
	if len(gm.mgr.monRows) != 1 {
		t.Fatalf("len(monRows) = %d, want 1", len(gm.mgr.monRows))
	}
	if gm.mgr.monRows[0].Enabled {
		t.Error("monRows[0].Enabled = true, want false (per the SetTarget override)")
	}
}

// TestMonitorToggleEnable drives the list screen's enter/space toggle: it should issue
// applyMonitorEnableCmd flipping the row under the cursor.
func TestMonitorToggleEnable(t *testing.T) {
	cfg := &config.Config{}
	api := &fakeAPI{cfg: cfg}
	m := newModel(api)
	m.step = stepManage
	m.mgr.screen = manageMonitorList
	m.mgr.monTargets = []core.TargetView{{ID: "disk:/", Kind: "disk", Display: "/", Available: true}}
	m.mgr.monRows = buildMonitorRows(m.mgr.monTargets, cfg)
	m.mgr.monCursor = 0

	var mm tea.Model = m
	mm, cmd := mm.Update(keyRunes(' '))
	if cmd == nil {
		t.Fatal("expected applyMonitorEnableCmd, got nil")
	}
	msg := runCmd(t, cmd)
	applied, ok := msg.(monitorAppliedMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want monitorAppliedMsg", msg)
	}
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	if api.applied == nil || api.applied.TargetEnabled("disk:/") {
		t.Error("applied config should have disk:/ disabled after toggling an enabled row")
	}

	mm, _ = mm.Update(applied)
	got := mm.(model)
	if got.mgr.monRows[0].Enabled {
		t.Error("monRows[0].Enabled = true after toggle+applied msg, want false")
	}
}

// TestMonitorThresholdEdit drives 't' -> type a value -> enter, and asserts
// the applied config carries the new threshold and monRows reflects it.
func TestMonitorThresholdEdit(t *testing.T) {
	cfg := &config.Config{}
	api := &fakeAPI{cfg: cfg}
	m := newModel(api)
	m.step = stepManage
	m.mgr.screen = manageMonitorList
	m.mgr.monTargets = []core.TargetView{{ID: "disk:/", Kind: "disk", Display: "/", Available: true}}
	m.mgr.monRows = buildMonitorRows(m.mgr.monTargets, cfg)
	m.mgr.monCursor = 0

	var mm tea.Model = m
	mm, _ = mm.Update(keyRunes('t'))
	if mm.(model).mgr.screen != manageMonitorThreshold {
		t.Fatalf("mgr.screen = %v, want manageMonitorThreshold", mm.(model).mgr.screen)
	}

	mm = typeString(t, mm, "88.5")
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	if mm.(model).mgr.screen != manageMonitorList {
		t.Fatalf("mgr.screen after commit = %v, want manageMonitorList", mm.(model).mgr.screen)
	}
	msg := runCmd(t, cmd)
	applied := msg.(monitorAppliedMsg)
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	v, ok := api.applied.TargetThreshold("disk:/")
	if !ok || v != 88.5 {
		t.Errorf("applied threshold = %v, %v, want 88.5, true", v, ok)
	}

	mm, _ = mm.Update(applied)
	got := mm.(model)
	if !got.mgr.monRows[0].ThresholdSet || got.mgr.monRows[0].Threshold != 88.5 {
		t.Errorf("monRows[0] = %+v, want ThresholdSet=true Threshold=88.5", got.mgr.monRows[0])
	}
}

// TestManageMonitorMenuEntryIssuesDiscoverCmd drives the menu -> "monitor thresholds" path
// (menu cursor 3) and asserts it issues discoverMonitorCmd.
func TestManageMonitorMenuEntryIssuesDiscoverCmd(t *testing.T) {
	api := &fakeAPI{
		cfg:            &config.Config{},
		monitorTargets: []core.TargetView{{ID: "disk:/", Kind: "disk", Display: "/", Available: true}},
	}
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(keyRunes('m'))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // open "monitor thresholds"
	got := mm.(model)
	if got.mgr.screen != manageMonitorList {
		t.Fatalf("mgr.screen = %v, want manageMonitorList", got.mgr.screen)
	}
	if !got.mgr.monLoading {
		t.Error("monLoading = false, want true right after opening the screen")
	}
	if cmd == nil {
		t.Fatal("expected discoverMonitorCmd, got nil")
	}
	msg := runCmd(t, cmd)
	tmsg, ok := msg.(monitorTargetsMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want monitorTargetsMsg", msg)
	}
	if tmsg.err != nil {
		t.Fatalf("discover err = %v, want nil", tmsg.err)
	}
	if len(tmsg.targets) != 1 || tmsg.targets[0].ID != "disk:/" {
		t.Errorf("targets = %+v, want the fake's MonitorTargets result to round-trip", tmsg.targets)
	}
}

// TestManageMonitorMenuEntrySurfacesDiscoverError asserts a MonitorTargets error from the
// socket reaches monitorTargetsMsg.err rather than being swallowed or crashing the flow.
func TestManageMonitorMenuEntrySurfacesDiscoverError(t *testing.T) {
	wantErr := errors.New("discovery failed")
	api := &fakeAPI{cfg: &config.Config{}, monitorTargetsErr: wantErr}
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(keyRunes('m'))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	msg := runCmd(t, cmd)
	tmsg := msg.(monitorTargetsMsg)
	if tmsg.err == nil || tmsg.err.Error() != wantErr.Error() {
		t.Fatalf("discover err = %v, want %v", tmsg.err, wantErr)
	}

	mm, _ = mm.Update(tmsg)
	got := mm.(model)
	if got.mgr.monErr == nil {
		t.Error("mgr.monErr = nil, want the discover error surfaced")
	}
	if got.mgr.monLoading {
		t.Error("monLoading should be false once the (errored) msg lands")
	}
}
