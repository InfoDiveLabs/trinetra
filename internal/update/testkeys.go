package update

import (
	"crypto/ed25519"
	"encoding/base64"
)

// TestSigner signs with a deterministic key derived from one seed byte. It
// exists for tests and the e2e fixture tool; nothing in the host update path
// calls it, and production trust comes only from ProductionKeys().
type TestSigner struct{ priv ed25519.PrivateKey }

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

func (t TestSigner) SignRelease(b []byte) []byte { return t.sign(ReleasePrefix, b) }
func (t TestSigner) SignPointer(b []byte) []byte { return t.sign(ChannelPrefix, b) }
func (t TestSigner) signRaw(b []byte) []byte     { return t.sign("", b) }

// TestKeySet returns the deterministic test key set (signers 1/4 = CI,
// 2/5 = maint, 3/6 = pointer). It is available in every build, unlike
// ProductionKeys(), which only returns these keys when built with the
// trinetra_testkeys tag (keys_testkeys.go). Tooling that needs to verify
// against the same test trust anchor without that build tag — such as
// cmd/trinetra-release's `verify --testkeys` and `cosign --testkeys` — calls
// this directly; nothing in the host update path does.
func TestKeySet() KeySet {
	return KeySet{
		CI:      []PublicKey{NewTestSigner(1).Public(), NewTestSigner(4).Public()},
		Maint:   []PublicKey{NewTestSigner(2).Public(), NewTestSigner(5).Public()},
		Pointer: []PublicKey{NewTestSigner(3).Public(), NewTestSigner(6).Public()},
	}
}
