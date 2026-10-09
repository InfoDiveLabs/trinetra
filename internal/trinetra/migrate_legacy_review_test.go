package trinetra

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the serverwatch -> trinetra migration's edge cases.

// --- a legacy plugin with no trinetra counterpart is called out ---

func TestLegacyMigrationWarnsWhenPluginHasNoTrinetraCounterpart(t *testing.T) {
	webNote := "serverwatch-web was installed but no trinetra-web was found next to trinetra"
	ctlNote := "serverwatch-ctl was installed but no trinetra-ctl was found next to trinetra"
	cases := []struct {
		name             string
		legacyCtl        bool
		legacyWeb        bool
		newCtl, newWeb   bool
		wantCtl, wantWeb bool
	}{
		{name: "core only", legacyCtl: true, legacyWeb: true, wantCtl: true, wantWeb: true},
		{name: "all plugins", legacyCtl: true, legacyWeb: true, newCtl: true, newWeb: true},
		{name: "web missing", legacyCtl: true, legacyWeb: true, newCtl: true, wantWeb: true},
		{name: "legacy had ctl only", legacyCtl: true, wantCtl: true},
		{name: "legacy had no plugins"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testMigrationPaths(t)
			makeLegacyInstall(t, p)
			if !tc.legacyCtl {
				_ = os.Remove(p.OldCtl)
			}
			if !tc.legacyWeb {
				_ = os.Remove(p.OldWeb)
			}
			f := &fakeMigrationOps{paths: p}
			plan, err := planLegacyMigration(p, planOptions{})
			if err != nil || plan == nil {
				t.Fatalf("plan=%v err=%v", plan, err)
			}
			install := func() error {
				if err := f.install(); err != nil {
					return err
				}
				dir := filepath.Dir(p.NewBin)
				if tc.newCtl {
					mustWrite(t, filepath.Join(dir, "trinetra-ctl"), "CTL")
				}
				if tc.newWeb {
					mustWrite(t, filepath.Join(dir, "trinetra-web"), "WEB")
				}
				return nil
			}
			sum, err := applyLegacyMigration(plan, f, install)
			if err != nil {
				t.Fatal(err)
			}
			s := sum.String()
			if got := strings.Contains(s, webNote); got != tc.wantWeb {
				t.Errorf("web note present=%v, want %v:\n%s", got, tc.wantWeb, s)
			}
			if got := strings.Contains(s, ctlNote); got != tc.wantCtl {
				t.Errorf("ctl note present=%v, want %v:\n%s", got, tc.wantCtl, s)
			}
			if tc.wantWeb && !strings.Contains(s, "the web UI is down until you put trinetra-web in the same directory as the trinetra binary and re-run `sudo trinetra install`") {
				t.Errorf("web note lacks the fix:\n%s", s)
			}
			if tc.wantCtl && !strings.Contains(s, "re-run `sudo trinetra install`") {
				t.Errorf("ctl note lacks the fix:\n%s", s)
			}
		})
	}
}

// --- a serverwatch daemon running outside the unit blocks the move ---

func writeLegacyPID(t *testing.T, p migrationPaths, pid int) {
	t.Helper()
	mustWrite(t, filepath.Join(p.OldStateDir, legacyPIDFileName), fmt.Sprint(pid))
}

func TestLegacyMigrationRefusesStrayDaemonBeforeChangingAnything(t *testing.T) {
	for _, exe := range []string{"/usr/local/bin/serverwatch", "/usr/local/bin/serverwatch (deleted)", "/opt/sw/serverwatch"} {
		t.Run(exe, func(t *testing.T) {
			p := testMigrationPaths(t)
			makeLegacyInstall(t, p)
			writeLegacyPID(t, p, 4242)
			root := filepath.Dir(filepath.Dir(p.OldConfigDir))
			before := snapshot(t, root)
			f := &fakeMigrationOps{paths: p, procs: map[int]string{4242: exe}}

			_, err := runMigration(t, f)
			if err == nil {
				t.Fatal("migration ran while a stray serverwatch daemon was alive")
			}
			for _, want := range []string{"pid 4242", "outside serverwatch.service", "--force", "Nothing was changed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal lacks %q:\n%v", want, err)
				}
			}
			if after := snapshot(t, root); !equalSnap(before, after) {
				t.Error("refusal changed the filesystem")
			}
			for _, c := range f.calls {
				if c[0] != "is-active" {
					t.Errorf("refusal ran a changing systemctl call: %v", c)
				}
			}
			if f.installed != 0 {
				t.Error("install ran")
			}
		})
	}
}

// A daemon still alive after `systemctl stop` (the unit was running AND a
// second daemon was started by hand) is refused after the stop; the message
// says the service was stopped and no data moved.
func TestLegacyMigrationRefusesDaemonStillAliveAfterStop(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	writeLegacyPID(t, p, 4242)
	wantCfg, wantState := snapshot(t, p.OldConfigDir), snapshot(t, p.OldStateDir)
	f := &fakeMigrationOps{paths: p, activeUntilStop: true, procs: map[int]string{4242: "/usr/local/bin/serverwatch", 77: "/usr/local/bin/serverwatch"}, unitPIDs: []int{77}}

	_, err := runMigration(t, f)
	if err == nil {
		t.Fatal("migration ran while a serverwatch daemon survived the stop")
	}
	for _, want := range []string{"pid 4242", "still running after `systemctl stop serverwatch`", "serverwatch.service was stopped and disabled", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%v", want, err)
		}
	}
	if got := snapshot(t, p.OldStateDir); !equalSnap(got, wantState) {
		t.Error("state dir changed")
	}
	if got := snapshot(t, p.OldConfigDir); !equalSnap(got, wantCfg) {
		t.Error("config dir changed")
	}
	if lexistsT(p.NewStateDir) || lexistsT(p.NewConfigDir) || f.installed != 0 {
		t.Error("migration went past the refusal")
	}
}

// The pid file of the daemon serverwatch.service itself runs is not a stray:
// the stop ends it, and the migration proceeds.
func TestLegacyMigrationProceedsWhenUnitDaemonStopped(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	writeLegacyPID(t, p, 4242)
	f := &fakeMigrationOps{paths: p, activeUntilStop: true, procs: map[int]string{4242: "/usr/local/bin/serverwatch"}, unitPIDs: []int{4242}}
	if _, err := runMigration(t, f); err != nil {
		t.Fatalf("migration refused a daemon systemd just stopped: %v", err)
	}
	if !lexistsT(p.NewStateDir) {
		t.Fatal("state not moved")
	}
}

func TestLegacyMigrationIgnoresStalePIDFile(t *testing.T) {
	for name, procs := range map[string]map[int]string{
		"no process":       {},
		"pid reused":       {4242: "/usr/bin/bash"},
		"trinetra":         {4242: "/usr/local/bin/trinetra"},
		"serverwatch-like": {4242: "/usr/local/bin/serverwatch-web"},
	} {
		t.Run(name, func(t *testing.T) {
			p := testMigrationPaths(t)
			makeLegacyInstall(t, p)
			writeLegacyPID(t, p, 4242)
			f := &fakeMigrationOps{paths: p, procs: procs}
			if _, err := runMigration(t, f); err != nil {
				t.Fatalf("stale pid file blocked the migration: %v", err)
			}
		})
	}
}

func TestLegacyMigrationStrayDaemonForce(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	writeLegacyPID(t, p, 4242)
	f := &fakeMigrationOps{paths: p, procs: map[int]string{4242: "/usr/local/bin/serverwatch"}}
	plan, err := planLegacyMigration(p, planOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan.force = true
	sum, err := applyLegacyMigration(plan, f, f.install)
	if err != nil {
		t.Fatalf("with --force: %v", err)
	}
	if !strings.Contains(sum.String(), "pid 4242") || !strings.Contains(sum.String(), "--force") {
		t.Errorf("summary lacks the stray-daemon force note:\n%s", sum)
	}
}

// --- the guard ignores empty legacy dirs; CLI writes run it ---

func TestLegacyDaemonGuardIgnoresEmptyLegacyDirs(t *testing.T) {
	p := testMigrationPaths(t)
	restoreGlobals(t)
	cfgPath = filepath.Join(p.NewConfigDir, "config.json")
	stateDir = p.NewStateDir
	for _, d := range []string{p.OldConfigDir, p.OldStateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacyInstallGuard(p); err != nil {
		t.Fatalf("empty legacy dirs refused: %v", err)
	}
	mustWrite(t, filepath.Join(p.OldStateDir, "status.json"), "{}")
	err := legacyInstallGuard(p)
	if err == nil {
		t.Fatal("legacy state with data not refused")
	}
	if strings.Contains(err.Error(), p.OldConfigDir) {
		t.Errorf("guard names the empty config dir: %v", err)
	}
}

func legacyOnlyHost(t *testing.T) migrationPaths {
	t.Helper()
	p := testMigrationPaths(t)
	restoreGlobals(t)
	legacyRoot = filepath.Dir(filepath.Dir(p.OldConfigDir))
	cfgPath = filepath.Join(p.NewConfigDir, "config.json")
	stateDir = p.NewStateDir
	makeLegacyInstall(t, p)
	return p
}

func TestCLIWritesRefuseOnLegacyOnlyHost(t *testing.T) {
	for _, args := range [][]string{
		{"config", "set", "server.name", "x"},
		{"config", "unset", "server.name"},
		{"telegram", "set-token", "1:abc"},
		{"monitor", "enable", "cpu"},
		{"monitor", "disable", "cpu"},
		{"monitor", "threshold", "cpu", "90"},
		{"schedule", "off"},
		{"quiet-hours", "off"},
		{"healthchecks", "off"},
		{"channel", "add", "webhook", "--url", "https://example.invalid/x"},
		{"channel", "remove", "x"},
		{"channel", "set", "x", "enabled", "false"},
		{"downtime", "purge"},
		{"alerts", "ack", "k"},
		{"alerts", "unack", "k"},
		{"migrate"},
		{"fleet", "init", "--address", "h"},
		{"fleet", "join", "code"},
		{"fleet", "leave"},
		{"fleet", "disable"},
		{"doctor"},
		{"dump", "--metric", "cpu"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			p := legacyOnlyHost(t)
			var errb, outb bytes.Buffer
			stderr, stdout = &errb, &outb
			if code := Main(args); code != 1 {
				t.Errorf("exit %d, want 1 (stderr %q)", code, errb.String())
			}
			if !strings.Contains(errb.String(), "found a serverwatch install at") ||
				!strings.Contains(errb.String(), "run `sudo trinetra install` first") {
				t.Errorf("stderr = %q", errb.String())
			}
			if lexistsT(p.NewConfigDir) || lexistsT(p.NewStateDir) {
				t.Error("the command created trinetra paths on a legacy-only host")
			}
		})
	}
}

func TestCLIReadsWorkOnLegacyOnlyHost(t *testing.T) {
	for _, args := range [][]string{
		{"config", "get"},
		{"config", "get", "server.name"},
		{"channel", "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			p := legacyOnlyHost(t)
			var errb, outb bytes.Buffer
			stderr, stdout = &errb, &outb
			if code := Main(args); code != 0 {
				t.Errorf("exit %d, want 0 (stderr %q)", code, errb.String())
			}
			if strings.Contains(errb.String(), "serverwatch install") {
				t.Errorf("read-only command was guarded: %q", errb.String())
			}
			if lexistsT(p.NewConfigDir) || lexistsT(p.NewStateDir) {
				t.Error("a read-only command created trinetra paths")
			}
		})
	}
}

func TestCLIWritesWorkOnFreshHost(t *testing.T) {
	p := testMigrationPaths(t)
	restoreGlobals(t)
	legacyRoot = filepath.Dir(filepath.Dir(p.OldConfigDir))
	cfgPath = filepath.Join(p.NewConfigDir, "config.json")
	stateDir = p.NewStateDir
	var errb, outb bytes.Buffer
	stderr, stdout = &errb, &outb
	if code := Main([]string{"config", "set", "server.name", "box"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !lexistsT(cfgPath) {
		t.Fatal("config not written on a fresh host")
	}
}

// --- a symlinked legacy dir moves, and a resume recognises it ---

// symlinkedLegacyState makes OldStateDir a symlink to a real dir elsewhere
// (e.g. /var/lib/serverwatch -> /data/sw) and returns that dir.
func symlinkedLegacyState(t *testing.T, p migrationPaths) string {
	t.Helper()
	data := filepath.Join(filepath.Dir(filepath.Dir(p.OldConfigDir)), "data", "sw")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p.OldStateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(data, p.OldStateDir); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLegacyMigrationSymlinkedStateDir(t *testing.T) {
	for _, stateOnly := range []bool{false, true} {
		for _, step := range append([]string{""}, migrationSteps...) {
			t.Run(fmt.Sprintf("stateOnly=%v/crash=%s", stateOnly, step), func(t *testing.T) {
				p := testMigrationPaths(t)
				data := symlinkedLegacyState(t, p)
				makeLegacyInstall(t, p)
				if stateOnly {
					if err := os.RemoveAll(p.OldConfigDir); err != nil {
						t.Fatal(err)
					}
				}
				want := snapshot(t, data)
				f := &fakeMigrationOps{paths: p, failStep: step}
				if step != "" {
					if _, err := runMigration(t, f); err == nil {
						t.Fatalf("expected a simulated crash at %s", step)
					}
					f.failStep = ""
				}
				sum, err := runMigration(t, f)
				if err != nil {
					t.Fatalf("migration: %v", err)
				}
				if sum == nil {
					t.Fatal("the re-run did not resume the interrupted migration")
				}
				if !strings.Contains(sum.String(), fmt.Sprintf("moved %s -> %s", p.OldStateDir, p.NewStateDir)) {
					t.Errorf("summary lacks the moved line:\n%s", sum)
				}
				if lexistsT(p.OldStateDir) {
					t.Error("old state path still exists")
				}
				if l, err := os.Readlink(p.NewStateDir); err != nil || l != data {
					t.Errorf("%s -> %q (%v), want the moved symlink -> %s", p.NewStateDir, l, err, data)
				}
				got := snapshot(t, data)
				if !lexistsT(filepath.Join(data, migratedFromServerwatchMarker)) {
					t.Error("migrated marker not written")
				}
				delete(got, migratedFromServerwatchMarker)
				if !equalSnap(got, want) {
					t.Errorf("data differs (markers left?)\n got: %v\nwant: %v", got, want)
				}
				for _, path := range []string{p.OldUnit, p.OldCtl, p.OldWeb} {
					if lexistsT(path) {
						t.Errorf("%s left behind", path)
					}
				}
				if l, err := os.Readlink(p.OldBin); err != nil || l != p.NewBin {
					t.Errorf("compat link %s -> %q (%v)", p.OldBin, l, err)
				}
				if plan, err := planLegacyMigration(p, planOptions{}); err != nil || plan != nil {
					t.Errorf("re-run after success: plan=%v err=%v", plan, err)
				}
			})
		}
	}
}

// A new path that is a symlink to someone else's dir still refuses, even
// when that dir carries a copy-verified marker with the source's token.
func TestLegacyMigrationRefusesForeignSymlinkedNewDir(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(fmt.Sprintf("forgedMarker=%v", forged), func(t *testing.T) {
			p := testMigrationPaths(t)
			makeLegacyInstall(t, p)
			wantState := snapshot(t, p.OldStateDir)
			f := &fakeMigrationOps{paths: p, failStep: "move-state"}
			if _, err := runMigration(t, f); err == nil {
				t.Fatal("expected simulated crash")
			}
			f.failStep = ""
			other := filepath.Join(t.TempDir(), "other")
			mustWrite(t, filepath.Join(other, "status.json"), "{}")
			if forged {
				tok, err := readMarkerToken(filepath.Join(p.OldStateDir, migratingMarker))
				if err != nil || tok == "" {
					t.Fatalf("no token: %v", err)
				}
				mustWrite(t, filepath.Join(other, copyVerifiedMarker), tok+"\n")
			}
			if err := os.MkdirAll(filepath.Dir(p.NewStateDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, p.NewStateDir); err != nil {
				t.Fatal(err)
			}
			if _, err := runMigration(t, f); err == nil {
				t.Fatal("expected a refusal for a foreign symlinked new dir")
			}
			assertLegacyIntact(t, p, wantState)
		})
	}
}
