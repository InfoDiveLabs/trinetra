package trinetra

import (
	"bytes"
	"fmt"
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

	samples, events, skipped, err := migrateLegacy(store, dir, now.Unix(), false)
	if err != nil {
		t.Fatal(err)
	}
	if skipped {
		t.Fatal("first run should not be skipped")
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

	// Marker written after a successful import.
	if _, ok := readMarker(dir); !ok {
		t.Fatal("marker file .migrated should exist after import")
	}

	// The daemon dual-writes, so it can RECREATE the legacy samples/ with data
	// already in the SampleStore. Simulate that: re-add a sample via the old
	// Store, then re-run migrate. The marker must make it a no-op -- no
	// re-import, no duplicate points.
	if err := old.AppendSample(Sample{TS: now.Add(-30 * time.Minute).Unix(), CPU: 99, Disks: map[string]float64{"/": 60}}); err != nil {
		t.Fatal(err)
	}
	samples2, events2, skipped2, err := migrateLegacy(store, dir, now.Unix(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !skipped2 {
		t.Fatal("second run should be skipped due to marker")
	}
	if samples2 != 0 || events2 != 0 {
		t.Fatalf("second run should import nothing, got samples=%d events=%d", samples2, events2)
	}
	// No duplicate cpu points from the recreated legacy file.
	pts2, err := store.Query("cpu", 0, now.Unix(), ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts2) != 2 {
		t.Fatalf("cpu points after skipped re-run = %d, want 2 (no duplicates)", len(pts2))
	}
}

func TestMigrateLegacyForceReimports(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	old := NewStore(dir, fixedClock{now})
	if err := old.AppendSample(Sample{TS: now.Add(-1 * time.Hour).Unix(), CPU: 10}); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, _, _, err := migrateLegacy(store, dir, now.Unix(), false); err != nil {
		t.Fatal(err)
	}
	// Daemon recreates a fresh legacy sample.
	if err := old.AppendSample(Sample{TS: now.Add(-30 * time.Minute).Unix(), CPU: 20}); err != nil {
		t.Fatal(err)
	}
	// --force imports it despite the marker.
	samples, _, skipped, err := migrateLegacy(store, dir, now.Unix()+1, true)
	if err != nil {
		t.Fatal(err)
	}
	if skipped {
		t.Fatal("--force run should not be skipped")
	}
	if samples != 1 {
		t.Fatalf("force run imported %d samples, want 1", samples)
	}
}

func TestMigrateArchiveNeverClobbersExisting(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	old := NewStore(dir, fixedClock{now})
	if err := old.AppendSample(Sample{TS: now.Add(-1 * time.Hour).Unix(), CPU: 10}); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// First migration archives samples/ -> samples.migrated/.
	if _, _, _, err := migrateLegacy(store, dir, now.Unix(), false); err != nil {
		t.Fatal(err)
	}
	firstArchive := filepath.Join(dir, "samples.migrated")
	if _, err := os.Stat(firstArchive); err != nil {
		t.Fatalf("first archive missing: %v", err)
	}

	// Daemon recreates samples/, then a --force re-run must NOT clobber the
	// existing samples.migrated/ -- it archives to a timestamped name instead.
	if err := old.AppendSample(Sample{TS: now.Add(-20 * time.Minute).Unix(), CPU: 20}); err != nil {
		t.Fatal(err)
	}
	forceNow := now.Unix() + 42
	if _, _, _, err := migrateLegacy(store, dir, forceNow, true); err != nil {
		t.Fatal(err)
	}
	// Original archive still intact.
	if _, err := os.Stat(firstArchive); err != nil {
		t.Fatalf("original archive should still exist: %v", err)
	}
	// New timestamped archive created.
	tsArchive := fmt.Sprintf("%s.migrated.%d", filepath.Join(dir, "samples"), forceNow)
	if _, err := os.Stat(tsArchive); err != nil {
		t.Fatalf("timestamped archive %s missing: %v", tsArchive, err)
	}
}

func TestMigrateLegacyNothingToMigrate(t *testing.T) {
	dir := t.TempDir() // never touched by an old Store: no samples/, no downtime.jsonl
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	samples, events, skipped, err := migrateLegacy(store, dir, time.Now().Unix(), false)
	if err != nil {
		t.Fatal(err)
	}
	if skipped {
		t.Fatal("empty dir run should not be skipped (no marker)")
	}
	if samples != 0 || events != 0 {
		t.Fatalf("expected nothing to migrate, got samples=%d events=%d", samples, events)
	}
	// Nothing imported => no marker left behind, so a later real run can import.
	if _, ok := readMarker(dir); ok {
		t.Fatal("no marker should be written when there was nothing to migrate")
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
