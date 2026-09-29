package trinetra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// holdApplyLock simulates another process (CLI, socket or web) in the middle
// of an apply/rollback/install.
func holdApplyLock(t *testing.T, p updatePaths) {
	t.Helper()
	unlock, ok, err := update.TryLock(p.applyLock())
	if err != nil || !ok {
		t.Fatalf("take apply lock: ok=%v err=%v", ok, err)
	}
	t.Cleanup(unlock)
}

// TestConcurrentApplyRollbackRefused is R16: while another apply/rollback/
// install holds update/apply.lock, every entry point refuses at once with
// "update already in progress" and touches nothing.
func TestConcurrentApplyRollbackRefused(t *testing.T) {
	p := testUpdatePaths(t)
	os.MkdirAll(p.previous(), 0o700)
	os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("PREV-core"), 0o755)
	holdApplyLock(t, p)
	src := signedRelease(t, "0.5.0", map[string][]byte{"trinetra-linux-amd64": []byte("NEW-core"), "trinetra-web-linux-amd64": []byte("NEW-web")})
	guarded := 0
	u := updater{paths: p, keys: testKeys(), x: fakeVersionExec("0.5.0"), now: func() time.Time { return time.Unix(1000, 0) },
		arch: "amd64", running: mustVer("0.4.1"), launchGuard: func() error { guarded++; return nil }, src: src}
	c := config.Default()

	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, errUpdateInProgress) || !strings.Contains(err.Error(), "already in progress") {
			t.Errorf("%s while locked: %v, want errUpdateInProgress", what, err)
		}
	}
	_, err := u.apply(context.Background(), c, applyOptions{Version: "0.5.0"})
	check("apply", err)
	check("rollback", u.rollback())
	check("preflightApply", u.preflightApply(c, applyOptions{Version: "0.5.0"}))
	check("preflightRollback", u.preflightRollback())

	if guarded != 0 {
		t.Fatal("guard launched while another update held the lock")
	}
	if b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra")); string(b) != "OLD-core" {
		t.Fatalf("binary changed: %q", b)
	}
	if _, err := os.Stat(p.staging("0.5.0")); !os.IsNotExist(err) {
		t.Fatal("staged while another update held the lock")
	}
}

// TestInstallRefusedWhilePendingOrLocked is R16: install takes the same
// apply lock and refuses while an update is pending (finish or roll back
// first), so it can never overwrite binaries under a live guard.
func TestInstallRefusedWhilePendingOrLocked(t *testing.T) {
	p := testUpdatePaths(t)
	update.SaveState(p.dir(), update.State{Pending: &update.Pending{Version: "0.5.0", From: "0.4.1"}})
	if _, err := installPreflight(p); err == nil || !strings.Contains(err.Error(), "pending") || !strings.Contains(err.Error(), "roll") {
		t.Fatalf("install with a pending update: %v", err)
	}
	update.SaveState(p.dir(), update.State{})
	unlock, err := installPreflight(p)
	if err != nil {
		t.Fatalf("install preflight on an idle host: %v", err)
	}
	if _, err := installPreflight(p); !errors.Is(err, errUpdateInProgress) {
		t.Fatalf("second install while the first holds the lock: %v", err)
	}
	unlock()
}
