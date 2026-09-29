// internal/update/fetch_test.go
package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
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
