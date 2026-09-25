// Package trinetra: migrate.go implements the `trinetra migrate`
// subcommand (s9 in docs/handbook/09-storage-and-data-model.md): a one-shot importer that reads
// every legacy JSONL sample/downtime record written by the old Store
// (store.go) and re-appends it into the configured SampleStore, then
// archives the legacy files.
//
// Idempotency note: archiving the legacy files alone does NOT make migrate
// safe to re-run -- a second run would re-read whatever legacy data is on disk
// and re-import (duplicate) it. To make migrate a true one-shot we drop a
// marker file (<oldDir>/.migrated, containing the unix time of the import)
// after the first successful import and refuse to import again while it
// exists. `--force` bypasses the marker for a deliberate re-import.
package trinetra

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// migratedMarker is the sentinel filename written under oldDir after a
// successful import; its presence makes a subsequent migrateLegacy a no-op
// (unless force is set). It holds the import's unix time as decimal text.
const migratedMarker = ".migrated"

func markerPath(oldDir string) string { return filepath.Join(oldDir, migratedMarker) }

// readMarker returns the recorded migration time and true if the marker
// exists and is readable, else (zero, false).
func readMarker(oldDir string) (time.Time, bool) {
	b, err := os.ReadFile(markerPath(oldDir))
	if err != nil {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(string(trimSpace(b)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// migrateLegacy imports every legacy JSONL sample and downtime event found
// under oldDir (the directory the old Store roots its samples/ dir and
// downtime.jsonl in) into store, using the same fastMetricSet+slowMetricSet
// key mapping the live daemon uses so migrated history matches live data.
//
// It is a guarded one-shot: unless force is set, an existing marker file
// (see migratedMarker) short-circuits the whole operation to (0, 0, nil)
// with skipped=true -- this is what prevents a double-import of the same
// legacy files on a second run.
//
// When it does import, it: appends every sample/event, calls
// store.Downsample(now), archives the legacy files by renaming them to a
// "*.migrated" sibling (never deleting; if that sibling already exists it
// uses a timestamped "*.migrated.<now>" name so an earlier archive is never
// clobbered), then writes the marker. It writes the marker (and archives)
// only when something was actually imported, so a bare oldDir with no legacy
// data leaves no marker and stays re-runnable.
func migrateLegacy(store SampleStore, oldDir string, now int64, force bool) (samples, events int, skipped bool, err error) {
	if !force {
		if _, ok := readMarker(oldDir); ok {
			return 0, 0, true, nil
		}
	}

	old := NewStore(oldDir, realClock{})
	samplesDir := old.samplesDir()
	downPath := old.downPath()

	smps, err := old.SamplesSince(0)
	if err != nil {
		return 0, 0, false, fmt.Errorf("read legacy samples: %w", err)
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
			return samples, events, false, fmt.Errorf("append sample ts=%d: %w", smp.TS, err)
		}
		samples++
	}

	evs, err := old.DownSince(0)
	if err != nil {
		return samples, events, false, fmt.Errorf("read legacy downtime: %w", err)
	}
	for _, e := range evs {
		if err := store.AppendEvent(e); err != nil {
			return samples, events, false, fmt.Errorf("append event start=%d: %w", e.Start, err)
		}
		events++
	}

	if samples == 0 && events == 0 {
		// Nothing on disk to migrate: leave no marker so a later run (once the
		// daemon has actually produced legacy data) can still import.
		return 0, 0, false, nil
	}

	if err := store.Downsample(now); err != nil {
		return samples, events, false, fmt.Errorf("downsample: %w", err)
	}

	if _, statErr := os.Stat(samplesDir); statErr == nil {
		if err := os.Rename(samplesDir, archiveDest(samplesDir, now)); err != nil {
			return samples, events, false, fmt.Errorf("archive %s: %w", samplesDir, err)
		}
	}
	if _, statErr := os.Stat(downPath); statErr == nil {
		if err := os.Rename(downPath, archiveDest(downPath, now)); err != nil {
			return samples, events, false, fmt.Errorf("archive %s: %w", downPath, err)
		}
	}

	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		return samples, events, false, fmt.Errorf("write marker: %w", err)
	}
	if err := os.WriteFile(markerPath(oldDir), []byte(strconv.FormatInt(now, 10)), 0o644); err != nil {
		return samples, events, false, fmt.Errorf("write marker: %w", err)
	}

	return samples, events, false, nil
}

// archiveDest returns the rename target for archiving base: "<base>.migrated"
// normally, or "<base>.migrated.<now>" when the plain target already exists
// (from an earlier --force run), so an existing archive is never overwritten
// -- preserving the rename-not-delete audit trail.
func archiveDest(base string, now int64) string {
	dst := base + ".migrated"
	if _, err := os.Stat(dst); err == nil {
		dst = fmt.Sprintf("%s.migrated.%d", base, now)
	}
	return dst
}

// cmdMigrate opens the configured SampleStore and imports every legacy JSONL
// record found in stateDir into it (see migrateLegacy). It is a guarded
// one-shot: once a marker file records a successful import it refuses to run
// again (printing when it was migrated) unless --force is passed, so a second
// invocation can never cause a duplicate import of the legacy files.
func cmdMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "re-import even if a previous migration marker exists")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !*force {
		if t, ok := readMarker(stateDir); ok {
			fmt.Fprintf(stdout, "already migrated at %s; nothing to do (use --force to re-import)\n", t.Format(time.RFC3339))
			return 0
		}
	}

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

	samples, events, skipped, err := migrateLegacy(store, stateDir, time.Now().Unix(), *force)
	if err != nil {
		fmt.Fprintln(stderr, "migrate:", err)
		return 1
	}
	if skipped {
		// Marker appeared between our check above and the import (race); treat
		// as already-migrated rather than importing.
		fmt.Fprintln(stdout, "already migrated; nothing to do (use --force to re-import)")
		return 0
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
