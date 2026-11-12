// Package plugintest builds a real, signed SDK plugin package for end-to-end tests.
package plugintest

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/registry/regtool"
)

// ID is the test plugin's id.
const ID = "e2e"

// WidgetText is a string the test plugin's "main" nav page always renders.
const WidgetText = "E2E-WIDGET-OK"

// Manifest is the test plugin's plugin.json before hashes are filled.
const Manifest = `{
  "schema": 1,
  "id": "e2e",
  "name": "E2E Probe",
  "version": "1.0.0",
  "publisher": "Trinetra tests",
  "description": "End-to-end test plugin.",
  "host_api": "^1.0",
  "permissions": ["metrics.publish", "alerts.raise"],
  "binaries": [],
  "config_schema": {
    "type": "object",
    "properties": {
      "mode": {"type": "string", "title": "Mode", "enum": ["run", "crash"], "default": "run"}
    }
  },
  "contributes": {
    "slots": [
      {"slot": "nav.page", "id": "main", "title": "E2E", "min_role": "viewer"}
    ]
  }
}
`

// Package is a built test plugin package.
type Package struct {
	Path     string
	Key      ed25519.PublicKey
	Manifest plugin.Manifest
}

// Build compiles the test plugin for the host platform, signs its manifest with a fresh key and packages it under dir.
func Build(t testing.TB, dir string) Package {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("plugintest: cannot locate source")
	}
	return buildPackage(t, dir, filepath.Join(filepath.Dir(self), "testdata", "e2eplugin"), ID, []byte(Manifest), "")
}

// BuildFrom builds the plugin in srcDir (its plugin.json and optional files/ directory) the way Build does.
func BuildFrom(t testing.TB, dir, srcDir string) Package {
	t.Helper()
	mb, err := os.ReadFile(filepath.Join(srcDir, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(mb, &head); err != nil {
		t.Fatal(err)
	}
	return buildPackage(t, dir, srcDir, head.ID, mb, filepath.Join(srcDir, "files"))
}

func buildPackage(t testing.TB, dir, src, id string, manifest []byte, filesDir string) Package {
	t.Helper()
	build := filepath.Join(dir, "build")
	bin := filepath.Join(build, filepath.FromSlash(plugin.BinaryPath(runtime.GOOS, runtime.GOARCH, id)))
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plugintest: go build: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(build, "plugin.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if filesDir != "" {
		if err := os.CopyFS(filepath.Join(build, "files"), os.DirFS(filesDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	m, err := regtool.FillHashes(build, "plugin.json")
	if err != nil {
		t.Fatalf("plugintest: hash: %v", err)
	}
	key := filepath.Join(dir, "signing.key")
	pub, err := regtool.Keygen(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := regtool.SignFile(key, "manifest", filepath.Join(build, "plugin.json"), filepath.Join(build, "plugin.json.sig")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, id+"-"+m.Version+".tar.gz")
	if _, _, err := regtool.Package(build, runtime.GOOS, runtime.GOARCH, out); err != nil {
		t.Fatalf("plugintest: package: %v", err)
	}
	return Package{Path: out, Key: pub, Manifest: m}
}

// ShortDir returns a fresh directory under /tmp, short enough for unix socket paths on macOS.
func ShortDir(t testing.TB) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "tp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}
