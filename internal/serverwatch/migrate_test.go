package serverwatch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateLegacyImportsSamplesAndEvents(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	old := NewStore(dir, fixedClock{now})

	s1 := Sample{TS: now.Add(-2 * time.Hour).Unix(), CPU: 10, MemPct: 20, SwapPct: 1, Load1: 0.5, TempC: 40, Disks: map[string]float64{"/": 55}}
	s2 := Sample{TS: now.Add(-1 * time.Hour).Unix(), CPU: 15, MemPct: 25, SwapPct: 2, Load1: 0.6, TempC: 41, Disks: map[string]float64{"/": 56, "/data": 70}}
	if err := old.AppendSample(s1); err != nil {
		t.Fatal(err)
	}
	if err := old.AppendSample(s2); err != nil {
		t.Fatal(err)
	}

	ev := DownEvent{Type: "power_down", Start: now.Add(-3 * time.Hour).Unix(), End: now.Add(-2*time.Hour - 30*time.Minute).Unix(), DurationSec: 1800}
	if err := old.AppendDown(ev); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	samples, events, err := migrateLegacy(store, dir, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if samples != 2 {
		t.Fatalf("samples = %d, want 2", samples)
	}
	if events != 1 {
		t.Fatalf("events = %d, want 1", events)
	}

	pts, err := store.Query("cpu", 0, now.Unix(), ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || pts[0].Avg != 10 || pts[1].Avg != 15 {
		t.Fatalf("cpu points = %+v", pts)
	}

	diskPts, err := store.Query("disk:/data", 0, now.Unix(), ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(diskPts) != 1 || diskPts[0].Avg != 70 {
		t.Fatalf("disk:/data points = %+v, want one point of 70", diskPts)
	}

	loadPts, err := store.Query("load1", 0, now.Unix(), ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(loadPts) != 2 {
		t.Fatalf("load1 points = %+v, want 2 (fastMetricSet mapping)", loadPts)
	}

	evs, err := store.Events(0, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "power_down" {
		t.Fatalf("events = %+v", evs)
	}

	// Old files archived (renamed), never deleted.
	if _, err := os.Stat(filepath.Join(dir, "samples")); !os.IsNotExist(err) {
		t.Fatalf("samples dir should have been renamed away, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "samples.migrated")); err != nil {
		t.Fatalf("samples.migrated missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "downtime.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("downtime.jsonl should have been renamed away, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "downtime.jsonl.migrated")); err != nil {
		t.Fatalf("downtime.jsonl.migrated missing: %v", err)
	}

	// Re-running is a no-op: legacy files are gone, so nothing to import.
	samples2, events2, err := migrateLegacy(store, dir, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if samples2 != 0 || events2 != 0 {
		t.Fatalf("second run should import nothing, got samples=%d events=%d", samples2, events2)
	}
}

func TestMigrateLegacyNothingToMigrate(t *testing.T) {
	dir := t.TempDir() // never touched by an old Store: no samples/, no downtime.jsonl
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	samples, events, err := migrateLegacy(store, dir, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if samples != 0 || events != 0 {
		t.Fatalf("expected nothing to migrate, got samples=%d events=%d", samples, events)
	}
}

func TestMigrateCLISmokeEmptyStateDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	stateDir = filepath.Join(dir, "state")

	var out bytes.Buffer
	stdout = &out
	if code := Main([]string{"migrate"}); code != 0 {
		t.Fatalf("migrate on empty state dir exit=%d, stderr unavailable, stdout=%q", code, out.String())
	}
	if got := out.String(); got != "nothing to migrate\n" {
		t.Fatalf("stdout = %q, want %q", got, "nothing to migrate\n")
	}
}
