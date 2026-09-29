package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PublicKey lets other packages build a KeySet without importing crypto/ed25519.
type PublicKey = ed25519.PublicKey

const (
	ReleasePrefix = "trinetra-release-v1\n"
	ChannelPrefix = "trinetra-channel-v1\n"

	// MaxPointerLifetime is the longest span a channel pointer's Expires may
	// be set beyond its Issued time. A compromised pointer key must not be
	// able to freeze a host on a stale release by self-declaring a far-future
	// expiry.
	MaxPointerLifetime = 14 * 24 * time.Hour

	// pointerClockSkew is the slack allowed for clock drift between the
	// signer and the host when checking pointer lifetime and issue time.
	pointerClockSkew = time.Hour
)

var (
	ErrMalformed        = errors.New("update: malformed release data")
	ErrMissingSignature = errors.New("update: signature missing")
	ErrBadSignature     = errors.New("update: signature does not verify against any trusted key")
	ErrWrongProduct     = errors.New("update: not a trinetra release")
	ErrWrongChannel     = errors.New("update: release is for a different channel")
	ErrDowngrade        = errors.New("update: version is lower than the highest version already installed")
	ErrAlreadyInstalled = errors.New("update: this version is already installed")
	ErrTooOld           = errors.New("update: running version is too old to upgrade directly to this release")
	ErrExpired          = errors.New("update: channel pointer has expired")
	ErrNoKeys           = errors.New("update: this build has no release keys compiled in")
)

// KeySet is the trust anchor: public keys compiled into the running binary.
// Each role holds its current and next key.
type KeySet struct {
	CI, Maint, Pointer []ed25519.PublicKey
}

func (k KeySet) empty() bool { return len(k.CI) == 0 || len(k.Maint) == 0 || len(k.Pointer) == 0 }

// mustKeySet decodes compiled-in base64 keys; a malformed constant is a
// programming error caught by TestProductionKeysDecode.
func mustKeySet(ci, maint, ptr []string) KeySet {
	dec := func(in []string) []ed25519.PublicKey {
		var out []ed25519.PublicKey
		for _, s := range in {
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil || len(b) != ed25519.PublicKeySize {
				panic("update: bad compiled-in public key " + s)
			}
			out = append(out, ed25519.PublicKey(b))
		}
		return out
	}
	return KeySet{CI: dec(ci), Maint: dec(maint), Pointer: dec(ptr)}
}

// Fingerprints lists "role:hex(sha256(pubkey))" for every key, CI first.
func Fingerprints(k KeySet) []string {
	var out []string
	add := func(role string, keys []ed25519.PublicKey) {
		for _, pk := range keys {
			sum := sha256.Sum256(pk)
			out = append(out, role+":"+hex.EncodeToString(sum[:]))
		}
	}
	add("ci", k.CI)
	add("maint", k.Maint)
	add("pointer", k.Pointer)
	return out
}

// decodeSig reads a .sig file: base64 of a 64-byte signature, optional
// trailing whitespace.
func decodeSig(b []byte) ([]byte, error) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil, ErrMissingSignature
	}
	sig, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrBadSignature
	}
	return sig, nil
}

// VerifySignature checks a single detached signature file against a set of
// trusted keys for one domain-separated role (ReleasePrefix or
// ChannelPrefix), without requiring a second, paired signature the way
// VerifyRelease does. It exists for tooling that must verify one signature
// before its counterpart exists yet — for example cmd/trinetra-release
// cosign checking a release's CI signature before the maintainer signature
// has been produced.
func VerifySignature(keys []PublicKey, prefix string, msg, sigFile []byte) error {
	return verifyAny(keys, prefix, msg, sigFile)
}

func verifyAny(keys []ed25519.PublicKey, prefix string, msg, sigFile []byte) error {
	sig, err := decodeSig(sigFile)
	if err != nil {
		return err
	}
	full := append([]byte(prefix), msg...)
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, full, sig) {
			return nil
		}
	}
	return ErrBadSignature
}

// VerifyRelease checks both signatures over the exact manifest bytes, then
// decodes and validates the manifest. Signatures are checked first, so a
// malformed-but-unsigned manifest reports a signature error.
func VerifyRelease(keys KeySet, manifest, ciSig, maintSig []byte) (Manifest, error) {
	if keys.empty() {
		return Manifest{}, ErrNoKeys
	}
	if err := verifyAny(keys.CI, ReleasePrefix, manifest, ciSig); err != nil {
		return Manifest{}, fmt.Errorf("CI signature: %w", err)
	}
	if err := verifyAny(keys.Maint, ReleasePrefix, manifest, maintSig); err != nil {
		return Manifest{}, fmt.Errorf("maintainer signature: %w", err)
	}
	return DecodeManifest(manifest)
}

// VerifyPointer checks a channel pointer's signature, then its lifetime: a
// pointer must not have been issued in the future (past clock-skew slack),
// must not claim a lifetime longer than MaxPointerLifetime (plus slack), and
// must not have expired. All three protect against a compromised pointer
// key self-declaring an expiry far enough out to freeze a host on a stale
// release (Ruling R4).
func VerifyPointer(keys KeySet, pointer, sig []byte, now time.Time) (Pointer, error) {
	if keys.empty() {
		return Pointer{}, ErrNoKeys
	}
	if err := verifyAny(keys.Pointer, ChannelPrefix, pointer, sig); err != nil {
		return Pointer{}, fmt.Errorf("pointer signature: %w", err)
	}
	p, err := DecodePointer(pointer)
	if err != nil {
		return Pointer{}, err
	}
	issued, _ := time.Parse(time.RFC3339, p.Issued)
	exp, _ := time.Parse(time.RFC3339, p.Expires)
	if issued.After(now.Add(pointerClockSkew)) {
		return Pointer{}, fmt.Errorf("%w: pointer issued %s is in the future", ErrMalformed, p.Issued)
	}
	if lifetime := exp.Sub(issued); lifetime > MaxPointerLifetime+pointerClockSkew {
		return Pointer{}, fmt.Errorf("%w: pointer lifetime %s exceeds max %s", ErrMalformed, lifetime, MaxPointerLifetime)
	}
	if !now.Before(exp) {
		return Pointer{}, fmt.Errorf("%w (expired %s)", ErrExpired, p.Expires)
	}
	return p, nil
}

// Policy is the host-side context a verified manifest is checked against.
type Policy struct {
	Channel string
	// Floor is the highest version ever committed on this host; only
	// consulted when HasFloor is true.
	Floor Version
	// HasFloor reports whether Floor should be enforced. False means "no
	// floor recorded and no known running version" -- a fresh host, or one
	// where the running binary's version could not be determined -- and
	// therefore no lower bound at all: CheckPolicy skips the floor
	// comparison entirely rather than comparing against the zero Version,
	// which would wrongly read as "already at 0.0.0" and refuse any
	// pre-release of 0.0.0 (e.g. 0.0.0-rc.1) as a downgrade.
	HasFloor   bool
	Running    Version
	AllowEqual bool // re-apply the installed version (repair)
}

// ChannelAccepts reports whether a host on channel host accepts a release
// published on channel release (spec §2 Verification step 3, R18): a stable
// host accepts only stable; a beta host accepts beta and stable, so beta
// hosts also move on to final releases (beta = newest of either).
func ChannelAccepts(host, release string) bool {
	switch host {
	case "stable":
		return release == "stable"
	case "beta":
		return release == "beta" || release == "stable"
	default:
		return false
	}
}

// CheckPolicy applies the host rules to an already verified manifest.
func CheckPolicy(m Manifest, p Policy) error {
	if !ChannelAccepts(p.Channel, m.Channel) {
		return fmt.Errorf("%w: release %s, host %s", ErrWrongChannel, m.Channel, p.Channel)
	}
	v, _ := ParseVersion(m.Version)
	if p.HasFloor {
		switch c := CompareVersions(v, p.Floor); {
		case c < 0:
			return fmt.Errorf("%w: %s < %s", ErrDowngrade, v, p.Floor)
		case c == 0 && !p.AllowEqual:
			return fmt.Errorf("%w: %s", ErrAlreadyInstalled, v)
		}
	}
	min, _ := ParseVersion(m.MinUpgradeFrom)
	if CompareVersions(p.Running, min) < 0 {
		return fmt.Errorf("%w: running %s, needs at least %s", ErrTooOld, p.Running, min)
	}
	return nil
}
