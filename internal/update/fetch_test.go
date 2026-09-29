// internal/update/fetch_test.go
package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memSource map[string][]byte

func (m memSource) ReleaseAsset(_ context.Context, version, name string) (io.ReadCloser, error) {
	b, ok := m[version+"/"+name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m memSource) ChannelAsset(_ context.Context, name string) (io.ReadCloser, error) {
	b, ok := m["channels/"+name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func fileFor(name string, body []byte) File {
	s := sha256.Sum256(body)
	return File{Name: name, OS: "linux", Arch: "amd64", Size: int64(len(body)), SHA256: hex.EncodeToString(s[:])}
}

func TestFetchVerifiedOK(t *testing.T) {
	body := []byte("#!binary")
	src := memSource{"0.5.0/trinetra-linux-amd64": body}
	dir := t.TempDir()
	p, err := FetchVerified(context.Background(), src, "0.5.0", fileFor("trinetra-linux-amd64", body), dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, body) || filepath.Dir(p) != dir {
		t.Fatalf("got %q at %s", got, p)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestFetchVerifiedRejects(t *testing.T) {
	body := []byte("#!binary")
	f := fileFor("trinetra-linux-amd64", body)
	for name, served := range map[string][]byte{
		"tampered": []byte("#!BINARY"),
		"short":    body[:3],
		"oversize": append(append([]byte{}, body...), 'x'),
	} {
		dir := t.TempDir()
		_, err := FetchVerified(context.Background(), memSource{"0.5.0/trinetra-linux-amd64": served}, "0.5.0", f, dir, 0o755)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		ents, _ := os.ReadDir(dir)
		if len(ents) != 0 {
			t.Errorf("%s: left files behind: %v", name, ents)
		}
	}
}

func TestFetchReleaseVerifies(t *testing.T) {
	ci, maint := NewTestSigner(1), NewTestSigner(2)
	keys := KeySet{CI: []PublicKey{ci.Public()}, Maint: []PublicKey{maint.Public()}, Pointer: []PublicKey{NewTestSigner(3).Public()}}
	mb, cs, ms := signed(t, testManifest(), ci, maint)
	src := memSource{"0.5.0/manifest.json": mb, "0.5.0/manifest.ci.sig": cs, "0.5.0/manifest.maint.sig": ms}
	m, raw, err := FetchRelease(context.Background(), src, keys, "0.5.0")
	if err != nil || m.Version != "0.5.0" || !bytes.Equal(raw, mb) {
		t.Fatalf("FetchRelease: %v %+v", err, m)
	}
	delete(src, "0.5.0/manifest.maint.sig")
	if _, _, err := FetchRelease(context.Background(), src, keys, "0.5.0"); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("missing maint sig: %v", err)
	}
}

// TestFetchLatestBranches is R24/R17: FetchLatest's failure branches, which
// the freeze alert classifies. A missing pointer or pointer signature is
// ErrNoPointer (freeze signature), an expired pointer ErrExpired, a pointer
// for another channel ErrWrongChannel, and a plain transport error is passed
// through as neither.
func TestFetchLatestBranches(t *testing.T) {
	ci, maint, ptr := NewTestSigner(1), NewTestSigner(2), NewTestSigner(3)
	keys := keysFor(ci, maint, ptr)
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	mk := func(channel string, issued, expires time.Time) []byte {
		b, _ := json.Marshal(Pointer{Schema: 1, Product: "trinetra", Channel: channel, Version: "0.5.0",
			Issued: issued.Format(time.RFC3339), Expires: expires.Format(time.RFC3339)})
		return b
	}
	good := mk("stable", now.Add(-time.Hour), now.Add(24*time.Hour))
	expired := mk("stable", now.Add(-10*24*time.Hour), now.Add(-time.Hour))
	other := mk("beta", now.Add(-time.Hour), now.Add(24*time.Hour))

	if p, err := FetchLatest(context.Background(), memSource{"channels/stable.json": good, "channels/stable.json.sig": ptr.SignPointer(good)}, keys, "stable", now); err != nil || p.Version != "0.5.0" {
		t.Fatalf("good pointer: %+v %v", p, err)
	}
	for name, c := range map[string]struct {
		src  Source
		want error
	}{
		"missing pointer":   {memSource{}, ErrNoPointer},
		"missing signature": {memSource{"channels/stable.json": good}, ErrNoPointer},
		"expired":           {memSource{"channels/stable.json": expired, "channels/stable.json.sig": ptr.SignPointer(expired)}, ErrExpired},
		"channel mismatch":  {memSource{"channels/stable.json": other, "channels/stable.json.sig": ptr.SignPointer(other)}, ErrWrongChannel},
		"bad signature":     {memSource{"channels/stable.json": good, "channels/stable.json.sig": ci.SignPointer(good)}, ErrBadSignature},
	} {
		if _, err := FetchLatest(context.Background(), c.src, keys, "stable", now); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	netErr := errors.New("dial tcp: connection refused")
	_, err := FetchLatest(context.Background(), failSource{netErr}, keys, "stable", now)
	if !errors.Is(err, netErr) || errors.Is(err, ErrNoPointer) {
		t.Fatalf("transport error: %v", err)
	}
}

type failSource struct{ err error }

func (f failSource) ReleaseAsset(context.Context, string, string) (io.ReadCloser, error) {
	return nil, f.err
}
func (f failSource) ChannelAsset(context.Context, string) (io.ReadCloser, error) { return nil, f.err }
