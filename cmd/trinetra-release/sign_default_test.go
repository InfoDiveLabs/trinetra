//go:build !trinetra_testkeys

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSignRoleMaintTestAbsentInDefaultBuild only applies to the default
// build: "sign --role maint-test" must not exist unless this tool is built
// with -tags trinetra_testkeys (see sign_testkeys.go). The testkeys build
// intentionally makes this role work, so this assertion does not hold there.
func TestSignRoleMaintTestAbsentInDefaultBuild(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	os.WriteFile(in, []byte("data"), 0o644)
	if code := run([]string{"sign", "--role", "maint-test", "--in", in, "--out", filepath.Join(dir, "out.sig")}); code == 0 {
		t.Fatal("sign --role maint-test succeeded in a default build")
	}
}

// TestTestKeysFlagsRefusedInDefaultBuild is R22: a release build of this
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
