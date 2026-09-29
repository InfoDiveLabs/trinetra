//go:build trinetra_testkeys

// internal/update/keys_testkeys.go
package update

import "crypto/ed25519"

// ProductionKeys in a trinetra_testkeys build trusts the deterministic test
// signers (seeds 1 = ci, 2 = maint, 3 = pointer; 4/5/6 as "next" keys; see
// internal/update/updatetest). Only the e2e image is built with this tag;
// the release workflow refuses to ship a binary whose Fingerprints() match
// these, and default builds carry none of this (R22).
func ProductionKeys() KeySet {
	pub := func(seed byte) PublicKey {
		return ed25519.NewKeyFromSeed(testSeed(seed)).Public().(ed25519.PublicKey)
	}
	return KeySet{
		CI:      []PublicKey{pub(1), pub(4)},
		Maint:   []PublicKey{pub(2), pub(5)},
		Pointer: []PublicKey{pub(3), pub(6)},
	}
}

func testSeed(seed byte) []byte {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	return s
}
