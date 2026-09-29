//go:build trinetra_testkeys

// internal/update/keys_testkeys.go
package update

// ProductionKeys in a trinetra_testkeys build trusts the deterministic test
// signers (seeds 1 = ci, 2 = maint, 3 = pointer; 4/5/6 as "next" keys). Only
// the e2e image is built with this tag; the release workflow refuses to ship
// a binary whose Fingerprints() match these.
func ProductionKeys() KeySet {
	return KeySet{
		CI:      []PublicKey{NewTestSigner(1).Public(), NewTestSigner(4).Public()},
		Maint:   []PublicKey{NewTestSigner(2).Public(), NewTestSigner(5).Public()},
		Pointer: []PublicKey{NewTestSigner(3).Public(), NewTestSigner(6).Public()},
	}
}
