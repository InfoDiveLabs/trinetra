// Package serverwatch: coreapi_write_test.go covers the task-8 write methods
// (ApplyConfig/AckAlert/UnackAlert/TestChannel) on both core.API
// implementations -- the counterpart to coreapi_inproc_test.go/
// coreapi_file_test.go's read-method coverage.
package serverwatch

import (
	"path/filepath"
	"strings"
	"testing"

	"serverwatch/internal/config"
)

// TestFileAPIApplyConfigPersists is the task-8 brief's Step 1 test: the
// file-backed ApplyConfig must persist the posted config to cfgPath (via
// saveCfg) so a later config.Load(cfgPath) sees it -- the same behavior
// every existing `channel`/`target` CLI setter gets from saveCfg directly.
// cfgPath is overridden via the package-level test seam (mirrors
// channel_cli_test.go/dump_test.go/main_test.go's own use of it).
func TestFileAPIApplyConfigPersists(t *testing.T) {
	dir := t.TempDir()
	prevCfgPath := cfgPath
	cfgPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { cfgPath = prevCfgPath })

	api := newFileAPI(dir, config.Default())
	c := config.Default()
	c.Thresholds.CPUPct = 42
	if err := api.ApplyConfig(c); err != nil {
		t.Fatal(err)
	}

	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got.Thresholds.CPUPct != 42 {
		t.Fatalf("not persisted: %v", got.Thresholds.CPUPct)
	}
}

// TestInprocApplyConfigInvokesReloadClosure pins that the in-process
// ApplyConfig is nothing but a pass-through to the reload closure
// newInprocAPI was constructed with -- the exact pointer-swap-and-persist
// behavior cmdDaemon's own `reload` closure (daemon.go) performs. A fake
// closure here (rather than the real daemon reload) isolates ApplyConfig's
// OWN contract -- "call reload with what I was given, propagate its error"
// -- from reload's internal saveCfg/applyConfig mechanics, which belong to
// daemon.go and aren't this method's concern.
func TestInprocApplyConfigInvokesReloadClosure(t *testing.T) {
	var got *config.Config
	reload := func(c *config.Config) error {
		got = c
		return nil
	}
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, t.TempDir(), reload)

	c := config.Default()
	c.Thresholds.CPUPct = 77
	if err := api.ApplyConfig(c); err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("reload closure was not invoked with the exact config ApplyConfig was given")
	}
}

// TestInprocApplyConfigPropagatesReloadError pins that a reload failure
// (e.g. the daemon's own validation) surfaces back through ApplyConfig
// rather than being swallowed.
func TestInprocApplyConfigPropagatesReloadError(t *testing.T) {
	wantErr := errNotExist
	reload := func(*config.Config) error { return wantErr }
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, t.TempDir(), reload)

	if err := api.ApplyConfig(config.Default()); err != wantErr {
		t.Fatalf("ApplyConfig() err = %v, want %v", err, wantErr)
	}
}

// TestFileAPIAckAlertUnackAlertRoundTrip pins fileAPI's Ack/Unack: they must
// load alerts.json, flip Acked/AckedAt via AlertState.Ack/Unack, and save it
// back -- readable afterward via LoadAlertState, exactly like
// cmdAlertsAck's pre-task-8 inline sequence (now delegated to this method,
// see alerts_cli.go).
func TestFileAPIAckAlertUnackAlertRoundTrip(t *testing.T) {
	dir := t.TempDir()
	state := NewAlertState()
	state.Active["cpu"] = ActiveAlert{Since: 1000, Reason: "cpu high"}
	statePath := filepath.Join(dir, "alerts.json")
	if err := state.Save(statePath); err != nil {
		t.Fatal(err)
	}

	api := newFileAPI(dir, config.Default())
	if err := api.AckAlert("cpu"); err != nil {
		t.Fatalf("AckAlert: %v", err)
	}
	got := LoadAlertState(statePath, osFS{})
	if !got.Active["cpu"].Acked || got.Active["cpu"].AckedAt == 0 {
		t.Fatalf("expected cpu acked after AckAlert, got %+v", got.Active["cpu"])
	}

	if err := api.UnackAlert("cpu"); err != nil {
		t.Fatalf("UnackAlert: %v", err)
	}
	got2 := LoadAlertState(statePath, osFS{})
	if got2.Active["cpu"].Acked {
		t.Fatalf("expected cpu unacked after UnackAlert, got %+v", got2.Active["cpu"])
	}
}

// TestFileAPIAckAlertUnknownKeyReturnsError pins the same "no active alert
// for key %q" error AlertState.Ack itself returns for a never-fired key --
// the exact message cmdAlertsAck has always printed to stderr on a bad ack.
func TestFileAPIAckAlertUnknownKeyReturnsError(t *testing.T) {
	api := newFileAPI(t.TempDir(), config.Default())
	err := api.AckAlert("nope")
	if err == nil || !strings.Contains(err.Error(), `no active alert for key "nope"`) {
		t.Fatalf("AckAlert(unknown) err = %v, want it to mention the missing key", err)
	}
}

// TestInprocAckAlertUnackAlertRoundTrip is TestFileAPIAckAlertUnackAlertRoundTrip's
// in-process counterpart.
func TestInprocAckAlertUnackAlertRoundTrip(t *testing.T) {
	dir := t.TempDir()
	state := NewAlertState()
	state.Active["mem"] = ActiveAlert{Since: 500, Reason: "mem high"}
	statePath := filepath.Join(dir, "alerts.json")
	if err := state.Save(statePath); err != nil {
		t.Fatal(err)
	}

	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, dir, nil)
	if err := api.AckAlert("mem"); err != nil {
		t.Fatalf("AckAlert: %v", err)
	}
	got := LoadAlertState(statePath, osFS{})
	if !got.Active["mem"].Acked {
		t.Fatalf("expected mem acked after AckAlert, got %+v", got.Active["mem"])
	}

	if err := api.UnackAlert("mem"); err != nil {
		t.Fatalf("UnackAlert: %v", err)
	}
	got2 := LoadAlertState(statePath, osFS{})
	if got2.Active["mem"].Acked {
		t.Fatalf("expected mem unacked after UnackAlert, got %+v", got2.Active["mem"])
	}
}

// TestFileAPITestChannelDelegatesToSendTestNotification and its in-process
// counterpart below pin that TestChannel routes through the shared
// sendTestNotification (channel.go) rather than reimplementing it: an
// unknown channel name surfaces sendTestNotification's own "unknown channel"
// error, proving the call actually reached it (a real send is out of scope
// for a unit test -- see channel_cli_test.go's TestChannelAddListSetRemoveTestViaCLI
// for the equivalent CLI-level proof against a real, if undeliverable,
// channel).
func TestFileAPITestChannelDelegatesToSendTestNotification(t *testing.T) {
	api := newFileAPI(t.TempDir(), config.Default())
	err := api.TestChannel("does-not-exist")
	if err == nil || !strings.Contains(err.Error(), `unknown channel "does-not-exist"`) {
		t.Fatalf("TestChannel(unknown) err = %v, want an unknown-channel error", err)
	}
}

func TestInprocTestChannelDelegatesToSendTestNotification(t *testing.T) {
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, t.TempDir(), nil)
	err := api.TestChannel("does-not-exist")
	if err == nil || !strings.Contains(err.Error(), `unknown channel "does-not-exist"`) {
		t.Fatalf("TestChannel(unknown) err = %v, want an unknown-channel error", err)
	}
}

// TestInprocUnimplementedMethodsReturnSentinel pins that, after task 8, the
// ONLY inprocAPI method still returning errCoreNotImplemented is Subscribe
// (deferred to S5) -- every write method now does real work.
func TestInprocUnimplementedMethodsReturnSentinel(t *testing.T) {
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, t.TempDir(), func(*config.Config) error { return nil })
	if _, err := api.Subscribe(nil); err != errCoreNotImplemented { //nolint:staticcheck // nil context: exercising the stub only
		t.Errorf("Subscribe() err = %v, want errCoreNotImplemented", err)
	}
}
