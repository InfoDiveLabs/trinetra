package serverwatch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// TestFileAPIReadsStatusJSON is the Step 1 failing test from the task-6
// brief: newFileAPI must read a status.json written to stateDir back as a
// projected DashboardView (via the same buildDashboardView the in-process
// impl uses), proving the file-backed core.API can serve a separate CLI
// process that has no access to the daemon's live memory.
func TestFileAPIReadsStatusJSON(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, realClock{})
	_ = st.WriteStatus(Snapshot{TS: 9, CPU: 55})
	api := newFileAPI(dir, config.Default())
	v, err := api.Snapshot()
	if err != nil || v.CPU != 55 {
		t.Fatalf("got %+v err %v", v, err)
	}
}

// TestFileAPISnapshotMissingStatusJSONErrors pins that, unlike the alert
// state/log reads below (which degrade a missing file to "empty"),
// Snapshot() surfaces a real error when status.json hasn't been written yet
// -- there is no meaningful "empty Snapshot" to project.
func TestFileAPISnapshotMissingStatusJSONErrors(t *testing.T) {
	api := newFileAPI(t.TempDir(), config.Default())
	if _, err := api.Snapshot(); err == nil {
		t.Fatal("expected an error reading Snapshot with no status.json written")
	}
}

// TestFileAPISnapshotMatchesBuildDashboardView is fileAPI's counterpart to
// TestInprocSnapshotMatchesBuildDashboardView (coreapi_inproc_test.go): a
// fully-populated Snapshot round-tripped through status.json must project
// identically to the re-homed buildDashboardView, proving the file-backed
// path shares the exact same projection as the in-process one.
func TestFileAPISnapshotMatchesBuildDashboardView(t *testing.T) {
	dir := t.TempDir()
	snap := fullyPopulatedSnapshot()
	cfg := config.Default()

	st := NewStore(dir, realClock{})
	if err := st.WriteStatus(snap); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}

	want := buildDashboardView(snap)
	want.Availability = core.Availability{}

	api := newFileAPI(dir, cfg)
	got, err := api.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	got.Availability = core.Availability{}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("newFileAPI(...).Snapshot() != buildDashboardView(snap):\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestFileAPIMonitoringMatchesBuildMonitoringView is Monitoring()'s
// counterpart to the Snapshot parity test above.
func TestFileAPIMonitoringMatchesBuildMonitoringView(t *testing.T) {
	dir := t.TempDir()
	snap := fullyPopulatedSnapshot()
	cfg := config.Default()

	st := NewStore(dir, realClock{})
	if err := st.WriteStatus(snap); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}

	want := buildMonitoringView(snap, cfg)

	api := newFileAPI(dir, cfg)
	got, err := api.Monitoring()
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("newFileAPI(...).Monitoring() != buildMonitoringView(snap, cfg):\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestFileAPISeriesResolutionMapping is fileAPI.Series' counterpart to
// TestSeriesResolutionMapping (coreapi_inproc_test.go), against a real
// tsfile SampleStore opened via openConfiguredStore -- which reads the
// package-level stateDir var, so this test points it at dir (mirroring how
// dump_test.go/alerts_cli_test.go point stateDir at their own temp dirs).
func TestFileAPISeriesResolutionMapping(t *testing.T) {
	dir := t.TempDir()
	prevStateDir := stateDir
	stateDir = dir
	t.Cleanup(func() { stateDir = prevStateDir })

	store, err := OpenStore("tsfile", dir, StoreOptions{RawRetention: time.Hour})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	now := time.Now().Unix()
	nowKey := (now / 60) * 60
	oldKey := nowKey - 7200

	oldTS1, oldTS2 := oldKey+10, oldKey+20
	recentTS := nowKey

	if err := store.Append(oldTS1, MetricSet{"cpu": 40}); err != nil {
		t.Fatalf("Append oldTS1: %v", err)
	}
	if err := store.Append(oldTS2, MetricSet{"cpu": 60}); err != nil {
		t.Fatalf("Append oldTS2: %v", err)
	}
	if err := store.Append(recentTS, MetricSet{"cpu": 99}); err != nil {
		t.Fatalf("Append recentTS: %v", err)
	}
	if err := store.Downsample(now); err != nil {
		t.Fatalf("Downsample: %v", err)
	}
	// fileAPI opens the store fresh per call (openConfiguredStore) rather
	// than holding a long-lived handle, so close this one before querying
	// through the API to avoid two backends fighting over the same files.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cfg := config.Default()
	cfg.Storage.RawRetention = "1h"
	api := newFileAPI(dir, cfg)

	raw, err := api.Series("cpu", oldKey-10, now+10, core.ResRaw)
	if err != nil {
		t.Fatalf("Series(ResRaw): %v", err)
	}
	if len(raw) != 3 {
		t.Fatalf("ResRaw len = %d, want 3: %+v", len(raw), raw)
	}

	oneM, err := api.Series("cpu", oldKey-10, now+10, core.Res1m)
	if err != nil {
		t.Fatalf("Series(Res1m): %v", err)
	}
	if len(oneM) != 1 || oneM[0].TS != oldKey || oneM[0].Min != 40 || oneM[0].Avg != 50 || oneM[0].Max != 60 {
		t.Fatalf("Res1m point = %+v, want {TS:%d Min:40 Avg:50 Max:60}", oneM, oldKey)
	}

	autoOld, err := api.Series("cpu", oldKey-10, oldKey+70, core.ResAuto)
	if err != nil {
		t.Fatalf("Series(ResAuto, old window): %v", err)
	}
	if len(autoOld) != 1 || autoOld[0].TS != oldKey {
		t.Fatalf("ResAuto (old window) = %+v, want the 1m aggregate at %d", autoOld, oldKey)
	}
}

// TestFileAPIEventsMapsStoreEvents is fileAPI.Events' counterpart to
// TestEventsMapsStoreEvents (coreapi_inproc_test.go).
func TestFileAPIEventsMapsStoreEvents(t *testing.T) {
	dir := t.TempDir()
	prevStateDir := stateDir
	stateDir = dir
	t.Cleanup(func() { stateDir = prevStateDir })

	cfg := config.Default()
	store, err := openConfiguredStore(cfg)
	if err != nil {
		t.Fatalf("openConfiguredStore: %v", err)
	}
	if err := store.AppendEvent(DownEvent{Type: "power_down", Start: 100, End: 160, DurationSec: 60}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	api := newFileAPI(dir, cfg)
	got, err := api.Events(0, 200)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	want := []core.DownEventView{{Type: "power_down", Start: 100, End: 160, DurationSec: 60}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Events() = %+v, want %+v", got, want)
	}
}

// TestFileAPIActiveAlertsMapsFields is fileAPI.ActiveAlerts' counterpart to
// TestActiveAlertsMapsFields (coreapi_inproc_test.go).
func TestFileAPIActiveAlertsMapsFields(t *testing.T) {
	stateDir := t.TempDir()

	state := NewAlertState()
	state.Active["cpu"] = ActiveAlert{Since: 1000, Reason: "cpu = 95.0 >= threshold 90.0", Critical: true, Acked: false}
	state.Active["mem"] = ActiveAlert{Since: 2000, Reason: "mem = 80.0 >= threshold 75.0", Critical: false, Acked: true, AckedAt: 2500}
	if err := state.Save(filepath.Join(stateDir, "alerts.json")); err != nil {
		t.Fatalf("state.Save: %v", err)
	}

	api := newFileAPI(stateDir, config.Default())
	got, err := api.ActiveAlerts()
	if err != nil {
		t.Fatalf("ActiveAlerts: %v", err)
	}

	want := []core.AlertRecord{
		{Key: "cpu", Severity: "critical", Kind: "", Source: "cpu = 95.0 >= threshold 90.0", Time: 1000, Acked: false},
		{Key: "mem", Severity: "warning", Kind: "", Source: "mem = 80.0 >= threshold 75.0", Time: 2000, Acked: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ActiveAlerts() = %+v, want %+v (sorted by key)", got, want)
	}
}

// TestFileAPIAlertHistoryNewestFirstAndLimit is fileAPI.AlertHistory's
// counterpart to TestAlertHistoryNewestFirstAndLimit (coreapi_inproc_test.go).
func TestFileAPIAlertHistoryNewestFirstAndLimit(t *testing.T) {
	stateDir := t.TempDir()

	log := NewAlertLog(filepath.Join(stateDir, "alertlog.jsonl"))
	events := []AlertEvent{
		{Time: 100, Key: "cpu", Title: "CPU high", Severity: "critical", Kind: "fire", Source: "threshold"},
		{Time: 300, Key: "mem", Title: "Mem high", Severity: "warning", Kind: "fire", Source: "threshold"},
		{Time: 200, Key: "cpu", Title: "CPU normal", Severity: "critical", Kind: "recover", Source: "threshold"},
	}
	for _, ev := range events {
		if err := log.AppendAlertEvent(ev); err != nil {
			t.Fatalf("AppendAlertEvent(%+v): %v", ev, err)
		}
	}

	api := newFileAPI(stateDir, config.Default())

	got, err := api.AlertHistory(0, 0)
	if err != nil {
		t.Fatalf("AlertHistory(0, 0): %v", err)
	}
	wantAll := []core.AlertRecord{
		{Key: "mem", Severity: "warning", Kind: "fire", Source: "threshold", Time: 300, Acked: false},
		{Key: "cpu", Severity: "critical", Kind: "recover", Source: "threshold", Time: 200, Acked: false},
		{Key: "cpu", Severity: "critical", Kind: "fire", Source: "threshold", Time: 100, Acked: false},
	}
	if !reflect.DeepEqual(got, wantAll) {
		t.Fatalf("AlertHistory(0, 0) = %+v, want %+v (newest first)", got, wantAll)
	}

	gotLimited, err := api.AlertHistory(0, 2)
	if err != nil {
		t.Fatalf("AlertHistory(0, 2): %v", err)
	}
	if !reflect.DeepEqual(gotLimited, wantAll[:2]) {
		t.Fatalf("AlertHistory(0, 2) = %+v, want %+v", gotLimited, wantAll[:2])
	}
}

// TestFileAPIConfigReturnsPassedCfg pins Config()'s contract: it just
// returns the cfg newFileAPI was constructed with, no reload.
func TestFileAPIConfigReturnsPassedCfg(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.RawRetention = "3h"
	api := newFileAPI(t.TempDir(), cfg)
	got, err := api.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got != cfg {
		t.Fatalf("Config() returned a different *config.Config than newFileAPI was given")
	}
}

// TestFileAPIUnimplementedMethodsReturnSentinel pins that the write/Doctor/
// Subscribe methods all return errCoreNotImplemented, same as inprocAPI.
func TestFileAPIUnimplementedMethodsReturnSentinel(t *testing.T) {
	api := newFileAPI(t.TempDir(), config.Default())
	if _, err := api.Doctor(); err != errCoreNotImplemented {
		t.Errorf("Doctor() err = %v, want errCoreNotImplemented", err)
	}
	if err := api.ApplyConfig(config.Default()); err != errCoreNotImplemented {
		t.Errorf("ApplyConfig() err = %v, want errCoreNotImplemented", err)
	}
	if err := api.AckAlert("cpu"); err != errCoreNotImplemented {
		t.Errorf("AckAlert() err = %v, want errCoreNotImplemented", err)
	}
	if err := api.UnackAlert("cpu"); err != errCoreNotImplemented {
		t.Errorf("UnackAlert() err = %v, want errCoreNotImplemented", err)
	}
	if err := api.TestChannel("telegram"); err != errCoreNotImplemented {
		t.Errorf("TestChannel() err = %v, want errCoreNotImplemented", err)
	}
	if _, err := api.Subscribe(nil); err != errCoreNotImplemented { //nolint:staticcheck // nil context: exercising the stub only
		t.Errorf("Subscribe() err = %v, want errCoreNotImplemented", err)
	}
}

// TestFileAPISeriesStoreOpenFailureWrapsErrorText is a regression guard
// added after a coordinator-flagged review finding: routing cmdDump through
// newFileAPI(...).Series(...) had silently dropped the "open sample store: "
// prefix cmdDump always printed on a store-open failure (it used to wrap
// openConfiguredStore's error itself; now that open happens inside Series,
// nothing re-added the prefix). This pins that Series wraps a store-open
// failure with that exact prefix, so any caller that just prints the
// returned error -- as cmdDump does -- reproduces the original wording.
// An invalid storage backend ("bogus") is used to force openConfiguredStore
// to fail deterministically, without needing filesystem permission tricks.
func TestFileAPISeriesStoreOpenFailureWrapsErrorText(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Backend = "bogus"

	api := newFileAPI(t.TempDir(), cfg)
	_, err := api.Series("cpu", 0, 1000, core.ResRaw)
	if err == nil {
		t.Fatal("expected an error opening a store with an invalid backend")
	}
	if !strings.HasPrefix(err.Error(), "open sample store: ") {
		t.Fatalf("Series() err = %q, want it prefixed with %q", err.Error(), "open sample store: ")
	}
}

// TestFileAPIEventsStoreOpenFailureWrapsErrorText is Events' counterpart to
// the Series test above, same reasoning: Events opens the store the same
// way and must wrap a failure with the same prefix.
func TestFileAPIEventsStoreOpenFailureWrapsErrorText(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Backend = "bogus"

	api := newFileAPI(t.TempDir(), cfg)
	_, err := api.Events(0, 1000)
	if err == nil {
		t.Fatal("expected an error opening a store with an invalid backend")
	}
	if !strings.HasPrefix(err.Error(), "open sample store: ") {
		t.Fatalf("Events() err = %q, want it prefixed with %q", err.Error(), "open sample store: ")
	}
}

// TestFileAPISeriesQueryFailureWrapsErrorText is a regression guard added
// after a second coordinator-flagged review finding, sibling to the
// store-open wrap tests above: the pre-routing dumpSeries (dump.go) wrapped
// a store.Query failure as fmt.Errorf("query %s: %w", metric, err); once
// Series took over the store.Query call, that wrap needed to move with it
// or cmdDump's stderr on a query failure (a corrupt tsfile, a permission
// error) would silently lose the "query <metric>: " prefix. This forces a
// real store.Query failure -- not a store-open failure -- by opening a real
// tsfile store once (to create its on-disk layout), then overwriting the
// "cpu" metric's raw .tsd file with garbage so a subsequent Query fails at
// the file's header check, and asserts the wrap is present.
func TestFileAPISeriesQueryFailureWrapsErrorText(t *testing.T) {
	dir := t.TempDir()
	prevStateDir := stateDir
	stateDir = dir // openConfiguredStore (called inside Series) reads this
	t.Cleanup(func() { stateDir = prevStateDir })

	cfg := config.Default() // Storage.Backend defaults to "tsfile"

	store, err := OpenStore("tsfile", dir, StoreOptions{})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// tsFileStore lays out <dir>/ts/raw/<safeMetric>.tsd; "cpu" needs no
	// percent-encoding (safeMetric passes [A-Za-z0-9._-] through literally).
	badPath := filepath.Join(dir, "ts", "raw", "cpu.tsd")
	if err := os.WriteFile(badPath, []byte("not a valid tsfile header"), 0o644); err != nil {
		t.Fatalf("write corrupt tsd file: %v", err)
	}

	api := newFileAPI(dir, cfg)
	_, err = api.Series("cpu", 0, 1000, core.ResRaw)
	if err == nil {
		t.Fatal("expected an error querying a corrupt tsfile")
	}
	if !strings.HasPrefix(err.Error(), "query cpu: ") {
		t.Fatalf("Series() err = %q, want it prefixed with %q", err.Error(), "query cpu: ")
	}
}
