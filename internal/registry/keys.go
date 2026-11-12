//go:build !trinetra_testkeys

package registry

import (
	"crypto/ed25519"
	"encoding/base64"
)

var productionRegistryKeyB64 = []string{}

// Keys returns the registry public keys compiled into this build (current first).
func Keys() []ed25519.PublicKey { return decodeKeys(productionRegistryKeyB64) }

func decodeKeys(in []string) []ed25519.PublicKey {
	out := make([]ed25519.PublicKey, 0, len(in))
	for _, s := range in {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b) != ed25519.PublicKeySize {
			panic("registry: bad compiled-in public key " + s)
		}
		out = append(out, ed25519.PublicKey(b))
	}
	return out
}
