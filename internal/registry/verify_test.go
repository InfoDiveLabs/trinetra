package registry

import (
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func keys() []ed25519.PublicKey { return []ed25519.PublicKey{testPub(7)} }

func TestDecodeIndexStrict(t *testing.T) {
	good, _ := marshalSigned(t, sampleIndex(1), testPriv(7))
	if _, err := DecodeIndex(good); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string][]byte{
		"unknown field": []byte(`{"schema":1,"serial":1,"issued":"2026-11-13T00:00:00Z","expires":"2026-11-20T00:00:00Z","plugins":[],"x":1}`),
		"trailing":      append(append([]byte{}, good...), []byte(" {}")...),
		"bad schema":    []byte(`{"schema":2,"serial":1,"issued":"2026-11-13T00:00:00Z","expires":"2026-11-20T00:00:00Z","plugins":[]}`),
		"too big":       make([]byte, MaxIndexBytes+1),
	} {
		if _, err := DecodeIndex(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dup := sampleIndex(1)
	dup.Plugins = append(dup.Plugins, dup.Plugins[0])
	b, _ := marshalSigned(t, dup, testPriv(7))
	if _, err := DecodeIndex(b); err == nil {
		t.Error("duplicate id accepted")
	}
	badVer := sampleIndex(1)
	badVer.Plugins[0].Versions[1].Version = "1.0.0"
	b, _ = marshalSigned(t, badVer, testPriv(7))
	if _, err := DecodeIndex(b); err == nil {
		t.Error("duplicate version accepted")
	}
	badSum := sampleIndex(1)
	badSum.Plugins[0].Versions[0].Packages[0].SHA256 = "zz"
	b, _ = marshalSigned(t, badSum, testPriv(7))
	if _, err := DecodeIndex(b); err == nil {
		t.Error("bad sha256 accepted")
	}
}

func TestVerifyIndexGoodSignature(t *testing.T) {
	b, sig := marshalSigned(t, sampleIndex(5), testPriv(7))
	ix, st, err := VerifyIndex(keys(), b, sig, testNow, State{})
	if err != nil || ix.Serial != 5 || st.LastSerial != 5 || st.LastSHA256 == "" || st.Expires == 0 || st.FetchedAt != testNow.Unix() {
		t.Fatalf("ix=%+v st=%+v err=%v", ix, st, err)
	}
}

func TestVerifyIndexBadSignature(t *testing.T) {
	b, sig := marshalSigned(t, sampleIndex(5), testPriv(8))
	if _, _, err := VerifyIndex(keys(), b, sig, testNow, State{}); !errors.Is(err, plugin.ErrBadSignature) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := VerifyIndex(keys(), b, []byte("garbage"), testNow, State{}); !errors.Is(err, plugin.ErrBadSignature) {
		t.Fatalf("garbage sig: %v", err)
	}
	b, _ = marshalSigned(t, sampleIndex(5), testPriv(7))
	manifestSig := signWith(testPriv(7), plugin.ManifestSigPrefix, b)
	if _, _, err := VerifyIndex(keys(), b, manifestSig, testNow, State{}); !errors.Is(err, plugin.ErrBadSignature) {
		t.Fatalf("wrong domain prefix accepted: %v", err)
	}
}

func TestVerifyIndexRejectsLowerSerial(t *testing.T) {
	b, sig := marshalSigned(t, sampleIndex(4), testPriv(7))
	if _, _, err := VerifyIndex(keys(), b, sig, testNow, State{LastSerial: 5}); !errors.Is(err, ErrRollback) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyIndexAcceptsSameSerialSameBytes(t *testing.T) {
	b, sig := marshalSigned(t, sampleIndex(5), testPriv(7))
	_, st, err := VerifyIndex(keys(), b, sig, testNow, State{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyIndex(keys(), b, sig, testNow, st); err != nil {
		t.Fatalf("same bytes: %v", err)
	}
}

func TestVerifyIndexRejectsSameSerialDifferentBytes(t *testing.T) {
	b, sig := marshalSigned(t, sampleIndex(5), testPriv(7))
	_, st, _ := VerifyIndex(keys(), b, sig, testNow, State{})
	other := sampleIndex(5)
	other.Plugins[0].Name = "Changed"
	b2, sig2 := marshalSigned(t, other, testPriv(7))
	if _, _, err := VerifyIndex(keys(), b2, sig2, testNow, st); !errors.Is(err, ErrRollback) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyIndexExpired(t *testing.T) {
	b, sig := marshalSigned(t, sampleIndex(5), testPriv(7))
	if _, _, err := VerifyIndex(keys(), b, sig, testNow.Add(21*24*time.Hour), State{}); !errors.Is(err, ErrIndexExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyIndexLifetimeTooLong(t *testing.T) {
	ix := sampleIndex(5)
	ix.Expires = testNow.Add(60 * 24 * time.Hour).Format(time.RFC3339)
	b, sig := marshalSigned(t, ix, testPriv(7))
	if _, _, err := VerifyIndex(keys(), b, sig, testNow, State{}); err == nil {
		t.Fatal("long lifetime accepted")
	}
	ix = sampleIndex(5)
	ix.Issued = testNow.Add(3 * time.Hour).Format(time.RFC3339)
	b, sig = marshalSigned(t, ix, testPriv(7))
	if _, _, err := VerifyIndex(keys(), b, sig, testNow, State{}); err == nil {
		t.Fatal("future issue accepted")
	}
}

func TestVerifyIndexRejectsHTTPURL(t *testing.T) {
	ix := sampleIndex(5)
	ix.Plugins[0].Versions[0].Packages[0].URL = "http://example.com/x.tgz"
	b, sig := marshalSigned(t, ix, testPriv(7))
	if _, _, err := VerifyIndex(keys(), b, sig, testNow, State{}); err == nil {
		t.Fatal("http url accepted")
	}
}

func TestVerifyManifestNoKeys(t *testing.T) {
	if err := VerifyManifest(nil, []byte("m"), []byte("s")); !errors.Is(err, plugin.ErrNoRegistryKeys) {
		t.Fatalf("err = %v", err)
	}
	m := []byte(`{"id":"x"}`)
	sig := signWith(testPriv(7), plugin.ManifestSigPrefix, m)
	if err := VerifyManifest(keys(), m, sig); err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifest(keys(), []byte(`{"id":"y"}`), sig); !errors.Is(err, plugin.ErrBadSignature) {
		t.Fatalf("tampered: %v", err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	if st, err := LoadState(p); err != nil || st != (State{}) {
		t.Fatalf("missing: %+v %v", st, err)
	}
	want := State{LastSerial: 3, LastSHA256: "ab", FetchedAt: 10, Expires: 20}
	if err := SaveState(p, want); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadState(p); err != nil || got != want {
		t.Fatalf("got %+v %v", got, err)
	}
}
