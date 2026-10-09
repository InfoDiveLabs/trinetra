package update

import (
	"crypto/ed25519"
	"encoding/base64"
)

// TestSigner is this package's own copy of updatetest.TestSigner (package update's internal
// tests cannot import updatetest, which imports update).
type TestSigner struct{ priv ed25519.PrivateKey }

func NewTestSigner(seed byte) TestSigner {
	return TestSigner{priv: ed25519.NewKeyFromSeed(testSeed(seed))}
}

func (t TestSigner) Public() ed25519.PublicKey { return t.priv.Public().(ed25519.PublicKey) }

func (t TestSigner) sign(prefix string, b []byte) []byte {
	sig := ed25519.Sign(t.priv, append([]byte(prefix), b...))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

func (t TestSigner) SignRelease(b []byte) []byte { return t.sign(ReleasePrefix, b) }
func (t TestSigner) SignPointer(b []byte) []byte { return t.sign(ChannelPrefix, b) }
func (t TestSigner) signRaw(b []byte) []byte     { return t.sign("", b) }

// testKeySet mirrors updatetest.TestKeySet for this package's own tests.
func testKeySet() KeySet {
	return KeySet{
		CI:      []PublicKey{NewTestSigner(1).Public(), NewTestSigner(4).Public()},
		Maint:   []PublicKey{NewTestSigner(2).Public(), NewTestSigner(5).Public()},
		Pointer: []PublicKey{NewTestSigner(3).Public(), NewTestSigner(6).Public()},
	}
}
