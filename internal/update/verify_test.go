// internal/update/verify_test.go
package update

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func testManifest() Manifest {
	return Manifest{
		Schema: 1, Product: "trinetra", Version: "0.5.0", Channel: "stable",
		Published: "2026-10-01T10:00:00Z", MinUpgradeFrom: "0.4.1",
		Files: []File{{Name: "trinetra-linux-amd64", OS: "linux", Arch: "amd64", Size: 3, SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}},
	}
}

func signed(t *testing.T, m Manifest, ci, maint TestSigner) (mb, cs, ms []byte) {
	t.Helper()
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return mb, ci.SignRelease(mb), maint.SignRelease(mb)
}

func keysFor(ci, maint, ptr TestSigner) KeySet {
	return KeySet{CI: []PublicKey{ci.Public()}, Maint: []PublicKey{maint.Public()}, Pointer: []PublicKey{ptr.Public()}}
}

func TestVerifyReleaseRequiresBothSignatures(t *testing.T) {
	ci, maint, ptr, other := NewTestSigner(1), NewTestSigner(2), NewTestSigner(3), NewTestSigner(9)
	keys := keysFor(ci, maint, ptr)
	mb, cs, ms := signed(t, testManifest(), ci, maint)

	if _, err := VerifyRelease(keys, mb, cs, ms); err != nil {
		t.Fatalf("valid release refused: %v", err)
	}
	if _, err := VerifyRelease(keys, mb, nil, ms); !errors.Is(err, ErrMissingSignature) {
		t.Errorf("missing CI sig: err = %v, want ErrMissingSignature", err)
	}
	if _, err := VerifyRelease(keys, mb, cs, nil); !errors.Is(err, ErrMissingSignature) {
		t.Errorf("missing maint sig: err = %v, want ErrMissingSignature", err)
	}
	if _, err := VerifyRelease(keys, mb, other.SignRelease(mb), ms); !errors.Is(err, ErrBadSignature) {
		t.Errorf("CI sig by unknown key: err = %v, want ErrBadSignature", err)
	}
	if _, err := VerifyRelease(keys, mb, cs, ci.SignRelease(mb)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("CI key used as maint: err = %v, want ErrBadSignature", err)
	}
	tampered := append([]byte{}, mb...)
	tampered[len(tampered)-2] = ' '
	if _, err := VerifyRelease(keys, tampered, cs, ms); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered manifest: err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyReleaseDomainSeparation(t *testing.T) {
	ci, maint, ptr := NewTestSigner(1), NewTestSigner(2), NewTestSigner(3)
	keys := keysFor(ci, maint, ptr)
	mb, _, ms := signed(t, testManifest(), ci, maint)
	// A signature over the raw bytes (no prefix) or with the channel prefix must not verify.
	if _, err := VerifyRelease(keys, mb, ci.signRaw(mb), ms); !errors.Is(err, ErrBadSignature) {
		t.Errorf("unprefixed sig accepted: %v", err)
	}
	if _, err := VerifyRelease(keys, mb, ci.SignPointer(mb), ms); !errors.Is(err, ErrBadSignature) {
		t.Errorf("channel-prefixed sig accepted as release sig: %v", err)
	}
}

func TestDecodeManifestStrict(t *testing.T) {
	good, _ := json.Marshal(testManifest())
	if _, err := DecodeManifest(good); err != nil {
		t.Fatalf("good manifest: %v", err)
	}
	for name, b := range map[string]string{
		"unknown field":  `{"schema":1,"product":"trinetra","version":"0.5.0","channel":"stable","published":"2026-10-01T10:00:00Z","min_upgrade_from":"0.4.1","files":[],"extra":1}`,
		"wrong product":  `{"schema":1,"product":"other","version":"0.5.0","channel":"stable","published":"2026-10-01T10:00:00Z","min_upgrade_from":"0.4.1","files":[{"name":"a","os":"linux","arch":"amd64","size":1,"sha256":"00"}]}`,
		"wrong schema":   `{"schema":2,"product":"trinetra","version":"0.5.0","channel":"stable","published":"2026-10-01T10:00:00Z","min_upgrade_from":"0.4.1","files":[{"name":"a","os":"linux","arch":"amd64","size":1,"sha256":"00"}]}`,
		"beta on stable": `{"schema":1,"product":"trinetra","version":"0.5.0-beta.1","channel":"stable","published":"2026-10-01T10:00:00Z","min_upgrade_from":"0.4.1","files":[{"name":"a","os":"linux","arch":"amd64","size":1,"sha256":"00"}]}`,
		"bad channel":    `{"schema":1,"product":"trinetra","version":"0.5.0","channel":"nightly","published":"2026-10-01T10:00:00Z","min_upgrade_from":"0.4.1","files":[{"name":"a","os":"linux","arch":"amd64","size":1,"sha256":"00"}]}`,
		"no files":       `{"schema":1,"product":"trinetra","version":"0.5.0","channel":"stable","published":"2026-10-01T10:00:00Z","min_upgrade_from":"0.4.1","files":[]}`,
		"bad sha":        `{"schema":1,"product":"trinetra","version":"0.5.0","channel":"stable","published":"2026-10-01T10:00:00Z","min_upgrade_from":"0.4.1","files":[{"name":"a","os":"linux","arch":"amd64","size":1,"sha256":"zz"}]}`,
		"path in name":   `{"schema":1,"product":"trinetra","version":"0.5.0","channel":"stable","published":"2026-10-01T10:00:00Z","min_upgrade_from":"0.4.1","files":[{"name":"../x","os":"linux","arch":"amd64","size":1,"sha256":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}]}`,
		"trailing data":  string(good) + `{}`,
	} {
		if _, err := DecodeManifest([]byte(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckPolicy(t *testing.T) {
	m := testManifest()
	v := func(s string) Version { x, _ := ParseVersion(s); return x }
	ok := Policy{Channel: "stable", Floor: v("0.4.1"), Running: v("0.4.1")}
	if err := CheckPolicy(m, ok); err != nil {
		t.Fatalf("ok policy: %v", err)
	}
	cases := []struct {
		p    Policy
		want error
	}{
		{Policy{Channel: "beta", Floor: v("0.4.1"), Running: v("0.4.1")}, ErrWrongChannel},
		{Policy{Channel: "stable", Floor: v("0.6.0"), Running: v("0.4.1")}, ErrDowngrade},
		{Policy{Channel: "stable", Floor: v("0.5.0"), Running: v("0.5.0")}, ErrAlreadyInstalled},
		{Policy{Channel: "stable", Floor: v("0.3.0"), Running: v("0.3.0")}, ErrTooOld},
	}
	for _, c := range cases {
		if err := CheckPolicy(m, c.p); !errors.Is(err, c.want) {
			t.Errorf("policy %+v: err = %v, want %v", c.p, err, c.want)
		}
	}
	eq := Policy{Channel: "stable", Floor: v("0.5.0"), Running: v("0.5.0"), AllowEqual: true}
	if err := CheckPolicy(m, eq); err != nil {
		t.Errorf("AllowEqual: %v", err)
	}
}

func TestVerifyPointer(t *testing.T) {
	ci, maint, ptr := NewTestSigner(1), NewTestSigner(2), NewTestSigner(3)
	keys := keysFor(ci, maint, ptr)
	issued := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	p := Pointer{Schema: 1, Product: "trinetra", Channel: "stable", Version: "0.5.0",
		Issued: issued.Format(time.RFC3339), Expires: issued.Add(14 * 24 * time.Hour).Format(time.RFC3339)}
	pb, _ := json.Marshal(p)
	if _, err := VerifyPointer(keys, pb, ptr.SignPointer(pb), issued.Add(time.Hour)); err != nil {
		t.Fatalf("valid pointer: %v", err)
	}
	if _, err := VerifyPointer(keys, pb, ptr.SignPointer(pb), issued.Add(15*24*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Errorf("expired pointer: %v", err)
	}
	if _, err := VerifyPointer(keys, pb, ci.SignPointer(pb), issued.Add(time.Hour)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("pointer signed by CI key accepted: %v", err)
	}
	if _, err := VerifyPointer(keys, pb, ptr.SignRelease(pb), issued.Add(time.Hour)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("release-prefixed sig accepted as pointer sig: %v", err)
	}

	// Ruling R4: a pointer's own lifetime must not exceed MaxPointerLifetime
	// (plus clock-skew slack), regardless of what it self-declares.
	longLived := Pointer{Schema: 1, Product: "trinetra", Channel: "stable", Version: "0.5.0",
		Issued: issued.Format(time.RFC3339), Expires: issued.Add(30 * 24 * time.Hour).Format(time.RFC3339)}
	llb, _ := json.Marshal(longLived)
	if _, err := VerifyPointer(keys, llb, ptr.SignPointer(llb), issued.Add(time.Hour)); !errors.Is(err, ErrMalformed) {
		t.Errorf("30-day lifetime pointer accepted: err = %v, want ErrMalformed", err)
	}

	// A pointer issued more than 1h in the future (relative to now) is rejected.
	futureIssued := Pointer{Schema: 1, Product: "trinetra", Channel: "stable", Version: "0.5.0",
		Issued: issued.Add(2 * time.Hour).Format(time.RFC3339), Expires: issued.Add(2*time.Hour + 14*24*time.Hour).Format(time.RFC3339)}
	fib, _ := json.Marshal(futureIssued)
	if _, err := VerifyPointer(keys, fib, ptr.SignPointer(fib), issued); !errors.Is(err, ErrMalformed) {
		t.Errorf("pointer issued 2h in the future accepted: err = %v, want ErrMalformed", err)
	}

	// Exactly MaxPointerLifetime (14 days) is still accepted.
	exact14d := Pointer{Schema: 1, Product: "trinetra", Channel: "stable", Version: "0.5.0",
		Issued: issued.Format(time.RFC3339), Expires: issued.Add(MaxPointerLifetime).Format(time.RFC3339)}
	e14b, _ := json.Marshal(exact14d)
	if _, err := VerifyPointer(keys, e14b, ptr.SignPointer(e14b), issued.Add(time.Hour)); err != nil {
		t.Errorf("exactly-14-day pointer refused: %v", err)
	}
}
