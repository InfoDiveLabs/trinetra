//go:build !trinetra_testkeys

package update

import "testing"

// TestVerifyReleaseFailsClosedWithProductionKeys only applies to the
// default build: production key lists are empty until the maintainer key
// ceremony (Task 12), so every verification must fail closed with
// ErrNoKeys rather than silently trusting nothing. The trinetra_testkeys
// build intentionally populates ProductionKeys(), so this assertion does
// not hold there.
func TestVerifyReleaseFailsClosedWithProductionKeys(t *testing.T) {
	_, err := VerifyRelease(ProductionKeys(), []byte("manifest"), []byte("sig"), []byte("sig"))
	if err != ErrNoKeys {
		t.Fatalf("VerifyRelease with ProductionKeys() = %v, want ErrNoKeys", err)
	}
}
