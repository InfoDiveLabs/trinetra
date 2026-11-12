package registry

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

var (
	ErrRollback     = errors.New("registry: index serial is older than the last one seen")
	ErrIndexExpired = errors.New("registry: index has expired")
)

func verifySig(keys []ed25519.PublicKey, prefix string, msg, sigFile []byte) error {
	if len(keys) == 0 {
		return plugin.ErrNoRegistryKeys
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigFile)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return plugin.ErrBadSignature
	}
	signed := append([]byte(prefix), msg...)
	for _, k := range keys {
		if ed25519.Verify(k, signed, sig) {
			return nil
		}
	}
	return plugin.ErrBadSignature
}

// VerifyIndex checks signature (plugin.IndexSigPrefix), lifetime, expiry and serial against st.
func VerifyIndex(keys []ed25519.PublicKey, b, sig []byte, now time.Time, st State) (Index, State, error) {
	return verifyIndex(keys, b, sig, now, st, true)
}

func verifyIndex(keys []ed25519.PublicKey, b, sig []byte, now time.Time, st State, checkExpiry bool) (Index, State, error) {
	if err := verifySig(keys, plugin.IndexSigPrefix, b, sig); err != nil {
		return Index{}, st, err
	}
	ix, err := DecodeIndex(b)
	if err != nil {
		return Index{}, st, err
	}
	issued, _ := time.Parse(time.RFC3339, ix.Issued)
	expires, _ := time.Parse(time.RFC3339, ix.Expires)
	if expires.Sub(issued) > MaxIndexLifetime+clockSkew {
		return Index{}, st, fmt.Errorf("registry: index lifetime exceeds %s", MaxIndexLifetime)
	}
	if issued.After(now.Add(clockSkew)) {
		return Index{}, st, errors.New("registry: index is issued in the future")
	}
	if checkExpiry && !now.Before(expires) {
		return Index{}, st, ErrIndexExpired
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	if ix.Serial < st.LastSerial || (ix.Serial == st.LastSerial && st.LastSHA256 != "" && st.LastSHA256 != digest) {
		return Index{}, st, ErrRollback
	}
	return ix, State{LastSerial: ix.Serial, LastSHA256: digest, FetchedAt: now.Unix(), Expires: expires.Unix()}, nil
}

// VerifyManifest checks plugin.json.sig (plugin.ManifestSigPrefix) against keys; plugin.ErrNoRegistryKeys when keys is empty.
func VerifyManifest(keys []ed25519.PublicKey, manifest, sig []byte) error {
	return verifySig(keys, plugin.ManifestSigPrefix, manifest, sig)
}
