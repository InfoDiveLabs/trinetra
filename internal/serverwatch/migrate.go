// Package serverwatch: migrate.go implements the `serverwatch migrate`
// subcommand (s9 in docs/DESIGN-storage.md): a one-shot importer that reads
// every legacy JSONL sample/downtime record written by the old Store
// (store.go) and re-appends it into the configured SampleStore, then
// archives the legacy files so a second run is a no-op.
package serverwatch

import (
	"fmt"
	"os"
	"time"

	"serverwatch/internal/config"
)

// migrateLegacy imports every legacy JSONL sample and downtime event found
// under oldDir (the directory the old Store roots its samples/ dir and
// downtime.jsonl in) into store, using the same fastMetricSet+slowMetricSet
// key mapping the live daemon uses so migrated history matches live data.
// After import it calls store.Downsample(now) so any rows completed enough
// to roll up do so immediately. Finally it archives the legacy files by
// renaming them to a "*.migrated" sibling (samples/ -> samples.migrated/,
// downtime.jsonl -> downtime.jsonl.migrated) rather than deleting them, so
// the import is safe to re-run: with the legacy paths gone, a second call
// finds nothing under oldDir and returns (0, 0, nil).
func migrateLegacy(store SampleStore, oldDir string, now int64) (samples, events int, err error) {
	old := NewStore(oldDir, realClock{})
	samplesDir := old.samplesDir()
	downPath := old.downPath()

	smps, err := old.SamplesSince(0)
	if err != nil {
		return 0, 0, fmt.Errorf("read legacy samples: %w", err)
	}
	for _, smp := range smps {
		snap := Snapshot{
			TS:      smp.TS,
			CPU:     smp.CPU,
			MemPct:  smp.MemPct,
			SwapPct: smp.SwapPct,
			Load1:   smp.Load1,
			TempC:   smp.TempC,
			Disks:   smp.Disks,
		}
		ms := fastMetricSet(snap)
		for k, v := range slowMetricSet(snap) {
			ms[k] = v
		}
		if err := store.Append(smp.TS, ms); err != nil {
			return samples, events, fmt.Errorf("append sample ts=%d: %w", smp.TS, err)
		}
		samples++
	}

	evs, err := old.DownSince(0)
	if err != nil {
		return samples, events, fmt.Errorf("read legacy downtime: %w", err)
	}
	for _, e := range evs {
		if err := store.AppendEvent(e); err != nil {
			return samples, events, fmt.Errorf("append event start=%d: %w", e.Start, err)
		}
		events++
	}

	if samples > 0 || events > 0 {
		if err := store.Downsample(now); err != nil {
			return samples, events, fmt.Errorf("downsample: %w", err)
		}
	}

	if _, statErr := os.Stat(samplesDir); statErr == nil {
		if err := os.Rename(samplesDir, samplesDir+".migrated"); err != nil {
			return samples, events, fmt.Errorf("archive %s: %w", samplesDir, err)
		}
	}
	if _, statErr := os.Stat(downPath); statErr == nil {
		if err := os.Rename(downPath, downPath+".migrated"); err != nil {
			return samples, events, fmt.Errorf("archive %s: %w", downPath, err)
		}
	}

	return samples, events, nil
}

// cmdMigrate opens the configured SampleStore and imports every legacy JSONL
// record found in stateDir into it (see migrateLegacy). It is safe to run
// more than once: after the first successful run the legacy files are
// archived, so subsequent runs find nothing to import and say so.
func cmdMigrate(args []string) int {
	cfg, err := loadCfg()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	store, err := openConfiguredStore(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "open sample store:", err)
		return 1
	}
	defer store.Close()

	samples, events, err := migrateLegacy(store, stateDir, time.Now().Unix())
	if err != nil {
		fmt.Fprintln(stderr, "migrate:", err)
		return 1
	}
	if samples == 0 && events == 0 {
		fmt.Fprintln(stdout, "nothing to migrate")
		return 0
	}
	fmt.Fprintf(stdout, "migrated %d samples, %d events; legacy files archived (*.migrated)\n", samples, events)
	return 0
}

// openConfiguredStore opens the SampleStore backend/retention cfg describes,
// rooted at stateDir. Mirrors the store-opening logic in cmdDaemon
// (daemon.go) so migrate/dump see exactly the same store the daemon writes
// to; a parse failure on the (already-validated) retention duration strings
// falls back to the backend's built-in defaults rather than failing.
func openConfiguredStore(cfg *config.Config) (SampleStore, error) {
	rawRet, _ := time.ParseDuration(cfg.Storage.RawRetention)
	rollupRet, _ := time.ParseDuration(cfg.Storage.RollupRetention)
	return OpenStore(cfg.Storage.Backend, stateDir, StoreOptions{
		RawRetention:    rawRet,
		RollupRetention: rollupRet,
		EventRetention:  rollupRet,
	})
}
