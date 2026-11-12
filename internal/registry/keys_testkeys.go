//go:build trinetra_testkeys

package registry

import "crypto/ed25519"

// Keys returns the deterministic test key of the trinetra_testkeys build.
func Keys() []ed25519.PublicKey {
	return []ed25519.PublicKey{ed25519.NewKeyFromSeed(TestSeed[:]).Public().(ed25519.PublicKey)}
}
