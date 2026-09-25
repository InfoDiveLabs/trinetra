package trinetra

import (
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func withFleetOnDisk(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prev := cfgPath
	cfgPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { cfgPath = prev })
	c := config.Default()
	c.Fleet.Role = config.RoleChild
	c.Fleet.MasterURL = "https://m.example:9443"
	c.Fleet.CAPin = "sha256:abc"
	c.Fleet.NodeID = "0123456789abcdef0123456789abcdef"
	c.Fleet.Address = "a.example"
	if err := saveCfg(c); err != nil {
		t.Fatal(err)
	}
}

func assertFleetOnDiskKept(t *testing.T) {
	t.Helper()
	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fleet.Role != config.RoleChild || got.Fleet.MasterURL != "https://m.example:9443" ||
		got.Fleet.CAPin != "sha256:abc" || got.Fleet.NodeID != "0123456789abcdef0123456789abcdef" || got.Fleet.Address != "a.example" {
		t.Fatalf("fleet keys on disk changed: %+v", got.Fleet)
	}
}

// A config saved by an older plugin (or a daemon holding a pre-`fleet join`
// copy in memory) has an empty fleet block; saving it must not wipe the
// fleet identity `serverwatch fleet join` wrote to disk.
func TestApplyConfigWithEmptyFleetBlockKeepsOnDiskFleetKeys(t *testing.T) {
	withFleetOnDisk(t)
	c := config.Default()
	c.Thresholds.CPUPct = 42
	if err := newFileAPI(t.TempDir(), config.Default()).ApplyConfig(c); err != nil {
		t.Fatal(err)
	}
	assertFleetOnDiskKept(t)
	got, _ := config.Load(cfgPath)
	if got.Thresholds.CPUPct != 42 {
		t.Fatal("the rest of the config was not saved")
	}
}

// Only `serverwatch fleet ...` commands may change the fleet identity keys.
func TestApplyConfigCannotSetFleetRole(t *testing.T) {
	withFleetOnDisk(t)
	c := config.Default()
	c.Fleet.Role = config.RoleMaster
	c.Fleet.MasterURL = "https://evil.example"
	c.Fleet.Listen = ":9555" // not an identity key: still editable
	if err := saveDaemonCfg(c); err != nil {
		t.Fatal(err)
	}
	assertFleetOnDiskKept(t)
	if c.Fleet.Role != config.RoleChild {
		t.Fatalf("in-memory config not overlaid: role %q", c.Fleet.Role)
	}
	got, _ := config.Load(cfgPath)
	if got.Fleet.Listen != ":9555" {
		t.Fatalf("fleet.listen = %q, want the saved value", got.Fleet.Listen)
	}
}
