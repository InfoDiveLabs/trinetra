//go:build !trinetra_testkeys

package update

import "crypto/ed25519"

// testSeed is defined by keys_testkeys.go in a trinetra_testkeys build; the
// default build's tests need their own copy.
func testSeed(seed byte) []byte {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	return s
}
