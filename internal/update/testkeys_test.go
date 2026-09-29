package update

import "testing"

// TestTestKeySetMatchesDeterministicSigners pins testKeySet() to the exact
// signer seeds documented on it and on keys_testkeys.go's ProductionKeys(),
// so the two cannot silently drift now that ProductionKeys() (in a
// trinetra_testkeys build) simply calls this function.
func TestTestKeySetMatchesDeterministicSigners(t *testing.T) {
	k := testKeySet()
	want := KeySet{
		CI:      []PublicKey{NewTestSigner(1).Public(), NewTestSigner(4).Public()},
		Maint:   []PublicKey{NewTestSigner(2).Public(), NewTestSigner(5).Public()},
		Pointer: []PublicKey{NewTestSigner(3).Public(), NewTestSigner(6).Public()},
	}
	eq := func(a, b []PublicKey) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if !a[i].Equal(b[i]) {
				return false
			}
		}
		return true
	}
	if !eq(k.CI, want.CI) || !eq(k.Maint, want.Maint) || !eq(k.Pointer, want.Pointer) {
		t.Fatalf("testKeySet() = %+v, want %+v", k, want)
	}
}
