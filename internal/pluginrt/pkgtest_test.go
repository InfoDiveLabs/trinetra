package pluginrt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type rawEntry struct {
	hdr  tar.Header
	body []byte
}

func buildRaw(t *testing.T, ents []rawEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range ents {
		h := e.hdr
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildPackage(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var ents []rawEntry
	for _, n := range names {
		ents = append(ents, rawEntry{hdr: tar.Header{Name: n}, body: files[n]})
	}
	return buildRaw(t, ents)
}

type manifestOpt func(*plugin.Manifest)

var testBinary = []byte("#!/bin/sh\nexit 0\n")

func testManifest(id string, opts ...manifestOpt) plugin.Manifest {
	sum := sha256.Sum256(testBinary)
	m := plugin.Manifest{
		Schema: 1, ID: id, Name: "Test " + id, Version: "1.0.0", Publisher: "tester",
		Description: "d", HostAPI: "^1.0", Permissions: []plugin.Permission{plugin.PermMetricsRead},
		Binaries: []plugin.Binary{{OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(testBinary))}},
	}
	for _, o := range opts {
		o(&m)
	}
	return m
}

var testPub, testPriv = func() (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	k := ed25519.NewKeyFromSeed(seed)
	return k.Public().(ed25519.PublicKey), k
}()

func signManifest(mb []byte) []byte {
	return ed25519.Sign(testPriv, append([]byte(plugin.ManifestSigPrefix), mb...))
}

func packageFiles(t *testing.T, m plugin.Manifest, signed bool) map[string][]byte {
	t.Helper()
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f := map[string][]byte{"plugin.json": mb, plugin.BinaryPath("linux", "amd64", m.ID): testBinary}
	if signed {
		f["plugin.json.sig"] = signManifest(mb)
	}
	return f
}

type fakeRegistry struct {
	revoked map[string]string
	latest  map[string]string
	resolve func(id, version string) (plugin.PackageRef, error)
	onRev   func([]plugin.Revocation)
}

func (r *fakeRegistry) VerifyManifest(m, sig []byte) error {
	if len(sig) == ed25519.SignatureSize && ed25519.Verify(testPub, append([]byte(plugin.ManifestSigPrefix), m...), sig) {
		return nil
	}
	return plugin.ErrBadSignature
}

func (r *fakeRegistry) Resolve(id, version, goos, goarch string) (plugin.PackageRef, error) {
	if r.resolve != nil {
		return r.resolve(id, version)
	}
	return plugin.PackageRef{}, plugin.ErrNotFound
}

func (r *fakeRegistry) Latest(id string) (string, bool) { v, ok := r.latest[id]; return v, ok }

func (r *fakeRegistry) Revocation(id, version string) (string, bool) {
	s, ok := r.revoked[id+"@"+version]
	return s, ok
}

func (r *fakeRegistry) OnRevoke(f func([]plugin.Revocation)) { r.onRev = f }

func (r *fakeRegistry) Catalog(bool) (plugin.Catalog, error) { return plugin.Catalog{}, nil }

