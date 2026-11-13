package pluginrt

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func readPkg(t *testing.T, m plugin.Manifest, signed bool) Package {
	t.Helper()
	p, err := ReadPackage(bytes.NewReader(buildPackage(t, packageFiles(t, m, signed))), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerifySignedIsVerified(t *testing.T) {
	tr, err := Verify(readPkg(t, testManifest("demo"), true), &fakeRegistry{}, "linux", "amd64", "1.0.0")
	if err != nil || tr != plugin.TrustVerified {
		t.Fatalf("%v %v", tr, err)
	}
}

func TestVerifyUnsignedIsUnverified(t *testing.T) {
	tr, err := Verify(readPkg(t, testManifest("demo"), false), &fakeRegistry{}, "linux", "amd64", "1.0.0")
	if err != nil || tr != plugin.TrustUnverified {
		t.Fatalf("%v %v", tr, err)
	}
	tr, err = Verify(readPkg(t, testManifest("demo"), true), nil, "linux", "amd64", "1.0.0")
	if err != nil || tr != plugin.TrustUnverified {
		t.Fatalf("nil registry: %v %v", tr, err)
	}
}

func TestVerifyUnknownKeyIsUnverified(t *testing.T) {
	p := readPkg(t, testManifest("demo"), true)
	p.Sigs.Maint = bytes.Repeat([]byte{1}, 64)
	tr, err := Verify(p, &fakeRegistry{}, "linux", "amd64", "1.0.0")
	if err != nil || tr != plugin.TrustUnverified {
		t.Fatalf("%v %v", tr, err)
	}
}

func TestVerifyBinaryHashMismatch(t *testing.T) {
	p := readPkg(t, testManifest("demo"), true)
	if err := os.WriteFile(filepath.Join(p.Dir, "bin", "linux-amd64", "trinetra-plugin-demo"), []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(p, &fakeRegistry{}, "linux", "amd64", "1.0.0"); err == nil {
		t.Fatal("expected mismatch")
	}
}

func TestVerifyMissingPlatformBinary(t *testing.T) {
	if _, err := Verify(readPkg(t, testManifest("demo"), true), &fakeRegistry{}, "linux", "arm64", "1.0.0"); err == nil {
		t.Fatal("expected error")
	}
}

func TestVerifyIncompatibleHostAPI(t *testing.T) {
	m := testManifest("demo", func(m *plugin.Manifest) { m.HostAPI = "^2.0" })
	_, err := Verify(readPkg(t, m, true), &fakeRegistry{}, "linux", "amd64", "1.0.0")
	if !errors.Is(err, plugin.ErrIncompatible) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyMinTrinetra(t *testing.T) {
	m := testManifest("demo", func(m *plugin.Manifest) { m.MinTrinetra = "2.0.0" })
	p := readPkg(t, m, true)
	if _, err := Verify(p, &fakeRegistry{}, "linux", "amd64", "1.0.0"); err == nil {
		t.Fatal("expected min_trinetra error")
	}
	for _, v := range []string{"dev", "", "2.1.0"} {
		if _, err := Verify(p, &fakeRegistry{}, "linux", "amd64", v); err != nil {
			t.Fatalf("core %q: %v", v, err)
		}
	}
}

func TestVerifyRevoked(t *testing.T) {
	reg := &fakeRegistry{revoked: map[string]string{"demo@1.0.0": "malware"}}
	_, err := Verify(readPkg(t, testManifest("demo"), true), reg, "linux", "amd64", "1.0.0")
	if !errors.Is(err, plugin.ErrRevoked) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyCIOnlyIsUnverified(t *testing.T) {
	p := readPkg(t, testManifest("demo"), true)
	p.Sigs.Maint = nil
	tr, err := Verify(p, &fakeRegistry{}, "linux", "amd64", "1.0.0")
	if err != nil || tr != plugin.TrustUnverified {
		t.Fatalf("a CI signature alone must not make a plugin verified: %v %v", tr, err)
	}
}
