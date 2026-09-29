// Package updatetest holds the deterministic test signers and key set used
// by tests and by trinetra_testkeys builds (the e2e image). Nothing in a
// default build imports it, so release binaries carry neither the signers'
// publicly derivable private keys nor the test trust anchor (R22; pinned by
// TestReleaseBinariesCarryNoTestKeys).
package updatetest

import (
	"crypto/ed25519"
	"encoding/base64"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// TestSigner signs with a deterministic key derived from one seed byte.
type TestSigner struct{ priv ed25519.PrivateKey }

// NewTestSigner returns the signer whose 32-byte seed is seed repeated.
func NewTestSigner(seed byte) TestSigner {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	return TestSigner{priv: ed25519.NewKeyFromSeed(s)}
}

func (t TestSigner) Public() ed25519.PublicKey { return t.priv.Public().(ed25519.PublicKey) }

func (t TestSigner) sign(prefix string, b []byte) []byte {
	sig := ed25519.Sign(t.priv, append([]byte(prefix), b...))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// SignRelease signs a release manifest (update.ReleasePrefix).
func (t TestSigner) SignRelease(b []byte) []byte { return t.sign(update.ReleasePrefix, b) }

// SignPointer signs a channel pointer (update.ChannelPrefix).
func (t TestSigner) SignPointer(b []byte) []byte { return t.sign(update.ChannelPrefix, b) }

// TestKeySet returns the deterministic test key set (signers 1/4 = CI,
// 2/5 = maint, 3/6 = pointer): exactly what update.ProductionKeys() returns
// in a trinetra_testkeys build.
func TestKeySet() update.KeySet {
	return update.KeySet{
		CI:      []update.PublicKey{NewTestSigner(1).Public(), NewTestSigner(4).Public()},
		Maint:   []update.PublicKey{NewTestSigner(2).Public(), NewTestSigner(5).Public()},
		Pointer: []update.PublicKey{NewTestSigner(3).Public(), NewTestSigner(6).Public()},
	}
}
