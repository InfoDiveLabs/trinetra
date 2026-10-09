//go:build !trinetra_testkeys

package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// TestSignRoleMaintTestAbsentInDefaultBuild only applies to the default build: "sign --role
// maint-test" must not exist unless this tool is built with -tags trinetra_testkeys.
func TestSignRoleMaintTestAbsentInDefaultBuild(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	os.WriteFile(in, []byte("data"), 0o644)
	if code := run([]string{"sign", "--role", "maint-test", "--in", in, "--out", filepath.Join(dir, "out.sig")}); code == 0 {
		t.Fatal("sign --role maint-test succeeded in a default build")
	}
}

// TestTestKeysFlagsRefusedInDefaultBuild: a release build of this
// tool carries no test trust anchor, so verify/cosign --testkeys refuse.
func TestTestKeysFlagsRefusedInDefaultBuild(t *testing.T) {
	if testKeySet != nil {
		t.Fatal("testKeySet is set in a default build")
	}
	if code := run([]string{"verify", t.TempDir(), "--testkeys"}); code == 0 {
		t.Fatal("verify --testkeys succeeded in a default build")
	}
	if code := run([]string{"cosign", "v1.2.3", "--testkeys", "--repo", "o/r", "--key", "k"}); code == 0 {
		t.Fatal("cosign --testkeys succeeded in a default build")
	}
}

// TestManifestKeysFromBinaryFillsProductionKeys: --keys-from-binary copies this tool's
// compiled-in ProductionKeys into manifest.keys.
func TestManifestKeysFromBinaryFillsProductionKeys(t *testing.T) {
	dir := t.TempDir()
	writeAllReleaseFiles(t, dir)
	if code := run([]string{"manifest", "--dir", dir, "--version", "0.5.0", "--channel", "stable",
		"--min-upgrade-from", "0.4.1", "--published", "2026-10-01T10:00:00Z", "--keys-from-binary"}); code != 0 {
		t.Fatalf("manifest --keys-from-binary exit %d", code)
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := update.DecodeManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	k := update.ProductionKeys()
	if len(m.Keys.CI) != len(k.CI) || len(m.Keys.Maint) != len(k.Maint) || len(m.Keys.Pointer) != len(k.Pointer) {
		t.Fatalf("manifest keys %+v do not match the compiled-in key set sizes", m.Keys)
	}
	for i, pk := range k.Maint {
		if m.Keys.Maint[i] != base64.StdEncoding.EncodeToString(pk) {
			t.Fatalf("manifest maint key %d = %s, want the compiled-in key", i, m.Keys.Maint[i])
		}
	}
}
