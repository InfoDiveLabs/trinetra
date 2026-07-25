// Package serverwatch: alerts_cli.go implements `serverwatch alerts ...`:
// listing currently-active alerts plus recent alert-log history, and
// acknowledging/unacknowledging an active alert.
package serverwatch

import (
	"flag"
	"fmt"
	"sort"
	"time"
)

const usageAlerts = `usage:
  serverwatch alerts [list] [--since 24h] [--limit 20]
  serverwatch alerts ack <key>
  serverwatch alerts unack <key>`

func cmdAlerts(args []string) int {
	if len(args) == 0 {
		return cmdAlertsList(nil)
	}
	switch args[0] {
	case "list":
		return cmdAlertsList(args[1:])
	case "ack":
		return cmdAlertsAck(args[1:], true)
	case "unack":
		return cmdAlertsAck(args[1:], false)
	default:
		fmt.Fprintf(stderr, "unknown alerts subcommand %q\n\n%s\n", args[0], usageAlerts)
		return 2
	}
}

func alertStateAndLogPaths() (statePath, logPath string) {
	s := NewStore(stateDir, realClock{})
	return s.AlertStatePath(), s.AlertLogPath()
}

func cmdAlertsAck(args []string, ack bool) int {
	verb := "ack"
	if !ack {
		verb = "unack"
	}
	if len(args) != 1 {
		fmt.Fprintf(stderr, "usage: alerts %s <key>\n", verb)
		return 2
	}
	key := args[0]
	statePath, _ := alertStateAndLogPaths()
	state := LoadAlertState(statePath, osFS{})

	var err error
	if ack {
		err = state.Ack(key, time.Now().Unix())
	} else {
		err = state.Unack(key)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := state.Save(statePath); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if ack {
		fmt.Fprintf(stdout, "%s acknowledged\n", key)
	} else {
		fmt.Fprintf(stdout, "%s unacknowledged\n", key)
	}
	return 0
}

func cmdAlertsList(args []string) int {
	fs := flag.NewFlagSet("alerts list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "24h", "how far back to show alert-log history, e.g. 24h, 7d")
	limit := fs.Int("limit", 20, "max history events to show (most recent first)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sinceDur, err := time.ParseDuration(*since)
	if err != nil {
		fmt.Fprintln(stderr, "invalid --since:", err)
		return 2
	}

	statePath, logPath := alertStateAndLogPaths()
	state := LoadAlertState(statePath, osFS{})
	printActiveAlerts(state)

	events, err := NewAlertLog(logPath).AlertEventsSince(time.Now().Add(-sinceDur).Unix())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	printAlertHistory(events, *limit)
	return 0
}

func printActiveAlerts(state *AlertState) {
	fmt.Fprintln(stdout, "ACTIVE ALERTS")
	if len(state.Active) == 0 {
		fmt.Fprintln(stdout, "  (none)")
		return
	}
	keys := make([]string, 0, len(state.Active))
	for k := range state.Active {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	now := time.Now().Unix()
	for _, k := range keys {
		a := state.Active[k]
		ack := ""
		if a.Acked {
			ack = fmt.Sprintf(" [acked %s ago]", humanDur(now-a.AckedAt))
		}
		fmt.Fprintf(stdout, "  %-20s since %s ago  %s%s\n", k, humanDur(now-a.Since), a.Reason, ack)
	}
}

func printAlertHistory(events []AlertEvent, limit int) {
	fmt.Fprintln(stdout, "HISTORY")
	if len(events) == 0 {
		fmt.Fprintln(stdout, "  (none)")
		return
	}
	// events is in chronological (append) order; show the most recent limit,
	// most-recent first.
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		ts := time.Unix(e.Time, 0).Format("2006-01-02 15:04:05")
		fmt.Fprintf(stdout, "  %s  %-6s %-20s %-8s %s\n", ts, e.Kind, e.Key, e.Severity, e.Title)
		for _, d := range e.Delivered {
			status := "ok"
			if !d.OK {
				status = "FAILED: " + d.Err
			}
			fmt.Fprintf(stdout, "      -> %-12s %s\n", d.Channel, status)
		}
	}
}
