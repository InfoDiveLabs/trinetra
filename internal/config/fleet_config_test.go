package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFleetDefaultsAreSolo(t *testing.T) {
	c := Default()
	if c.FleetRole() != RoleSolo {
		t.Fatalf("FleetRole() = %q, want solo", c.FleetRole())
	}
	if c.FleetListen() != ":9443" {
		t.Fatalf("FleetListen() = %q", c.FleetListen())
	}
	if c.FleetOutboxMaxBytes() != 512<<20 {
		t.Fatalf("FleetOutboxMaxBytes() = %d", c.FleetOutboxMaxBytes())
	}
	if c.FleetNodeDownAfter() != 2*time.Minute {
		t.Fatalf("FleetNodeDownAfter() = %v", c.FleetNodeDownAfter())
	}
	if v, ok := c.Get("fleet.role"); !ok || v != "solo" {
		t.Fatalf("Get(fleet.role) = %q,%v", v, ok)
	}
}

func TestFleetLegacyConfigWithoutBlockLoadsSolo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeRaw(t, p, `{"sample_interval":60,"telegram":{}}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.FleetRole() != RoleSolo {
		t.Fatalf("legacy config role = %q, want solo", c.FleetRole())
	}
}

func TestFleetSettableKeysValidate(t *testing.T) {
	c := Default()
	if err := c.Set("fleet.listen", "0.0.0.0:9555"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("fleet.listen", "nope"); err == nil {
		t.Fatal("bad listen accepted")
	}
	if err := c.Set("fleet.outbox_max_mb", "64"); err != nil {
		t.Fatal(err)
	}
	if c.FleetOutboxMaxBytes() != 64<<20 {
		t.Fatalf("outbox bytes = %d", c.FleetOutboxMaxBytes())
	}
	if err := c.Set("fleet.outbox_max_mb", "8"); err == nil {
		t.Fatal("outbox_max_mb below 16 accepted")
	}
	if err := c.Set("fleet.node_down_after", "90s"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("fleet.node_down_after", "10s"); err == nil {
		t.Fatal("node_down_after below 30s accepted")
	}
}

func TestFleetManagedKeysRefuseConfigSet(t *testing.T) {
	c := Default()
	for _, k := range []string{"fleet.role", "fleet.address", "fleet.master_url", "fleet.ca_pin", "fleet.node_id"} {
		err := c.Set(k, "x")
		if err == nil || !strings.Contains(err.Error(), "trinetra fleet") {
			t.Fatalf("Set(%s) err = %v, want pointer to `trinetra fleet`", k, err)
		}
	}
}

func TestFleetRoundTripsThroughSaveLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	c := Default()
	c.Fleet.Role = RoleChild
	c.Fleet.MasterURL = "https://m:9443"
	c.Fleet.CAPin = "sha256:abc"
	c.Fleet.NodeID = "n1"
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.FleetRole() != RoleChild || got.Fleet.MasterURL != "https://m:9443" || got.Fleet.CAPin != "sha256:abc" || got.Fleet.NodeID != "n1" {
		t.Fatalf("round trip lost fleet fields: %+v", got.Fleet)
	}
}

func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
