//go:build trinetra_testkeys

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
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
	want := update.NewTestSigner(2).SignRelease(body)
	if string(got) != string(want) {
		t.Fatalf("signature mismatch:\n got  %q\n want %q", got, want)
	}
}
