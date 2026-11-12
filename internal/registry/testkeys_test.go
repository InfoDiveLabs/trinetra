//go:build trinetra_testkeys

package registry

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

func TestTestKeysMatchSeed(t *testing.T) {
	ks := Keys()
	want := ed25519.NewKeyFromSeed(TestSeed[:]).Public().(ed25519.PublicKey)
	if len(ks) != 1 || !bytes.Equal(ks[0], want) {
		t.Fatal("test key does not match TestSeed")
	}
}
