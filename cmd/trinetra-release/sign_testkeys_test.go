//go:build trinetra_testkeys

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/update/updatetest"
)

// TestSignRoleMaintTestSignsWithTestSigner2 only applies to the
// trinetra_testkeys build: "sign --role maint-test" must sign with the
// deterministic maintainer test key (seed 2), matching the e2e fixtures.
func TestSignRoleMaintTestSignsWithTestSigner2(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "manifest.json")
	out := filepath.Join(dir, "manifest.maint.sig")
	body := []byte("fixture manifest bytes")
	if err := os.WriteFile(in, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"sign", "--role", "maint-test", "--in", in, "--out", out}); code != 0 {
		t.Fatalf("sign --role maint-test exit %d", code)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := updatetest.NewTestSigner(2).SignRelease(body)
	if string(got) != string(want) {
		t.Fatalf("signature mismatch:\n got  %q\n want %q", got, want)
	}
}

func TestVerifyCommandWithTestKeys(t *testing.T) {
	dir := t.TempDir()
	writeAllReleaseFiles(t, dir)
	if code := run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable", "--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z"}); code != 0 {
		t.Fatalf("manifest exit %d", code)
	}
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	os.WriteFile(filepath.Join(dir, "manifest.ci.sig"), updatetest.NewTestSigner(1).SignRelease(mb), 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.maint.sig"), updatetest.NewTestSigner(2).SignRelease(mb), 0o644)
	if code := run([]string{"verify", dir, "--testkeys"}); code != 0 {
		t.Fatalf("verify exit %d", code)
	}
	// Tampering with a release file must fail verification.
	os.WriteFile(filepath.Join(dir, "trinetra-linux-amd64"), []byte("tampered"), 0o755)
	if code := run([]string{"verify", dir, "--testkeys"}); code == 0 {
		t.Fatal("verify accepted a tampered file")
	}
}

// TestVerifyTestKeysPrintsWarningBanner covers review M6: a --testkeys
// verification must be impossible to mistake for a real one.
func TestVerifyTestKeysPrintsWarningBanner(t *testing.T) {
	dir := t.TempDir()
	writeAllReleaseFiles(t, dir)
	if code := run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable", "--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z"}); code != 0 {
		t.Fatalf("manifest exit %d", code)
	}
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	os.WriteFile(filepath.Join(dir, "manifest.ci.sig"), updatetest.NewTestSigner(1).SignRelease(mb), 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.maint.sig"), updatetest.NewTestSigner(2).SignRelease(mb), 0o644)

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"verify", dir, "--testkeys"})
	})
	if code != 0 {
		t.Fatalf("verify exit %d", code)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "TEST keys") {
		t.Fatalf("missing test-keys warning banner in output: %q", out)
	}
}

// TestManifestKeysFromBinary: manifest --keys-from-binary fills
// manifest.keys with exactly the compiled-in key set (base64), so cosign's
// rotation review compares like with like.
func TestManifestKeysFromBinary(t *testing.T) {
	dir := t.TempDir()
	writeAllReleaseFiles(t, dir)
	if code := run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable",
		"--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z", "--keys-from-binary"}); code != 0 {
		t.Fatalf("manifest --keys-from-binary exit %d", code)
	}
	mb, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	m, err := update.DecodeManifest(mb)
	if err != nil {
		t.Fatal(err)
	}
	if d := diffManifestKeys(m.Keys, update.ProductionKeys()); len(d) != 0 || len(m.Keys.CI) != 2 {
		t.Fatalf("manifest keys %+v differ from ProductionKeys: %v", m.Keys, d)
	}
	if code := run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable",
		"--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z", "--keys-from-binary", "--keys-ci", "x"}); code == 0 {
		t.Fatal("--keys-from-binary combined with --keys-ci accepted")
	}
}
