package serverwatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyFileAtomicReplace guards the rename-based copyFile: it must replace
// an existing dst (the running-binary upgrade path) with the new content and
// perm, and leave no ".tmp-install" scratch behind. rename(2) -- not a
// truncating write -- is what makes this ETXTBSY-safe for a live daemon.
func TestCopyFileAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("NEW-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst, 0o755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-BINARY" {
		t.Errorf("dst content = %q, want NEW-BINARY", got)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o755 {
		t.Errorf("dst perm = %v, want 0755", fi.Mode().Perm())
	}
	if _, err := os.Stat(dst + ".tmp-install"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}
}

func TestRenderUnit(t *testing.T) {
	u := renderUnit("/usr/local/bin/serverwatch")
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"ExecStart=/usr/local/bin/serverwatch daemon",
		"Restart=always",
		"WatchdogSec=",
		"RuntimeDirectory=serverwatch",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit missing %q:\n%s", want, u)
		}
	}
}

// TestCmdDoctorPrintsCollectorSummary is a CLI-level smoke test for
// cmdDoctor: it loads the configured store (via openConfiguredStore, same
// helper migrate/dump use) and renders the collector on/off toggles plus
// SampleStore stats via buildDoctorReport/renderDoctorReport (systemd.go).
// Exercises the real tsfile-backend path end to end, not just those helpers
// in isolation.
func TestCmdDoctorPrintsCollectorSummary(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	stateDir = filepath.Join(dir, "state")

	var out bytes.Buffer
	stdout = &out
	if code := Main([]string{"doctor"}); code != 0 {
		t.Fatalf("doctor exit=%d", code)
	}
	got := out.String()
	for _, want := range []string{
		"collectors: container_stats=on net_throughput=on services=on processes=on smart_attrs=on",
		"time-series:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor output missing %q; got:\n%s", want, got)
		}
	}
}

func TestQuietHoursAndScheduleRequireArgs(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	cases := []struct {
		name string
		args []string
	}{
		{"quiet-hours no arg", []string{"quiet-hours"}},
		{"quiet-hours too many", []string{"quiet-hours", "23-8", "extra"}},
		{"schedule no arg", []string{"schedule"}},
		{"schedule daily no value", []string{"schedule", "daily"}},
		{"schedule weekly no value", []string{"schedule", "weekly"}},
	}
	for _, tc := range cases {
		errb.Reset()
		if code := Main(tc.args); code != 2 {
			t.Fatalf("%s: exit=%d, want 2 (stderr=%q)", tc.name, code, errb.String())
		}
	}
}

// TestWritePluginManifest_RecordsPresentCompanions is the core install-time
// manifest test: given a bin directory with both companion binaries present,
// writePluginManifest must record each one's exact SHA-256 under its
// pluginPath-convention name ("ctl"/"web"), mode 0600, at
// <stateDir>/plugins.json.
func TestWritePluginManifest_RecordsPresentCompanions(t *testing.T) {
	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	binDir := t.TempDir()
	ctlContent := []byte("pretend serverwatch-ctl binary")
	webContent := []byte("pretend serverwatch-web binary")
	if err := os.WriteFile(filepath.Join(binDir, "serverwatch-ctl"), ctlContent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "serverwatch-web"), webContent, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := writePluginManifest(binDir); err != nil {
		t.Fatalf("writePluginManifest: %v", err)
	}

	wantCtl, err := sha256File(filepath.Join(binDir, "serverwatch-ctl"))
	if err != nil {
		t.Fatal(err)
	}
	wantWeb, err := sha256File(filepath.Join(binDir, "serverwatch-web"))
	if err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(pluginManifestPath())
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("manifest mode = %o, want 0600", fi.Mode().Perm())
	}

	b, err := os.ReadFile(pluginManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	want := map[string]string{"ctl": wantCtl, "web": wantWeb}
	if got["ctl"] != want["ctl"] || got["web"] != want["web"] {
		t.Fatalf("manifest = %v, want %v", got, want)
	}

	// Round-trip through the exact reader the launcher uses.
	loaded, err := loadPluginManifest()
	if err != nil {
		t.Fatalf("loadPluginManifest: %v", err)
	}
	if loaded["ctl"] != wantCtl || loaded["web"] != wantWeb {
		t.Fatalf("loadPluginManifest = %v, want %v", loaded, want)
	}
}

// TestWritePluginManifest_OmitsAbsentCompanion checks that when only one
// companion binary is present next to the daemon (a `serverwatch-ctl`-only
// install with no `serverwatch-web`, or vice versa), the manifest simply
// omits the absent one rather than erroring or recording a bogus entry.
func TestWritePluginManifest_OmitsAbsentCompanion(t *testing.T) {
	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "serverwatch-ctl"), []byte("ctl only"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately no serverwatch-web in binDir.

	if err := writePluginManifest(binDir); err != nil {
		t.Fatalf("writePluginManifest: %v", err)
	}

	got, err := loadPluginManifest()
	if err != nil {
		t.Fatalf("loadPluginManifest: %v", err)
	}
	if _, ok := got["ctl"]; !ok {
		t.Errorf("manifest missing ctl entry: %v", got)
	}
	if _, ok := got["web"]; ok {
		t.Errorf("manifest has a web entry despite no serverwatch-web on disk: %v", got)
	}
}

// TestWritePluginManifest_VerifyPluginAcceptsMatchAndRejectsTamper is the
// end-to-end check that the manifest writePluginManifest produces is exactly
// what verifyPlugin (plugin_launch.go, Task 1) expects: a companion file
// that still matches what was recorded at install time verifies clean, and
// the same file modified afterward (a swap/tamper) is rejected.
func TestWritePluginManifest_VerifyPluginAcceptsMatchAndRejectsTamper(t *testing.T) {
	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	binDir := t.TempDir()
	if err := os.Chmod(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctlPath := filepath.Join(binDir, "serverwatch-ctl")
	if err := os.WriteFile(ctlPath, []byte("original ctl binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := writePluginManifest(binDir); err != nil {
		t.Fatalf("writePluginManifest: %v", err)
	}
	manifest, err := loadPluginManifest()
	if err != nil {
		t.Fatalf("loadPluginManifest: %v", err)
	}

	// Matching companion: verifyPlugin must accept it.
	if err := verifyPlugin(ctlPath, os.Getuid(), manifest, "ctl"); err != nil {
		t.Fatalf("verifyPlugin on untouched companion: want nil, got %v", err)
	}

	// Tamper with the binary AFTER the manifest was recorded (a swap
	// attack): verifyPlugin must now refuse it.
	if err := os.WriteFile(ctlPath, []byte("attacker-controlled payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	err = verifyPlugin(ctlPath, os.Getuid(), manifest, "ctl")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("verifyPlugin on tampered companion: want errPluginVerificationFailed, got %v", err)
	}
}

// TestWritePluginManifest_NonFatalStyle documents (and guards) the intended
// call style in cmdInstall: writePluginManifest returns a plain error the
// caller is expected to log as a warning and continue past, not a value that
// forces cmdInstall to abort the daemon install over a manifest hiccup. This
// test exercises the success path explicitly returns nil so callers know
// "no error" means the manifest was written.
func TestWritePluginManifest_NonFatalStyle(t *testing.T) {
	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	binDir := t.TempDir() // no companions at all
	if err := writePluginManifest(binDir); err != nil {
		t.Fatalf("writePluginManifest with no companions present: want nil, got %v", err)
	}
	got, err := loadPluginManifest()
	if err != nil {
		t.Fatalf("loadPluginManifest: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("manifest = %v, want empty", got)
	}
}

// TestWritePluginManifest_ForcesModeOnReinstall guards against a gotcha
// os.WriteFile has: it only applies its mode argument when CREATING the
// file. On a re-install (`serverwatch install` run again to upgrade), if
// plugins.json already exists with looser permissions, a plain
// os.WriteFile(path, b, 0o600) call would truncate and rewrite its content
// but leave the existing (looser) mode untouched, silently weakening the
// "root-only trust anchor" guarantee. This pre-creates the manifest at
// 0o644 and asserts writePluginManifest forces it back down to exactly
// 0o600, proving the mode is enforced on an EXISTING file, not just on
// create.
func TestWritePluginManifest_ForcesModeOnReinstall(t *testing.T) {
	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	// Simulate a prior install that (somehow) left plugins.json world/group
	// readable.
	if err := os.WriteFile(pluginManifestPath(), []byte(`{"ctl":"stale"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(pluginManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("precondition: manifest mode = %o, want 0644", fi.Mode().Perm())
	}

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "serverwatch-ctl"), []byte("ctl v2"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := writePluginManifest(binDir); err != nil {
		t.Fatalf("writePluginManifest (re-install): %v", err)
	}

	fi, err = os.Stat(pluginManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode after re-install = %o, want 0600 (mode must be forced on an existing file, not just on create)", fi.Mode().Perm())
	}
}

// TestCmdUninstall_RemovesPluginManifest checks the other half of Task 2:
// cmdUninstall removes <stateDir>/plugins.json (best-effort, like its other
// cleanups) so a subsequent front-door invocation correctly reports the
// plugins as not installed rather than checking against a stale manifest.
func TestCmdUninstall_RemovesPluginManifest(t *testing.T) {
	dir := t.TempDir()
	prevStateDir := stateDir
	prevCfgPath := cfgPath
	stateDir = filepath.Join(dir, "state")
	cfgPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() {
		stateDir = prevStateDir
		cfgPath = prevCfgPath
	})

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginManifestPath(), []byte(`{"ctl":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	stdout = &out
	if code := cmdUninstall(nil); code != 0 {
		t.Fatalf("cmdUninstall exit=%d", code)
	}

	if _, err := os.Stat(pluginManifestPath()); !os.IsNotExist(err) {
		t.Fatalf("plugin manifest still present after uninstall: err=%v", err)
	}
}
