package serverwatch

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestAlertsAckUnackViaCLI(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir

	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	s := NewAlertState()
	s.Active["disk:/"] = ActiveAlert{Since: 100, Reason: "disk:/ = 95.0 ≥ threshold 90.0"}
	statePath := filepath.Join(dir, "alerts.json")
	if err := s.Save(statePath); err != nil {
		t.Fatal(err)
	}

	if code := Main([]string{"alerts", "ack", "disk:/"}); code != 0 {
		t.Fatalf("ack exit=%d stderr=%s", code, errb.String())
	}
	reloaded := LoadAlertState(statePath, osFS{})
	if !reloaded.Active["disk:/"].Acked {
		t.Fatalf("expected disk:/ acked after CLI ack, got %+v", reloaded.Active["disk:/"])
	}

	errb.Reset()
	if code := Main([]string{"alerts", "ack", "nope"}); code == 0 {
		t.Fatal("expected non-zero exit acking a non-active key")
	}
	if !strings.Contains(errb.String(), "nope") {
		t.Errorf("expected key name in error, got %q", errb.String())
	}

	if code := Main([]string{"alerts", "unack", "disk:/"}); code != 0 {
		t.Fatalf("unack exit=%d stderr=%s", code, errb.String())
	}
	reloaded2 := LoadAlertState(statePath, osFS{})
	if reloaded2.Active["disk:/"].Acked {
		t.Fatalf("expected disk:/ unacked, got %+v", reloaded2.Active["disk:/"])
	}
}

func TestAlertsListShowsActiveAndHistoryViaCLI(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir

	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	s := NewAlertState()
	s.Active["cpu"] = ActiveAlert{Since: 100, Reason: "cpu = 99.0 ≥ threshold 90.0"}
	if err := s.Save(filepath.Join(dir, "alerts.json")); err != nil {
		t.Fatal(err)
	}

	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(AlertEvent{
		Time: 100, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly",
		Delivered: []Delivery{{Channel: "telegram", OK: true}},
	}); err != nil {
		t.Fatal(err)
	}

	if code := Main([]string{"alerts", "list", "--since", "999999h"}); code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "cpu") {
		t.Fatalf("expected active alert 'cpu' in output: %q", got)
	}
	if !strings.Contains(got, "telegram") {
		t.Fatalf("expected history event with 'telegram' delivery in output: %q", got)
	}

	// Bare `alerts` (no subcommand) behaves like `alerts list`.
	out.Reset()
	if code := Main([]string{"alerts"}); code != 0 {
		t.Fatalf("bare alerts exit=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "cpu") {
		t.Fatalf("expected active alert in bare `alerts` output: %q", out.String())
	}
}

func TestAlertsUnknownSubcommand(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	var errb bytes.Buffer
	stderr = &errb
	if code := Main([]string{"alerts", "bogus"}); code == 0 {
		t.Fatal("expected non-zero exit for unknown alerts subcommand")
	}
}
