// Package trinetra: downtime_cmd.go implements the `trinetra downtime`
// CLI, whose only subcommand today is `purge` -- an operator tool to clear
// bogus downtime events from the store, e.g. the short fabricated power_downs a
// daemon crash loop wrote before the #116 classification fix stopped them.
package trinetra

import (
	"flag"
	"fmt"
)

func cmdDowntime(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: trinetra downtime purge [--type power_down] [--max-seconds 300]")
		return 2
	}
	switch args[0] {
	case "purge":
		return cmdDowntimePurge(args[1:])
	default:
		fmt.Fprintf(stderr, "unknown downtime subcommand %q\nusage: trinetra downtime purge [--type power_down] [--max-seconds 300]\n", args[0])
		return 2
	}
}

// cmdDowntimePurge removes downtime events of a given type below a duration threshold.
func cmdDowntimePurge(args []string) int {
	fs := flag.NewFlagSet("downtime purge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	typ := fs.String("type", "power_down", "event type to purge: power_down|net_down")
	maxSec := fs.Int64("max-seconds", 300, "purge only events of --type SHORTER than this many seconds (targets restart-storm artifacts); 0 removes all events of --type")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *maxSec < 0 {
		fmt.Fprintln(stderr, "--max-seconds must be >= 0")
		return 2
	}

	cfg, err := loadCfg()
	if err != nil {
		fmt.Fprintln(stderr, "load config:", err)
		return 1
	}
	store, err := openConfiguredStore(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "open store:", err)
		return 1
	}
	defer store.Close()

	keep := func(e DownEvent) bool {
		if e.Type != *typ {
			return true // different type: never touched
		}
		if *maxSec > 0 && e.DurationSec >= *maxSec {
			return true // long enough to be a real outage: keep it
		}
		return false // matching type, and short (or no bound): purge
	}
	removed, err := store.PurgeEvents(keep)
	if err != nil {
		fmt.Fprintln(stderr, "purge events:", err)
		return 1
	}
	if *maxSec > 0 {
		fmt.Fprintf(stdout, "purged %d %s event(s) shorter than %ds\n", removed, *typ, *maxSec)
	} else {
		fmt.Fprintf(stdout, "purged %d %s event(s)\n", removed, *typ)
	}
	return 0
}
