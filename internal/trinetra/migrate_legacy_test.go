package trinetra

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// fakeMigrationOps stands in for systemctl, the primary directory rename and
// chown, so the migration runs against a temp root without root privileges.
type fakeMigrationOps struct {
	paths     migrationPaths
	exdev     bool   // primary old->new directory renames fail with EXDEV
	ebusy     bool   // primary old->new directory renames fail with EBUSY
	isActive  string // what `systemctl is-active serverwatch` prints
	failStep  string // checkpoint that fails (simulated crash)
	calls     [][]string
	chowns    map[string][2]int
	installed int
}

func (f *fakeMigrationOps) Systemctl(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if len(args) > 0 && args[0] == "is-active" {
		out := f.isActive
		if out == "" {
			out = "inactive"
		}
		if out == "active" {
			return out + "\n", nil
		}
		return out + "\n", errors.New("exit status 3")
	}
	return "", nil
}

func (f *fakeMigrationOps) Rename(oldpath, newpath string) error {
	if f.exdev && (oldpath == f.paths.OldConfigDir || oldpath == f.paths.OldStateDir) {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	if f.ebusy && (oldpath == f.paths.OldConfigDir || oldpath == f.paths.OldStateDir) {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EBUSY}
	}
	return os.Rename(oldpath, newpath)
}

func (f *fakeMigrationOps) Lchown(path string, uid, gid int) error {
	if f.chowns == nil {
		f.chowns = map[string][2]int{}
	}
	f.chowns[path] = [2]int{uid, gid}
	return os.Lchown(path, uid, gid)
}

func (f *fakeMigrationOps) Checkpoint(step string) error {
	if f.failStep != "" && step == f.failStep {
		return fmt.Errorf("simulated crash before %s", step)
	}
	return nil
}

// install is the fake "normal install": it only drops the new binary.
func (f *fakeMigrationOps) install() error {
	f.installed++
	return os.WriteFile(f.paths.NewBin, []byte("TRINETRA"), 0o755)
}

const legacyTestConfig = `{
  "server_name": "box",
  "telegram": {"token": "123:abc", "chat_id": "-100200300400500600"},
  "web": {
    "enabled": true,
    "mode": "manual",
    "tls_cert": "/etc/serverwatch/tls/cert.pem",
    "tls_key": "/var/lib/serverwatch/tls/key.pem"
  },
  "future_key": {"kept": [1, 2, 3]}
}
`

// makeLegacyInstall lays out a serverwatch install under root.
func makeLegacyInstall(t *testing.T, p migrationPaths) {
	t.Helper()
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(p.OldConfigDir, "config.json"), legacyTestConfig, 0o600)
	write(filepath.Join(p.OldConfigDir, "tls", "cert.pem"), "CERT", 0o644)
	write(filepath.Join(p.OldStateDir, "plugins.json"), `{"ctl":"aa"}`, 0o600)
	write(filepath.Join(p.OldStateDir, "status.json"), `{"ok":true}`, 0o644)
	write(filepath.Join(p.OldStateDir, "samples", "cpu.dat"), strings.Repeat("x", 70000), 0o644)
	write(filepath.Join(p.OldStateDir, "fleet", "pki", "ca.key"), "KEY", 0o600)
	write(filepath.Join(p.OldStateDir, "tls", "key.pem"), "TLSKEY", 0o600)
	if err := os.Chmod(filepath.Join(p.OldStateDir, "fleet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("samples/cpu.dat", filepath.Join(p.OldStateDir, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(legacyStateDirPath+"/samples/cpu.dat", filepath.Join(p.OldStateDir, "abs-link")); err != nil {
		t.Fatal(err)
	}
	write(p.OldUnit, "[Service]\nExecStart=/usr/local/bin/serverwatch daemon\n", 0o644)
	write(p.OldBin, "OLD-SERVERWATCH", 0o755)
	write(p.OldCtl, "OLD-CTL", 0o755)
	write(p.OldWeb, "OLD-WEB", 0o755)
	if err := os.MkdirAll(filepath.Dir(p.OldUsrBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p.OldBin, p.OldUsrBin); err != nil {
		t.Fatal(err)
	}
}

// snapshot records every entry under dir (relative path -> type/mode/size/
// content or link target), markers included.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return out
	}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		desc := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			l, _ := os.Readlink(path)
			desc += " -> " + l
		case info.Mode().IsRegular():
			b, _ := os.ReadFile(path)
			desc += fmt.Sprintf(" %d %x", len(b), b[:min(len(b), 16)])
		}
		out[rel] = desc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lexistsT(path string) bool { _, err := os.Lstat(path); return err == nil }

func testMigrationPaths(t *testing.T) migrationPaths {
	t.Helper()
	root := t.TempDir()
	orig := isOurLegacyBinaryFn
	isOurLegacyBinaryFn = func(path string) bool {
		b, err := os.ReadFile(path)
		return err == nil && string(b) == "OLD-SERVERWATCH"
	}
	t.Cleanup(func() { isOurLegacyBinaryFn = orig })
	return migrationPathsAt(root)
}

// runMigration plans and applies once, the way cmdInstall does.
func runMigration(t *testing.T, f *fakeMigrationOps) (*migrationSummary, error) {
	t.Helper()
	plan, err := planLegacyMigration(f.paths)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, nil
	}
	return applyLegacyMigration(plan, f, f.install)
}

// assertMigrated checks the complete post-migration state against the
// snapshots of the legacy dirs taken before migrating.
func assertMigrated(t *testing.T, p migrationPaths, wantCfg, wantState map[string]string) {
	t.Helper()
	for _, old := range []string{p.OldConfigDir, p.OldStateDir, p.OldUnit, p.OldCtl, p.OldWeb,
		p.NewConfigDir + stagingSuffix, p.NewStateDir + stagingSuffix} {
		if lexistsT(old) {
			t.Errorf("%s still exists after migration", old)
		}
	}
	gotState := snapshot(t, p.NewStateDir)
	delete(gotState, migratedFromServerwatchMarker)
	if !equalSnap(gotState, wantState) {
		t.Errorf("state dir content differs\n got: %v\nwant: %v", gotState, wantState)
	}
	gotCfg := snapshot(t, p.NewConfigDir)
	// config.json is rewritten (tls paths), compare everything else.
	delete(gotCfg, "config.json")
	wc := copySnap(wantCfg)
	delete(wc, "config.json")
	if !equalSnap(gotCfg, wc) {
		t.Errorf("config dir content differs\n got: %v\nwant: %v", gotCfg, wc)
	}
	b, err := os.ReadFile(filepath.Join(p.NewConfigDir, "config.json"))
	if err != nil {
		t.Fatalf("config not moved: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("rewritten config is not JSON: %v\n%s", err, b)
	}
	web := cfg["web"].(map[string]any)
	if web["tls_cert"] != "/etc/trinetra/tls/cert.pem" || web["tls_key"] != "/var/lib/trinetra/tls/key.pem" {
		t.Errorf("tls paths not rewritten: %v", web)
	}
	if cfg["future_key"] == nil || !strings.Contains(string(b), `"-100200300400500600"`) {
		t.Errorf("config rewrite lost data:\n%s", b)
	}
	if fi, _ := os.Stat(filepath.Join(p.NewConfigDir, "config.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", fi.Mode().Perm())
	}
	if l, err := os.Readlink(p.OldBin); err != nil || l != p.NewBin {
		t.Errorf("compat symlink %s -> %q (err %v), want -> %s", p.OldBin, l, err, p.NewBin)
	}
	if b, err := os.ReadFile(filepath.Join(p.NewStateDir, migratedFromServerwatchMarker)); err != nil || len(bytes.TrimSpace(b)) == 0 {
		t.Errorf("migrated marker missing/empty: %v", err)
	}
	if l, err := os.Readlink(p.OldUsrBin); err != nil || l != p.OldBin {
		t.Errorf("%s -> %q (err %v), want the compat link kept -> %s", p.OldUsrBin, l, err, p.OldBin)
	}
	for _, d := range []string{p.NewConfigDir, p.NewStateDir} {
		for _, m := range []string{migratingMarker, copyVerifiedMarker} {
			if lexistsT(filepath.Join(d, m)) {
				t.Errorf("marker %s left in %s", m, d)
			}
		}
	}
}

func equalSnap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func copySnap(m map[string]string) map[string]string {
	o := make(map[string]string, len(m))
	for k, v := range m {
		o[k] = v
	}
	return o
}

func TestLegacyMigrationMovesEverything(t *testing.T) {
	for _, exdev := range []bool{false, true} {
		t.Run(fmt.Sprintf("exdev=%v", exdev), func(t *testing.T) {
			p := testMigrationPaths(t)
			makeLegacyInstall(t, p)
			wantCfg, wantState := snapshot(t, p.OldConfigDir), snapshot(t, p.OldStateDir)
			f := &fakeMigrationOps{paths: p, exdev: exdev}

			sum, err := runMigration(t, f)
			if err != nil {
				t.Fatalf("migration failed: %v", err)
			}
			if sum == nil {
				t.Fatal("legacy install present but no migration ran")
			}
			assertMigrated(t, p, wantCfg, wantState)
			if f.installed != 1 {
				t.Errorf("normal install ran %d times, want 1", f.installed)
			}
			var sc []string
			for _, c := range f.calls {
				sc = append(sc, strings.Join(c, " "))
			}
			for _, want := range []string{"stop serverwatch", "disable serverwatch", "is-active serverwatch"} {
				if !strings.Contains(strings.Join(sc, "|"), want) {
					t.Errorf("systemctl calls %v missing %q", sc, want)
				}
			}
			if exdev {
				// every copied entry had its ownership set explicitly
				for rel := range wantState {
					if _, ok := f.chowns[filepath.Join(p.NewStateDir+stagingSuffix, rel)]; !ok {
						t.Errorf("no chown recorded for state entry %q", rel)
					}
				}
				if !strings.Contains(sum.String(), "copied") {
					t.Errorf("summary does not mention the copy:\n%s", sum)
				}
			}
			if !strings.Contains(sum.String(), "abs-link -> /var/lib/serverwatch/samples/cpu.dat") {
				t.Errorf("summary does not warn about the symlink into the old path:\n%s", sum)
			}
			if !strings.Contains(sum.String(), p.NewStateDir) || !strings.Contains(sum.String(), "web.tls_cert") {
				t.Errorf("summary incomplete:\n%s", sum)
			}

			// Idempotent: a re-run after success is a plain install.
			plan, err := planLegacyMigration(p)
			if err != nil || plan != nil {
				t.Fatalf("re-run after success: plan=%v err=%v, want nil/nil", plan, err)
			}
		})
	}
}

func TestLegacyMigrationRefusesWhenBothPresent(t *testing.T) {
	cases := map[string]func(p migrationPaths) string{
		"new config dir":  func(p migrationPaths) string { return filepath.Join(p.NewConfigDir, "config.json") },
		"new state dir":   func(p migrationPaths) string { return filepath.Join(p.NewStateDir, "plugins.json") },
		"new state, only": func(p migrationPaths) string { return filepath.Join(p.NewStateDir, "x") },
	}
	for name, newFile := range cases {
		t.Run(name, func(t *testing.T) {
			p := testMigrationPaths(t)
			makeLegacyInstall(t, p)
			mustWrite(t, newFile(p), "{}")
			root := filepath.Dir(filepath.Dir(p.OldConfigDir)) // temp root
			before := snapshot(t, root)

			plan, err := planLegacyMigration(p)
			if err == nil {
				t.Fatalf("expected refusal, got plan %v", plan)
			}
			for _, want := range []string{"refusing to merge", "Nothing was changed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q missing %q", err, want)
				}
			}
			if after := snapshot(t, root); !equalSnap(before, after) {
				t.Errorf("planning changed the filesystem")
			}
		})
	}
}

func TestLegacyMigrationNoLegacyIsNoop(t *testing.T) {
	p := testMigrationPaths(t)
	plan, err := planLegacyMigration(p)
	if err != nil || plan != nil {
		t.Fatalf("fresh host: plan=%v err=%v, want nil/nil", plan, err)
	}
	// An existing trinetra install alone is not a migration either.
	mustWrite(t, filepath.Join(p.NewConfigDir, "config.json"), "{}")
	mustWrite(t, filepath.Join(p.NewStateDir, "plugins.json"), "{}")
	plan, err = planLegacyMigration(p)
	if err != nil || plan != nil {
		t.Fatalf("trinetra-only host: plan=%v err=%v, want nil/nil", plan, err)
	}
}

// TestLegacyMigrationResumesAfterCrashAtEveryStep stops the migration at
// each step boundary (as a crash would), then re-runs install: the end state
// must be exactly the clean one, with no data lost.
func TestLegacyMigrationResumesAfterCrashAtEveryStep(t *testing.T) {
	for _, exdev := range []bool{false, true} {
		for _, step := range migrationSteps {
			t.Run(fmt.Sprintf("exdev=%v/%s", exdev, step), func(t *testing.T) {
				p := testMigrationPaths(t)
				makeLegacyInstall(t, p)
				wantCfg, wantState := snapshot(t, p.OldConfigDir), snapshot(t, p.OldStateDir)

				f := &fakeMigrationOps{paths: p, exdev: exdev, failStep: step}
				_, err := runMigration(t, f)
				var me *migrationError
				if !errors.As(err, &me) {
					t.Fatalf("expected a migrationError at %s, got %v", step, err)
				}
				msg := err.Error()
				for _, want := range []string{step, "sudo trinetra install", "Current state", "roll back"} {
					if !strings.Contains(msg, want) {
						t.Errorf("failure report missing %q:\n%s", want, msg)
					}
				}

				f.failStep = ""
				if _, err := runMigration(t, f); err != nil {
					t.Fatalf("re-run after crash at %s failed: %v", step, err)
				}
				assertMigrated(t, p, wantCfg, wantState)
			})
		}
	}
}

// A copy that died half-way leaves a partial staging dir next to an intact
// source; the re-run discards the partial copy and copies again.
func TestLegacyMigrationDiscardsPartialCopy(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantCfg, wantState := snapshot(t, p.OldConfigDir), snapshot(t, p.OldStateDir)
	mustWrite(t, filepath.Join(p.NewStateDir+stagingSuffix, "samples", "cpu.dat"), "trunc")

	f := &fakeMigrationOps{paths: p, exdev: true}
	if _, err := runMigration(t, f); err != nil {
		t.Fatal(err)
	}
	assertMigrated(t, p, wantCfg, wantState)
}

// A crash while removing the verified source leaves the new dir complete and
// the old one partly deleted (its marker is removed last). The re-run finishes
// the removal and keeps the new copy.
func TestLegacyMigrationFinishesSourceRemoval(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantCfg, wantState := snapshot(t, p.OldConfigDir), snapshot(t, p.OldStateDir)
	f := &fakeMigrationOps{paths: p, exdev: true, failStep: "rewrite-config"}
	if _, err := runMigration(t, f); err == nil {
		t.Fatal("expected simulated crash")
	}
	// Recreate a partly-deleted source: its marker (same run token as the
	// verified copy) plus one leftover file.
	tok, err := readMarkerToken(filepath.Join(p.NewStateDir, copyVerifiedMarker))
	if err != nil || tok == "" {
		t.Fatalf("verified copy has no token: %v", err)
	}
	mustWrite(t, filepath.Join(p.OldStateDir, migratingMarker), tok+"\n")
	mustWrite(t, filepath.Join(p.OldStateDir, "samples", "cpu.dat"), strings.Repeat("x", 70000))
	if err := os.Chmod(filepath.Join(p.OldStateDir, "samples", "cpu.dat"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.failStep = ""
	if _, err := runMigration(t, f); err != nil {
		t.Fatal(err)
	}
	assertMigrated(t, p, wantCfg, wantState)
}

// Without our marker a leftover old dir next to a migrated new dir is not
// provably a copy we made: refuse instead of deleting it.
func TestLegacyMigrationRefusesUnmarkedLeftover(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	f := &fakeMigrationOps{paths: p, failStep: "rewrite-config"}
	if _, err := runMigration(t, f); err == nil {
		t.Fatal("expected simulated crash")
	}
	mustWrite(t, filepath.Join(p.OldStateDir, "important"), "data")
	if _, err := planLegacyMigration(p); err == nil {
		t.Fatal("expected refusal for an unmarked old dir next to a migrated new dir")
	}
	if !lexistsT(filepath.Join(p.OldStateDir, "important")) {
		t.Fatal("unmarked old data was removed")
	}
}

func TestLegacyMigrationRefusesWhileOldServiceRuns(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantState := snapshot(t, p.OldStateDir)
	f := &fakeMigrationOps{paths: p, isActive: "active"}
	_, err := runMigration(t, f)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("expected a still-running error, got %v", err)
	}
	if got := snapshot(t, p.OldStateDir); !equalSnap(got, wantState) || lexistsT(p.NewStateDir) {
		t.Fatalf("state moved although the old daemon was running")
	}
	if f.installed != 0 {
		t.Fatal("install ran after a failed migration step")
	}
}

func TestLegacyMigrationCopyFailureKeepsSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files")
	}
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	unreadable := filepath.Join(p.OldStateDir, "samples", "cpu.dat")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	wantState := snapshot(t, p.OldStateDir)

	f := &fakeMigrationOps{paths: p, exdev: true}
	if _, err := runMigration(t, f); err == nil {
		t.Fatal("expected copy failure")
	}
	got := snapshot(t, p.OldStateDir)
	if _, ok := got[migratingMarker]; !ok {
		t.Error("source lost its in-progress marker")
	}
	delete(got, migratingMarker)
	if !equalSnap(got, wantState) {
		t.Fatal("source changed after a failed copy")
	}
	if lexistsT(p.NewStateDir) || lexistsT(p.NewStateDir+stagingSuffix) {
		t.Fatal("partial copy left in place")
	}
}

func TestCopyTreeVerifiedPreservesModesAndLinks(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	src := p.OldStateDir
	dst := filepath.Join(t.TempDir(), "copy")
	f := &fakeMigrationOps{paths: p}
	files, size, err := copyTreeVerified(src, dst, f)
	if err != nil {
		t.Fatal(err)
	}
	if want := snapshot(t, src); !equalSnap(snapshot(t, dst), want) {
		t.Fatalf("copy differs\n got %v\nwant %v", snapshot(t, dst), want)
	}
	if files != 7 || size != int64(len(`{"ctl":"aa"}`)+len(`{"ok":true}`)+70000+len("KEY")+len("TLSKEY")) {
		t.Errorf("files=%d size=%d", files, size)
	}
	uid, gid := os.Getuid(), os.Getgid()
	var paths []string
	for path, ids := range f.chowns {
		paths = append(paths, path)
		if ids != [2]int{uid, gid} {
			t.Errorf("chown %s to %v, want %d:%d", path, ids, uid, gid)
		}
	}
	sort.Strings(paths)
	if _, ok := f.chowns[filepath.Join(dst, "current")]; !ok {
		t.Errorf("symlink ownership not set; chowns: %v", paths)
	}
}

func TestRewriteLegacyConfigPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	f := &fakeMigrationOps{}

	mustWrite(t, path, legacyTestConfig)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := rewriteLegacyConfigPaths(path, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Errorf("changed = %v, want tls_cert and tls_key", changed)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{`"/etc/trinetra/tls/cert.pem"`, `"/var/lib/trinetra/tls/key.pem"`, `"123:abc"`, `"future_key"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("rewritten config missing %s:\n%s", want, b)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}

	// Second pass: nothing left to rewrite, file untouched byte for byte.
	before, _ := os.ReadFile(path)
	changed, err = rewriteLegacyConfigPaths(path, f)
	if err != nil || len(changed) != 0 {
		t.Fatalf("second pass changed=%v err=%v", changed, err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("idempotent pass rewrote the file")
	}

	// Paths elsewhere, or merely sharing a name prefix, are left alone.
	mustWrite(t, path, `{"web":{"tls_cert":"/etc/serverwatchX/c.pem","tls_key":"/opt/k.pem"}}`)
	if changed, err := rewriteLegacyConfigPaths(path, f); err != nil || len(changed) != 0 {
		t.Fatalf("unrelated paths: changed=%v err=%v", changed, err)
	}

	// Missing config: nothing to do.
	if changed, err := rewriteLegacyConfigPaths(filepath.Join(dir, "none.json"), f); err != nil || changed != nil {
		t.Fatalf("missing config: changed=%v err=%v", changed, err)
	}
}

func TestRewriteLegacyPath(t *testing.T) {
	for in, want := range map[string]string{
		"/etc/serverwatch/tls/c.pem":  "/etc/trinetra/tls/c.pem",
		"/var/lib/serverwatch/k.pem":  "/var/lib/trinetra/k.pem",
		"/etc/serverwatch":            "/etc/trinetra",
		"/etc/serverwatch-old/c.pem":  "/etc/serverwatch-old/c.pem",
		"/srv/etc/serverwatch/c.pem":  "/srv/etc/serverwatch/c.pem",
		"":                            "",
		"relative/etc/serverwatch/x":  "relative/etc/serverwatch/x",
		"/var/lib/serverwatchdog/x.p": "/var/lib/serverwatchdog/x.p",
	} {
		if got := rewriteLegacyPath(in); got != want {
			t.Errorf("rewriteLegacyPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLegacyDaemonGuard(t *testing.T) {
	cases := []struct {
		name             string
		oldCfg, oldState bool
		newCfg, newState bool
		wantRefuse       bool
	}{
		{name: "fresh install", wantRefuse: false},
		{name: "legacy only", oldCfg: true, oldState: true, wantRefuse: true},
		{name: "legacy state only", oldState: true, wantRefuse: true},
		{name: "legacy config only", oldCfg: true, wantRefuse: true},
		{name: "new only", newCfg: true, newState: true},
		{name: "new state only", newState: true},
		{name: "both", oldCfg: true, oldState: true, newCfg: true, newState: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testMigrationPaths(t)
			restoreGlobals(t)
			cfgPath = filepath.Join(p.NewConfigDir, "config.json")
			stateDir = p.NewStateDir
			if tc.oldCfg {
				mustWrite(t, filepath.Join(p.OldConfigDir, "config.json"), "{}")
			}
			if tc.oldState {
				mustWrite(t, filepath.Join(p.OldStateDir, "status.json"), "{}")
			}
			if tc.newCfg {
				mustWrite(t, cfgPath, "{}")
			}
			if tc.newState {
				mustWrite(t, filepath.Join(stateDir, "status.json"), "{}")
			}
			err := legacyInstallGuard(p)
			if (err != nil) != tc.wantRefuse {
				t.Fatalf("guard err = %v, wantRefuse %v", err, tc.wantRefuse)
			}
			if err != nil && !strings.Contains(err.Error(), "run `sudo trinetra install` to migrate") {
				t.Errorf("guard message lacks the hint: %v", err)
			}
		})
	}
}

func TestDaemonRefusesToStartOnLegacyOnlyHost(t *testing.T) {
	p := testMigrationPaths(t)
	restoreGlobals(t)
	legacyRoot = filepath.Dir(filepath.Dir(p.OldConfigDir))
	cfgPath = filepath.Join(p.NewConfigDir, "config.json")
	stateDir = p.NewStateDir
	mustWrite(t, filepath.Join(p.OldConfigDir, "config.json"), "{}")
	var errb bytes.Buffer
	stderr = &errb

	if code := cmdDaemon(nil); code == 0 {
		t.Fatal("daemon started on a legacy-only host")
	}
	if !strings.Contains(errb.String(), "found a serverwatch install at "+p.OldConfigDir) {
		t.Errorf("stderr = %q", errb.String())
	}
	if lexistsT(stateDir) {
		t.Error("daemon created the new state dir before refusing")
	}
}

func TestInvokedAsServerwatchPrintsNotice(t *testing.T) {
	restoreGlobals(t)
	for _, tc := range []struct {
		argv0  string
		notice bool
	}{
		{"/usr/local/bin/serverwatch", true},
		{"serverwatch", true},
		{"/usr/local/bin/trinetra", false},
	} {
		argv0 = func() string { return tc.argv0 }
		var errb, outb bytes.Buffer
		stderr, stdout = &errb, &outb
		if code := Main([]string{"help"}); code != 0 {
			t.Fatalf("%s help exit=%d", tc.argv0, code)
		}
		got := strings.Contains(errb.String(), "serverwatch is now trinetra; this name will be removed in the next release")
		if got != tc.notice {
			t.Errorf("argv0 %q: notice printed=%v, want %v (stderr %q)", tc.argv0, got, tc.notice, errb.String())
		}
		if outb.Len() == 0 {
			t.Errorf("argv0 %q: command did not run", tc.argv0)
		}
	}
}

// restoreGlobals puts back the package-level seams these tests override.
func restoreGlobals(t *testing.T) {
	t.Helper()
	oc, os_, oo, oe, lr, a0 := cfgPath, stateDir, stdout, stderr, legacyRoot, argv0
	t.Cleanup(func() {
		cfgPath, stateDir, stdout, stderr, legacyRoot, argv0 = oc, os_, oo, oe, lr, a0
	})
}

// As root, the real ops must carry a foreign uid/gid (and setgid bits) over.
func TestCopyTreeVerifiedPreservesForeignOwnershipAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown to another uid")
	}
	src := filepath.Join(t.TempDir(), "src")
	mustWrite(t, filepath.Join(src, "sub", "f"), "data")
	if err := os.Symlink("sub/f", filepath.Join(src, "l")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{src, filepath.Join(src, "sub"), filepath.Join(src, "sub", "f"), filepath.Join(src, "l")} {
		if err := os.Lchown(p, 1234, 5678); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(src, "sub"), 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	if _, _, err := copyTreeVerified(src, dst, osMigrationOps{}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".", "sub", "sub/f", "l"} {
		fi, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if st.Uid != 1234 || st.Gid != 5678 {
			t.Errorf("%s owner %d:%d, want 1234:5678", rel, st.Uid, st.Gid)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dst, "sub")); fi.Mode()&os.ModeSetgid == 0 || fi.Mode().Perm() != 0o750 {
		t.Errorf("sub mode = %v, want setgid 0750", fi.Mode())
	}
}

// assertLegacyIntact: the legacy state dir still holds all its data (the
// in-progress marker may have been added).
func assertLegacyIntact(t *testing.T, p migrationPaths, want map[string]string) {
	t.Helper()
	got := snapshot(t, p.OldStateDir)
	delete(got, migratingMarker)
	if !equalSnap(got, want) {
		t.Fatalf("legacy state dir changed\n got %v\nwant %v", got, want)
	}
}

// Review repro A: crash at move-state, then the operator starts a manual copy
// (cp -a) that brings the in-progress marker and one file into the new dir.
// The re-run must refuse and delete nothing.
func TestLegacyMigrationRefusesManualPartialCopy(t *testing.T) {
	for _, exdev := range []bool{false, true} {
		t.Run(fmt.Sprintf("exdev=%v", exdev), func(t *testing.T) {
			p := testMigrationPaths(t)
			makeLegacyInstall(t, p)
			wantState := snapshot(t, p.OldStateDir)
			f := &fakeMigrationOps{paths: p, exdev: exdev, failStep: "move-state"}
			if _, err := runMigration(t, f); err == nil {
				t.Fatal("expected simulated crash")
			}
			b, _ := os.ReadFile(filepath.Join(p.OldStateDir, migratingMarker))
			mustWrite(t, filepath.Join(p.NewStateDir, migratingMarker), string(b))
			mustWrite(t, filepath.Join(p.NewStateDir, "status.json"), "{}")
			root := filepath.Dir(filepath.Dir(p.OldConfigDir))
			before := snapshot(t, root)

			f.failStep = ""
			_, err := runMigration(t, f)
			if err == nil || !strings.Contains(err.Error(), "not a verified copy") {
				t.Fatalf("expected refusal, got %v", err)
			}
			if after := snapshot(t, root); !equalSnap(before, after) {
				t.Fatal("refused run changed the filesystem")
			}
			assertLegacyIntact(t, p, wantState)
		})
	}
}

// A forged or stale copy-verified marker whose token does not match the
// source's marker never authorises deleting the source.
func TestLegacyMigrationRefusesMismatchedVerifiedMarker(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantState := snapshot(t, p.OldStateDir)
	f := &fakeMigrationOps{paths: p, exdev: true, failStep: "move-state"}
	if _, err := runMigration(t, f); err == nil {
		t.Fatal("expected simulated crash")
	}
	mustWrite(t, filepath.Join(p.NewStateDir, copyVerifiedMarker), "not-the-token\n")
	mustWrite(t, filepath.Join(p.NewStateDir, "status.json"), "{}")
	f.failStep = ""
	if _, err := runMigration(t, f); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected token refusal, got %v", err)
	}
	assertLegacyIntact(t, p, wantState)
}

// A matching token is still not enough when the new dir lacks some of the
// source's data (e.g. the operator copied the marker by hand).
func TestLegacyMigrationRefusesIncompleteVerifiedCopy(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantState := snapshot(t, p.OldStateDir)
	f := &fakeMigrationOps{paths: p, exdev: true, failStep: "move-state"}
	if _, err := runMigration(t, f); err == nil {
		t.Fatal("expected simulated crash")
	}
	tok, _ := readMarkerToken(filepath.Join(p.OldStateDir, migratingMarker))
	mustWrite(t, filepath.Join(p.NewStateDir, copyVerifiedMarker), tok+"\n")
	mustWrite(t, filepath.Join(p.NewStateDir, "status.json"), `{"ok":true}`)
	f.failStep = ""
	if _, err := runMigration(t, f); err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("expected refusal, got %v", err)
	}
	assertLegacyIntact(t, p, wantState)
}

// Review repro C: the new path is a symlink to the old dir, before any run
// and after a crash. Refuse, delete nothing.
func TestLegacyMigrationRefusesSymlinkedNewDir(t *testing.T) {
	for _, crashFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("crashFirst=%v", crashFirst), func(t *testing.T) {
			p := testMigrationPaths(t)
			makeLegacyInstall(t, p)
			wantState := snapshot(t, p.OldStateDir)
			f := &fakeMigrationOps{paths: p, exdev: true}
			if crashFirst {
				f.failStep = "move-state"
				if _, err := runMigration(t, f); err == nil {
					t.Fatal("expected simulated crash")
				}
				f.failStep = ""
			}
			if err := os.MkdirAll(filepath.Dir(p.NewStateDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(p.OldStateDir, p.NewStateDir); err != nil {
				t.Fatal(err)
			}
			if _, err := runMigration(t, f); err == nil {
				t.Fatal("expected refusal for a symlinked new dir")
			}
			assertLegacyIntact(t, p, wantState)
		})
	}
}

func TestCheckNotAliased(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	mustWrite(t, filepath.Join(old, "sub", "f"), "x")
	mustWrite(t, filepath.Join(dir, "new", "f"), "x")
	if err := checkNotAliased(old, filepath.Join(dir, "new")); err != nil {
		t.Fatalf("separate dirs reported as aliased: %v", err)
	}
	if err := os.Symlink(old, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := checkNotAliased(old, filepath.Join(dir, "link")); err == nil {
		t.Error("symlink new -> old not detected")
	}
	if err := checkNotAliased(old, old); err == nil {
		t.Error("same dir not detected")
	}
	if err := checkNotAliased(old, filepath.Join(old, "sub")); err == nil {
		t.Error("new inside old not detected")
	}
	// A path that reaches old through a symlinked parent.
	if err := checkNotAliased(old, filepath.Join(dir, "link", "sub")); err == nil {
		t.Error("new resolving inside old through a symlinked parent not detected")
	}
}

// IMPORTANT 2: a legacy dir that is a mount point is refused up front, before
// the old service is touched, with "unmount it from the old path first".
func TestLegacyMigrationRefusesMountPointUpFront(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	orig := isMountPointFn
	isMountPointFn = func(path string) bool { return path == p.OldStateDir }
	t.Cleanup(func() { isMountPointFn = orig })
	root := filepath.Dir(filepath.Dir(p.OldConfigDir))
	before := snapshot(t, root)

	_, err := planLegacyMigration(p)
	if err == nil {
		t.Fatal("expected refusal for a mount-point legacy dir")
	}
	for _, want := range []string{"mount point", "unmount the volume from " + p.OldStateDir + " first", "Nothing was changed", "Do not mount"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%v", want, err)
		}
	}
	if after := snapshot(t, root); !equalSnap(before, after) {
		t.Fatal("planning changed the filesystem")
	}
}

// Following that advice (volume now mounted at the new path, old path left
// empty) migrates the rest and keeps the volume's data where it is.
func TestLegacyMigrationAfterVolumeRemountedAtNewPath(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantState := snapshot(t, p.OldStateDir)
	if err := os.MkdirAll(filepath.Dir(p.NewStateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.OldStateDir, p.NewStateDir); err != nil { // "remount"
		t.Fatal(err)
	}
	if err := os.Mkdir(p.OldStateDir, 0o755); err != nil { // empty mount point left
		t.Fatal(err)
	}
	f := &fakeMigrationOps{paths: p}
	sum, err := runMigration(t, f)
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot(t, p.NewStateDir)
	delete(got, migratedFromServerwatchMarker)
	if !equalSnap(got, wantState) {
		t.Errorf("volume data changed\n got %v\nwant %v", got, wantState)
	}
	if lexistsT(p.OldStateDir) || lexistsT(p.OldConfigDir) {
		t.Error("old dirs left behind")
	}
	if !strings.Contains(sum.String(), "kept the existing") {
		t.Errorf("summary lacks the kept-state note:\n%s", sum)
	}
}

// An EBUSY rename (mount point not caught up front, e.g. a race) stops with
// the unmount advice and deletes nothing, on every re-run.
func TestLegacyMigrationEBUSYRerun(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantState := snapshot(t, p.OldStateDir)
	f := &fakeMigrationOps{paths: p, ebusy: true}
	for i := 0; i < 2; i++ {
		_, err := runMigration(t, f)
		if err == nil || !strings.Contains(err.Error(), "Unmount it from "+p.OldStateDir+" first") ||
			strings.Contains(err.Error(), "mount it at "+p.NewStateDir+" instead, then") {
			t.Fatalf("run %d: err = %v", i, err)
		}
		assertLegacyIntact(t, p, wantState)
		if lexistsT(p.NewStateDir) {
			t.Fatalf("run %d: new state dir created", i)
		}
	}
}

func TestLegacyMigrationNeedsForceWhenSystemctlCannotAnswer(t *testing.T) {
	for _, out := range []string{"", "garbage"} {
		p := testMigrationPaths(t)
		makeLegacyInstall(t, p)
		wantState := snapshot(t, p.OldStateDir)
		f := &fakeMigrationOps{paths: p, isActive: out}
		if out == "" {
			f.isActive = " " // trimmed to empty
		}
		_, err := runMigration(t, f)
		if err == nil || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("is-active %q: expected a --force refusal, got %v", out, err)
		}
		assertLegacyIntact(t, p, wantState)

		plan, err := planLegacyMigration(p)
		if err != nil {
			t.Fatal(err)
		}
		plan.force = true
		sum, err := applyLegacyMigration(plan, f, f.install)
		if err != nil {
			t.Fatalf("with force: %v", err)
		}
		if !strings.Contains(sum.String(), "--force") {
			t.Errorf("summary lacks the force note:\n%s", sum)
		}
	}
}

// A failed install leaves the old unit and plugins in place, so the old
// daemon stays startable (after moving the dirs back).
func TestLegacyMigrationKeepsOldUnitUntilInstallSucceeds(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	f := &fakeMigrationOps{paths: p, failStep: "install"}
	if _, err := runMigration(t, f); err == nil {
		t.Fatal("expected failure")
	}
	for _, path := range []string{p.OldUnit, p.OldCtl, p.OldWeb} {
		if !lexistsT(path) {
			t.Errorf("%s removed before the new install succeeded", path)
		}
	}
	if b, _ := os.ReadFile(p.OldBin); string(b) != "OLD-SERVERWATCH" {
		t.Error("old binary replaced before the new install succeeded")
	}
}

func TestLegacyMigrationKeepsUnrecognisedCompatTarget(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	mustWrite(t, p.OldBin, "#!/bin/sh\necho operator script\n")
	f := &fakeMigrationOps{paths: p}
	sum, err := runMigration(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p.OldBin); !strings.Contains(string(b), "operator script") {
		t.Error("unrecognised /usr/local/bin/serverwatch was overwritten")
	}
	if !strings.Contains(sum.String(), "not a serverwatch binary we recognise") {
		t.Errorf("summary lacks the kept-binary note:\n%s", sum)
	}
}

func TestIsOurLegacyBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !isOurLegacyBinary(self) {
		t.Error("a binary built from this module is not recognised")
	}
	txt := filepath.Join(t.TempDir(), "serverwatch")
	mustWrite(t, txt, "#!/bin/sh\n")
	if isOurLegacyBinary(txt) {
		t.Error("a shell script was recognised as ours")
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	if got := unescapeMountinfo(`/mnt/my\040disk\011x`); got != "/mnt/my disk\tx" {
		t.Errorf("got %q", got)
	}
	if got := unescapeMountinfo(`/var/lib/serverwatch`); got != "/var/lib/serverwatch" {
		t.Errorf("got %q", got)
	}
}

func TestIsMountPointFromMountinfo(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "vol dir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.EvalSymlinks(target)
	mi := filepath.Join(dir, "mountinfo")
	mustWrite(t, mi, "36 35 98:0 / "+strings.ReplaceAll(abs, " ", `\040`)+" rw,noatime master:1 - ext3 /dev/root rw\n")
	orig := mountinfoPath
	mountinfoPath = mi
	t.Cleanup(func() { mountinfoPath = orig })
	if !isMountPoint(target) {
		t.Error("mount point listed in mountinfo not detected")
	}
	if isMountPoint(dir) {
		t.Error("plain dir reported as a mount point")
	}
}

func TestInstallRejectsUnknownFlag(t *testing.T) {
	restoreGlobals(t)
	var errb bytes.Buffer
	stderr = &errb
	if code := cmdInstall([]string{"--bogus"}); code != 2 || !strings.Contains(errb.String(), "--force") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}
