package pluginrt

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func newStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	state, lib := filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "lib")
	s, err := OpenStore(state, lib)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPlatform("linux", "amd64")
	return s, state, lib
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	rec := plugin.Installed{ID: "demo", Version: "1.0.0", State: plugin.StateInstalled, Trust: plugin.TrustVerified, Manifest: testManifest("demo")}
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("demo")
	if err != nil || got.Version != "1.0.0" || got.Manifest.Name != rec.Manifest.Name {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.Save(plugin.Installed{ID: "a-one"}); err != nil {
		t.Fatal(err)
	}
	l, _ := s.List()
	if len(l) != 2 || l[0].ID != "a-one" || l[1].ID != "demo" {
		t.Fatalf("list = %+v", l)
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestStoreFileModes(t *testing.T) {
	s, state, lib := newStore(t)
	_ = s.Save(plugin.Installed{ID: "demo"})
	_ = s.WriteConfig("demo", plugin.ConfigResponse{})
	if _, err := s.WriteToken("demo", "tok"); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(state, "plugins", "demo")
	for _, f := range []string{"record.json", "config.json", "token"} {
		if m := mode(t, filepath.Join(d, f)); m != 0o600 {
			t.Errorf("%s mode %v", f, m)
		}
	}
	if m := mode(t, d); m != 0o700 {
		t.Errorf("dir mode %v", m)
	}
	if m := mode(t, filepath.Join(state, "plugins")); m != 0o700 {
		t.Errorf("root mode %v", m)
	}
	if m := mode(t, lib); m != 0o755 {
		t.Errorf("lib mode %v", m)
	}
}

func TestStoreLoadMissingIsNotFound(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.Load("nope"); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Load("../etc"); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("traversal id: %v", err)
	}
}

func TestStoreConfigRevisionIncrements(t *testing.T) {
	s, _, _ := newStore(t)
	cfg := plugin.ConfigResponse{Values: map[string]json.RawMessage{"a": json.RawMessage(`1`)}}
	for want := int64(1); want <= 3; want++ {
		if err := s.WriteConfig("demo", cfg); err != nil {
			t.Fatal(err)
		}
		got, _ := s.ReadConfig("demo")
		if got.Revision != want || string(got.Values["a"]) != "1" {
			t.Fatalf("got %+v want rev %d", got, want)
		}
	}
	if c, err := s.ReadConfig("fresh"); err != nil || c.Revision != 0 || c.Values == nil {
		t.Fatalf("fresh config = %+v %v", c, err)
	}
}

func installTestPackage(t *testing.T, s *Store, version string) (string, Package) {
	t.Helper()
	m := testManifest("demo", func(m *plugin.Manifest) { m.Version = version })
	dir, _, err := s.Staging()
	if err != nil {
		t.Fatal(err)
	}
	p, err := ReadPackage(bytes.NewReader(buildPackage(t, packageFiles(t, m, false))), dir)
	if err != nil {
		t.Fatal(err)
	}
	arch := filepath.Join(t.TempDir(), "a.tar.gz")
	_ = os.WriteFile(arch, []byte("archive"), 0o600)
	p.Archive = arch
	bin, err := s.Install(p, version)
	if err != nil {
		t.Fatal(err)
	}
	return bin, p
}

func TestStoreInstallLayoutPassesVerifyPluginOwnerRules(t *testing.T) {
	s, _, lib := newStore(t)
	bin, _ := installTestPackage(t, s, "1.0.0")
	if m := mode(t, bin); m&0o022 != 0 || m&0o100 == 0 {
		t.Errorf("binary mode %v", m)
	}
	for _, d := range []string{lib, filepath.Join(lib, "demo"), s.VersionDir("demo", "1.0.0"), filepath.Dir(bin)} {
		if m := mode(t, d); m != 0o755 {
			t.Errorf("%s mode %v", d, m)
		}
	}
	if bin != filepath.Join(s.VersionDir("demo", "1.0.0"), "bin", "linux-amd64", "trinetra-plugin-demo") {
		t.Errorf("bin = %s", bin)
	}
	if b, _ := os.ReadFile(filepath.Join(s.VersionDir("demo", "1.0.0"), ArchiveName)); string(b) != "archive" {
		t.Errorf("archive = %q", b)
	}
}

func TestStorePruneKeepsCurrent(t *testing.T) {
	s, _, _ := newStore(t)
	installTestPackage(t, s, "1.0.0")
	installTestPackage(t, s, "1.1.0")
	if err := s.PruneVersions("demo", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.VersionDir("demo", "1.0.0")); !os.IsNotExist(err) {
		t.Fatal("old version kept")
	}
	if _, err := os.Stat(s.VersionDir("demo", "1.1.0")); err != nil {
		t.Fatal("current version removed")
	}
	if err := s.Delete("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.VersionDir("demo", "1.1.0")); !os.IsNotExist(err) {
		t.Fatal("delete left files")
	}
}

func TestStoreAuditAppends(t *testing.T) {
	s, state, _ := newStore(t)
	_ = s.AppendAudit(plugin.AuditEntry{TS: 1, Action: "plugin.install", Plugin: "demo"})
	_ = s.AppendAudit(plugin.AuditEntry{TS: 2, Action: "plugin.enable", Plugin: "demo"})
	b, _ := os.ReadFile(filepath.Join(state, "plugins", "audit.jsonl"))
	if bytes.Count(b, []byte("\n")) != 2 || !bytes.Contains(b, []byte("plugin.enable")) {
		t.Fatalf("audit = %s", b)
	}
	if m := mode(t, filepath.Join(state, "plugins", "audit.jsonl")); m != 0o600 {
		t.Errorf("audit mode %v", m)
	}
}
