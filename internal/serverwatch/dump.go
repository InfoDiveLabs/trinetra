// Package serverwatch: dump.go implements the `serverwatch dump` subcommand
// (s9 in docs/DESIGN-storage.md): export one metric's series from the
// configured SampleStore for humans or graphing tools.
package serverwatch

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"
)

// dumpSeries queries store for metric over [from, to] at resolution res and
// writes the result to w in the requested format ("csv" or "json"). csv
// writes a header row ("ts,min,avg,max") followed by one row per point, ts
// as a Unix timestamp (chosen over RFC3339 for simplicity: no timezone
// ambiguity, sorts as a plain integer). json writes a JSON array of points
// (Point's exported fields: TS/Min/Avg/Max), "[]" when there are none. A
// metric with no data in range is not an error in either format: csv prints
// just the header, json prints "[]".
func dumpSeries(w io.Writer, store SampleStore, metric string, from, to int64, res Resolution, format string) error {
	pts, err := store.Query(metric, from, to, res)
	if err != nil {
		return fmt.Errorf("query %s: %w", metric, err)
	}
	switch format {
	case "json":
		if pts == nil {
			pts = []Point{}
		}
		b, err := json.Marshal(pts)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	case "csv":
		if _, err := fmt.Fprintln(w, "ts,min,avg,max"); err != nil {
			return err
		}
		for _, p := range pts {
			if _, err := fmt.Fprintf(w, "%d,%g,%g,%g\n", p.TS, p.Min, p.Avg, p.Max); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown format %q (want csv|json)", format)
	}
}

// cmdDump parses `dump --metric <id> [--since 24h] [--res raw|1m] [--format csv|json]`
// and prints the result via dumpSeries.
func cmdDump(args []string) int {
	fs := flag.NewFlagSet("dump", flag.ContinueOnError)
	fs.SetOutput(stderr)
	metric := fs.String("metric", "", "metric id to dump, e.g. cpu, mem, disk:/ (required)")
	since := fs.String("since", "24h", "how far back to query, e.g. 24h, 30m")
	res := fs.String("res", "raw", "resolution: raw|1m")
	format := fs.String("format", "csv", "output format: csv|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *metric == "" {
		fmt.Fprintln(stderr, "usage: dump --metric <id> [--since 24h] [--res raw|1m] [--format csv|json]")
		return 2
	}
	sinceDur, err := time.ParseDuration(*since)
	if err != nil {
		fmt.Fprintln(stderr, "invalid --since:", err)
		return 2
	}
	var resolution Resolution
	switch *res {
	case "raw":
		resolution = ResRaw
	case "1m":
		resolution = Res1m
	default:
		fmt.Fprintf(stderr, "invalid --res %q (want raw|1m)\n", *res)
		return 2
	}
	if *format != "csv" && *format != "json" {
		fmt.Fprintf(stderr, "invalid --format %q (want csv|json)\n", *format)
		return 2
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

	now := time.Now().Unix()
	from := now - int64(sinceDur.Seconds())
	if err := dumpSeries(stdout, store, *metric, from, now, resolution, *format); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
