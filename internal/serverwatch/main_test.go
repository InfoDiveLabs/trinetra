package serverwatch

import (
	"bytes"
	"os"
	"path/filepath"
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

func TestUnknownCommand(t *testing.T) {
	if code := Main([]string{"frobnicate"}); code == 0 {
		t.Fatal("unknown command should be non-zero exit")
	}
}
