package update

import (
	"strings"
	"testing"
)

func TestFingerprintsAreRoleTagged(t *testing.T) {
	k := KeySet{CI: []PublicKey{NewTestSigner(1).Public()}, Maint: []PublicKey{NewTestSigner(2).Public()}, Pointer: []PublicKey{NewTestSigner(3).Public()}}
	fp := Fingerprints(k)
	if len(fp) != 3 || !strings.HasPrefix(fp[0], "ci:") || !strings.HasPrefix(fp[1], "maint:") || !strings.HasPrefix(fp[2], "pointer:") {
		t.Fatalf("Fingerprints = %v", fp)
	}
	if len(strings.TrimPrefix(fp[0], "ci:")) != 64 {
		t.Fatalf("fingerprint not sha256 hex: %q", fp[0])
	}
}

func TestProductionKeysDecode(t *testing.T) {
	// Must not panic; an empty set is allowed until the key ceremony and makes every
	// verification fail closed with ErrNoKeys.
	k := ProductionKeys()
	for _, pk := range append(append(append([]PublicKey{}, k.CI...), k.Maint...), k.Pointer...) {
		if len(pk) != 32 {
			t.Fatalf("bad key length %d", len(pk))
		}
	}
}
