package main

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"serverwatch/internal/config"
	sw "serverwatch/internal/serverwatch"
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

// TestManageScheduleOffAppliesImmediately drives: 'm' -> select "schedule"
// (menu cursor 0) -> the mode screen's default cursor is already "off" ->
// enter applies right away, with no value screen in between, and clears
// both schedule.daily/schedule.weekly.
func TestManageScheduleOffAppliesImmediately(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	api.cfg.Schedule.Daily = "03:30"
	var mm tea.Model = newModel(api)

	mm, _ = mm.Update(keyRunes('m'))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select "schedule" (menu cursor 0)
	if mm.(model).mgr.screen != manageScheduleMode {
		t.Fatalf("mgr.screen = %v, want manageScheduleMode", mm.(model).mgr.screen)
	}

	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // accept "off" (mode cursor 0)
	if !mm.(model).mgr.applying {
		t.Fatal("mgr.applying = false, want true")
	}
	if cmd == nil {
		t.Fatal("expected applyScheduleCmd, got nil")
	}
	msg := runCmd(t, cmd)
	applied, ok := msg.(manageAppliedMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want manageAppliedMsg", msg)
	}
	if applied.err != nil {
		t.Fatalf("apply err = %v, want nil", applied.err)
	}
	mm, _ = mm.Update(applied)
	got := mm.(model)
	if got.mgr.screen != manageResult {
		t.Fatalf("mgr.screen = %v, want manageResult", got.mgr.screen)
	}
	if got.mgr.applying {
		t.Error("mgr.applying should be false once the result msg lands")
	}
	if api.applyN != 1 {
		t.Fatalf("ApplyConfig called %d times, want 1", api.applyN)
	}
	if api.applied.Schedule.Daily != "" || api.applied.Schedule.Weekly != "" {
		t.Errorf("applied schedule = %+v, want both cleared", api.applied.Schedule)
	}
}

// TestManageScheduleDailySetsValue drives the daily path: mode "daily" ->
// type an HH:MM value -> enter applies, and asserts the applied config's
// schedule.daily/weekly match applySchedule's contract.
func TestManageScheduleDailySetsValue(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	var mm tea.Model = newModel(api)

	mm, _ = mm.Update(keyRunes('m'))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select "schedule"
	mm, _ = mm.Update(keyType(tea.KeyDown))  // mode cursor -> "daily"
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // select "daily"
	if mm.(model).mgr.screen != manageScheduleValue {
		t.Fatalf("mgr.screen = %v, want manageScheduleValue", mm.(model).mgr.screen)
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

// TestManageQuietHoursOff drives: 'm' -> "quiet hours" (menu cursor 1) ->
// type "off" -> enter applies, clearing quiet_hours.
func TestManageQuietHoursOff(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{QuietHours: "22-6"}}
	var mm tea.Model = newModel(api)

	mm, _ = mm.Update(keyRunes('m'))
	mm, _ = mm.Update(keyType(tea.KeyDown))  // menu cursor -> "quiet hours"
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // open it
	if mm.(model).mgr.screen != manageQuietValue {
		t.Fatalf("mgr.screen = %v, want manageQuietValue", mm.(model).mgr.screen)
	}

	mm = typeString(t, mm, "off")
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

// TestManageHealthchecksSetsURL drives: 'm' -> "healthchecks" (menu cursor
// 2) -> type a URL -> enter applies healthchecks.url.
func TestManageHealthchecksSetsURL(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	var mm tea.Model = newModel(api)

	mm, _ = mm.Update(keyRunes('m'))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // open "healthchecks"
	if mm.(model).mgr.screen != manageHealthValue {
		t.Fatalf("mgr.screen = %v, want manageHealthValue", mm.(model).mgr.screen)
	}

	mm = typeString(t, mm, "https://hc-ping.com/abc")
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

// TestManageValueApplyErrorSurfaces asserts a failing ApplyConfig reaches
// manageResult's applyErr, mirroring TestWizardApplyErrorSurfaces for the
// web-setup wizard.
func TestManageValueApplyErrorSurfaces(t *testing.T) {
	wantErr := errors.New("quiet_hours rejected")
	api := &fakeAPI{cfg: &config.Config{}, applyErr: wantErr}
	var mm tea.Model = newModel(api)

	mm, _ = mm.Update(keyRunes('m'))
	mm, _ = mm.Update(keyType(tea.KeyDown))
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // open "quiet hours"
	mm = typeString(t, mm, "22-6")
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
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

// TestManageResultAnyKeyReturnsToMenu asserts pressing a key on the result
// screen goes back to the menu list, not Home (so the user can keep
// managing other settings without re-entering via 'm').
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
	targets := []sw.Target{{ID: "disk:/", Kind: "disk", Display: "/", Available: true}}

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

// TestMonitorToggleEnable drives the list screen's enter/space toggle: it
// should issue applyMonitorEnableCmd flipping the row under the cursor, and
// once the resulting monitorAppliedMsg lands, monRows reflects the saved
// state (not just an optimistic local flip).
func TestMonitorToggleEnable(t *testing.T) {
	cfg := &config.Config{}
	api := &fakeAPI{cfg: cfg}
	m := newModel(api)
	m.step = stepManage
	m.mgr.screen = manageMonitorList
	m.mgr.monTargets = []sw.Target{{ID: "disk:/", Kind: "disk", Display: "/", Available: true}}
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
	m.mgr.monTargets = []sw.Target{{ID: "disk:/", Kind: "disk", Display: "/", Available: true}}
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

// TestManageMonitorMenuEntryIssuesDiscoverCmd drives the menu -> "monitor
// thresholds" path (menu cursor 3) and asserts it issues a non-nil discover
// command and flips monLoading, without asserting on real Discover()'s
// host-dependent target list.
func TestManageMonitorMenuEntryIssuesDiscoverCmd(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
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
}
