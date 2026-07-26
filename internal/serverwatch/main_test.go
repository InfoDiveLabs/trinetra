package serverwatch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSetGetViaCLI(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")

	var out bytes.Buffer
	stdout = &out
	if code := Main([]string{"config", "set", "sample_interval", "30"}); code != 0 {
		t.Fatalf("set exit=%d", code)
	}
	out.Reset()
	if code := Main([]string{"config", "get", "sample_interval"}); code != 0 {
		t.Fatalf("get exit=%d", code)
	}
	if got := out.String(); got != "30\n" {
		t.Fatalf("get output = %q, want 30", got)
	}
	// Persisted?
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

// TestConfigGetAllShowsAllCollectKeys asserts the full-dump `config get` (no
// key argument) surfaces every collect.* toggle, including ones left unset
// (nil pointer -> "use the default"). A raw json.Marshal of Config would omit
// unset *bool fields entirely (they carry `omitempty`), so cmdConfig must
// render effective values for display rather than the raw struct.
func TestConfigGetAllShowsAllCollectKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")

	var out bytes.Buffer
	stdout = &out
	if code := Main([]string{"config", "get"}); code != 0 {
		t.Fatalf("get exit=%d", code)
	}
	got := out.String()
	for _, key := range []string{"container_stats", "net_throughput", "services", "processes", "smart_attrs"} {
		if !strings.Contains(got, key) {
			t.Errorf("config get (full dump) missing %q; output:\n%s", key, got)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	if code := Main([]string{"frobnicate"}); code == 0 {
		t.Fatal("unknown command should be non-zero exit")
	}
}
