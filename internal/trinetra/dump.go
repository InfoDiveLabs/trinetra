// Package trinetra: dump.go implements the `trinetra dump` subcommand (s9 in
// docs/handbook/09-storage-and-data-model.md).
package trinetra

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// writeSeries writes pts to w in the requested format ("csv" or "json"). csv writes a
// header row ("ts,min,avg,max") followed by one row per point, ts as a Unix timestamp.
func writeSeries(w io.Writer, pts []Point, format string) error {
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

// seriesPointsToPoints converts core.API's Series result back into this package's own Point
// type, so cmdDump's json output keeps rendering capitalized field names.
func seriesPointsToPoints(pts []core.SeriesPoint) []Point {
	if pts == nil {
		return nil
	}
	out := make([]Point, len(pts))
	for i, p := range pts {
		out[i] = Point{TS: p.TS, Min: p.Min, Avg: p.Avg, Max: p.Max}
	}
	return out
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
	// res is the explicit resolution the --res flag chose; core.API.Series also accepts
	// core.ResAuto (a picker-decides sentinel).
	var resolution core.Resolution
	switch *res {
	case "raw":
		resolution = core.ResRaw
	case "1m":
		resolution = core.Res1m
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

	now := time.Now().Unix()
	from := now - int64(sinceDur.Seconds())
	pts, err := newFileAPI(stateDir, cfg).Series(*metric, from, now, resolution)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeSeries(stdout, seriesPointsToPoints(pts), *format); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
