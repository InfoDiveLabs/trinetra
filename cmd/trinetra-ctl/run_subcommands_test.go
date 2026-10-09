package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// TestRunStatusJSON: `status --json` emits valid JSON carrying the snapshot's
// fields (json tags), for piping into jq etc. The --json flag may appear
// anywhere in the args.
func TestRunStatusJSON(t *testing.T) {
	api := &fakeAPI{snapshot: core.DashboardView{Online: true, CPU: 42.5, Cores: 8}}
	var buf bytes.Buffer
	if code := run(api, []string{"status", "--json"}, &buf); code != 0 {
		t.Fatalf("status --json exit = %d, want 0\n%s", code, buf.String())
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if got["cpu"] != 42.5 {
		t.Errorf("json cpu = %v, want 42.5", got["cpu"])
	}
}

// TestRunAlertsJSON: `alerts --json` emits a JSON array of the active alerts.
func TestRunAlertsJSON(t *testing.T) {
	api := &fakeAPI{active: []core.AlertRecord{{Key: "docker:web", Severity: "critical"}}}
	var buf bytes.Buffer
	if code := run(api, []string{"--json", "alerts"}, &buf); code != 0 {
		t.Fatalf("alerts --json exit = %d, want 0", code)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not valid JSON array: %v\n%s", err, buf.String())
	}
	if len(got) != 1 || got[0]["key"] != "docker:web" {
		t.Errorf("unexpected alerts json: %v", got)
	}
}

// TestRunConfigGet: `config get <key>` prints the current value of a known
// flat config key.
func TestRunConfigGet(t *testing.T) {
	cfg := &config.Config{}
	if err := cfg.Set("web.enabled", "true"); err != nil {
		t.Fatalf("seed set: %v", err)
	}
	api := &fakeAPI{cfg: cfg}
	var buf bytes.Buffer
	if code := run(api, []string{"config", "get", "web.enabled"}, &buf); code != 0 {
		t.Fatalf("config get exit = %d, want 0\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "true") {
		t.Errorf("config get web.enabled = %q, want it to contain 'true'", buf.String())
	}
}

// TestRunConfigGetUnknownKey: an unrecognized key is a usage error (exit 2),
// not a silent empty value.
func TestRunConfigGetUnknownKey(t *testing.T) {
	api := &fakeAPI{}
	var buf bytes.Buffer
	if code := run(api, []string{"config", "get", "no.such.key"}, &buf); code != 2 {
		t.Fatalf("unknown key exit = %d, want 2", code)
	}
	if !strings.Contains(buf.String(), "no.such.key") {
		t.Errorf("expected the unknown key echoed back: %q", buf.String())
	}
}

// TestRunConfigSet: `config set <key> <value>` validates via config.Set and
// commits via ApplyConfig, so the daemon receives the mutated config.
func TestRunConfigSet(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	var buf bytes.Buffer
	if code := run(api, []string{"config", "set", "web.enabled", "true"}, &buf); code != 0 {
		t.Fatalf("config set exit = %d, want 0\n%s", code, buf.String())
	}
	if api.applied == nil {
		t.Fatalf("ApplyConfig was not called")
	}
	if v, _ := api.applied.Get("web.enabled"); v != "true" {
		t.Errorf("applied web.enabled = %q, want true", v)
	}
}

// TestRunConfigSetInvalidNotApplied: a value config.Set rejects must NOT be
// committed (ApplyConfig never called) and the command fails (exit 1).
func TestRunConfigSetInvalidNotApplied(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	var buf bytes.Buffer
	if code := run(api, []string{"config", "set", "web.enabled", "notabool"}, &buf); code != 1 {
		t.Fatalf("invalid config set exit = %d, want 1\n%s", code, buf.String())
	}
	if api.applied != nil {
		t.Errorf("invalid value must not be applied, but ApplyConfig ran")
	}
}

// TestRunConfigSetRefusedWhenManaged pins `trinetra-ctl config set`'s
// managed-key refusal: identical
// message to the CLI/web/TUI, and neither Config() nor ApplyConfig is ever
// called.
func TestRunConfigSetRefusedWhenManaged(t *testing.T) {
	base := &fakeAPI{cfg: config.Default()}
	api := fleetAwareFakeAPI{fakeAPI: base, status: managedStatus("thresholds.cpu_pct", "fragcpu654321")}
	var buf bytes.Buffer
	if code := run(api, []string{"config", "set", "thresholds.cpu_pct", "50"}, &buf); code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, buf.String())
	}
	if !containsAll(buf.String(), "thresholds.cpu_pct", "managed by the fleet master", "fragcpu654321") {
		t.Fatalf("output = %s, want the managed-by-master refusal naming the fragment", buf.String())
	}
	if base.applied != nil {
		t.Errorf("ApplyConfig must not be called for a managed key")
	}
}

// TestRunConfigUsage: `config` with no subcommand is a usage error.
func TestRunConfigUsage(t *testing.T) {
	api := &fakeAPI{}
	var buf bytes.Buffer
	if code := run(api, []string{"config"}, &buf); code != 2 {
		t.Errorf("bare config exit = %d, want 2", code)
	}
}

// TestRunChannelsTest: `channels test <name>` asks the daemon to send a live
// test notification through the named channel.
func TestRunChannelsTest(t *testing.T) {
	api := &fakeAPI{}
	var buf bytes.Buffer
	if code := run(api, []string{"channels", "test", "telegram"}, &buf); code != 0 {
		t.Fatalf("channels test exit = %d, want 0\n%s", code, buf.String())
	}
	if len(api.testChannelCalls) != 1 || api.testChannelCalls[0] != "telegram" {
		t.Errorf("TestChannel calls = %v, want [telegram]", api.testChannelCalls)
	}
}

// TestRunChannelsTestError: a failed test send surfaces as exit 1.
func TestRunChannelsTestError(t *testing.T) {
	api := &fakeAPI{testChannelErr: errTestBoom}
	var buf bytes.Buffer
	if code := run(api, []string{"channels", "test", "telegram"}, &buf); code != 1 {
		t.Errorf("failed channels test exit = %d, want 1", code)
	}
}

var errTestBoom = &stringErr{"boom"}

type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }
