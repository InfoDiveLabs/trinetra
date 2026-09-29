package trinetra

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

func readAudit(t *testing.T, path string) []core.AuditEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit %s: %v", path, err)
	}
	var out []core.AuditEntry
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var e core.AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// TestUpdateAuditTrail is R23 (spec §2 step 6): apply start, rollback start
// and the guard's commit/rollback each leave an audit entry with the actor,
// in the local update audit log (or the fleet master's audit log when this
// host is a master).
func TestUpdateAuditTrail(t *testing.T) {
	p := testUpdatePaths(t)
	src := signedRelease(t, "0.5.0", map[string][]byte{"trinetra-linux-amd64": []byte("NEW-core"), "trinetra-web-linux-amd64": []byte("NEW-web")})
	u := updater{paths: p, keys: testKeys(), x: fakeVersionExec("0.5.0"), now: func() time.Time { return time.Unix(1000, 0) },
		arch: "amd64", running: mustVer("0.4.1"), launchGuard: func() error { return nil }, src: src, actor: "cli:alice"}
	if _, err := u.apply(context.Background(), config.Default(), applyOptions{Version: "0.5.0"}); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	if _, err := runGuard(guardDeps{paths: p, health: &fakeHealth{active: true, version: "0.5.0", ts: 1002},
		now: clk.now, sleep: clk.sleep, restart: func() error { return nil }}); err != nil {
		t.Fatal(err)
	}
	u.actor = "socket"
	u.x = fakeVersionExec("0.4.1")
	if err := u.rollback(); err != nil {
		t.Fatal(err)
	}

	got := readAudit(t, filepath.Join(p.dir(), "audit.jsonl"))
	want := []struct{ actor, action, target string }{
		{"cli:alice", "update.apply", "0.5.0"},
		{"guard", "update.commit", "0.5.0"},
		{"socket", "update.rollback", "0.4.1"},
	}
	if len(got) != len(want) {
		t.Fatalf("audit entries = %+v", got)
	}
	for i, w := range want {
		if got[i].Actor != w.actor || got[i].Action != w.action || got[i].Target != w.target {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], w)
		}
	}

	// A fleet master records update events in its fleet audit log.
	os.MkdirAll(fleetMasterDir(p.StateDir), 0o700)
	if _, err := rollbackPending(p, update.Pending{Version: "0.5.9", From: "0.5.0"}, "x", func() error { return nil }, time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}
	fleet := readAudit(t, filepath.Join(fleetMasterDir(p.StateDir), "audit.jsonl"))
	if len(fleet) != 1 || fleet[0].Action != "update.rolled_back" || fleet[0].Actor != "guard" || fleet[0].Target != "0.5.9" {
		t.Fatalf("fleet audit = %+v", fleet)
	}
}

// TestUpdateStatusShowsLastCheck is R23: `update status` shows when the
// channel was last checked.
func TestUpdateStatusShowsLastCheck(t *testing.T) {
	p := testUpdatePaths(t)
	update.SaveState(p.dir(), update.State{LastCheck: 1700000000})
	st, err := updater{paths: p}.status()
	if err != nil || st.LastCheck != 1700000000 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if v := toUpdateStatusView(st); v.LastCheck != 1700000000 {
		t.Fatalf("view LastCheck = %d", v.LastCheck)
	}
	var out bytes.Buffer
	renderUpdateStatus(&out, st)
	if !strings.Contains(out.String(), "last check:") || !strings.Contains(out.String(), "2023-11-14") {
		t.Fatalf("status text:\n%s", out.String())
	}
	out.Reset()
	renderUpdateStatus(&out, updateStatus{})
	if !strings.Contains(out.String(), "last check: never") {
		t.Fatalf("status text with no check:\n%s", out.String())
	}
}

// TestTelegramVersionShowsAvailableUpdate is R23 (spec §2 Commands):
// Telegram /version reports the running version and, when a newer accepted
// release is known, "update available: X".
func TestTelegramVersionShowsAvailableUpdate(t *testing.T) {
	p := testUpdatePaths(t)
	if got := handleCommand("/version", nil, Snapshot{}, nil); !strings.Contains(got, "trinetra ") || strings.Contains(got, "update available") {
		t.Fatalf("/version with nothing available = %q", got)
	}
	update.SaveState(p.dir(), update.State{Available: "0.5.1"})
	if got := handleCommand("/version", nil, Snapshot{}, nil); !strings.Contains(got, "update available: 0.5.1") {
		t.Fatalf("/version = %q", got)
	}
	if !strings.Contains(helpText, "/version") {
		t.Fatal("/version missing from /help")
	}
}
