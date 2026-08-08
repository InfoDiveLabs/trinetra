package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"serverwatch/internal/core"
)

// run dispatches a single non-interactive ctl subcommand against api and
// writes its human-readable output to out, returning the process exit code
// (0 success, 2 usage error, 1 for a failed API call). It is deliberately
// decoupled from the socket dialing in main so a test can drive it with a
// fake core.API and no real control socket. These subcommands are a
// proof-of-transport for the control socket, not the final UX (the
// interactive TUI is Task 3).
func run(api core.API, args []string, out io.Writer) int {
	// --json may appear anywhere; strip it and pass the flag to the read
	// verbs (status/doctor/alerts). It is inert for the mutating verbs.
	jsonOut := false
	rest := args[:0:0]
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	args = rest

	if len(args) == 0 {
		printUsage(out)
		return 2
	}
	switch args[0] {
	case "status":
		return runStatus(api, out, jsonOut)
	case "doctor":
		return runDoctor(api, out, jsonOut)
	case "host":
		return runHost(api, out, jsonOut)
	case "logs":
		return runLogs(api, args[1:], out)
	case "alerts":
		return runAlerts(api, out, jsonOut)
	case "config":
		return runConfig(api, args[1:], out)
	case "channels":
		return runChannels(api, args[1:], out)
	case "help", "-h", "--help":
		printUsage(out)
		return 0
	default:
		fmt.Fprintf(out, "unknown subcommand %q\n\n", args[0])
		printUsage(out)
		return 2
	}
}

// emitJSON marshals v as indented JSON to out. Returns 1 on a marshal error
// (never expected for the DTOs, which are all plain JSON-tagged structs).
func emitJSON(out io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(out, "json: %v\n", err)
		return 1
	}
	out.Write(b)
	fmt.Fprintln(out)
	return 0
}

// printUsage writes the list of supported subcommands.
func printUsage(out io.Writer) {
	fmt.Fprint(out, "usage: serverwatch-ctl [--socket PATH] [--token PATH] [--json] <command>\n\n"+
		"run with no command to open the interactive TUI.\n\n"+
		"commands:\n"+
		"  status                  print the current dashboard snapshot\n"+
		"  doctor                  print the daemon's diagnostic report\n"+
		"  host                    print the host hardware/OS inventory\n"+
		"  logs <container> [--tail N]  print a docker container's recent logs\n"+
		"  alerts                  print the currently active alerts\n"+
		"  config get <key>        print one config key's current value\n"+
		"  config set <key> <val>  set one config key (validated, applied live)\n"+
		"  channels test <name>    send a test notification through a channel\n\n"+
		"--json makes status/doctor/alerts emit JSON instead of text.\n")
}

// runStatus renders the DashboardView snapshot as a compact key/value list,
// or as JSON when jsonOut is set.
func runStatus(api core.API, out io.Writer, jsonOut bool) int {
	snap, err := api.Snapshot()
	if err != nil {
		fmt.Fprintf(out, "status: %v\n", err)
		return 1
	}
	if jsonOut {
		return emitJSON(out, snap)
	}
	online := "offline"
	if snap.Online {
		online = "online"
	}
	fmt.Fprintf(out, "as of:      %s\n", formatTime(snap.TS))
	fmt.Fprintf(out, "internet:   %s\n", online)
	fmt.Fprintf(out, "cpu:        %.1f%% (%d cores)\n", snap.CPU, snap.Cores)
	fmt.Fprintf(out, "memory:     %.1f%%\n", snap.MemPct)
	fmt.Fprintf(out, "swap:       %.1f%%\n", snap.SwapPct)
	fmt.Fprintf(out, "load:       %.2f %.2f %.2f\n", snap.Load1, snap.Load5, snap.Load15)
	if snap.TempC != 0 {
		fmt.Fprintf(out, "temp:       %.1f C\n", snap.TempC)
	}
	fmt.Fprintf(out, "processes:  %d total (%d running, %d zombie)\n",
		snap.Processes.Total, snap.Processes.Running, snap.Processes.Zombie)
	fmt.Fprintf(out, "containers: %d/%d running\n", snap.ContainersRunning, snap.ContainersTotal)
	fmt.Fprintf(out, "units:      %d failed / %d total\n", snap.UnitsFailed, snap.UnitsTotal)
	fmt.Fprintf(out, "disks:      %d mounts, %d critical\n", len(snap.Disks), snap.DisksCritical)
	fmt.Fprintf(out, "network:    rx %.0f bps / tx %.0f bps\n", snap.NetRxBps, snap.NetTxBps)
	return 0
}

// runDoctor renders the DoctorReport as a readable diagnostic block, or as
// JSON when jsonOut is set.
func runDoctor(api core.API, out io.Writer, jsonOut bool) int {
	rep, err := api.Doctor()
	if err != nil {
		fmt.Fprintf(out, "doctor: %v\n", err)
		return 1
	}
	if jsonOut {
		return emitJSON(out, rep)
	}
	fmt.Fprintf(out, "docker:      %s\n", rep.DockerAccess)
	fmt.Fprintf(out, "smartctl:    %s\n", yesNo(rep.SmartctlAvailable))
	fmt.Fprintf(out, "thermal:     %d zones\n", rep.ThermalZones)
	fmt.Fprintf(out, "targets:     %d discovered\n", rep.TargetsDiscovered)
	fmt.Fprintf(out, "collectors:\n")
	fmt.Fprintf(out, "  container_stats: %s\n", onOff(rep.ContainerStatsOn))
	fmt.Fprintf(out, "  net_throughput:  %s\n", onOff(rep.NetThroughputOn))
	fmt.Fprintf(out, "  services:        %s\n", onOff(rep.ServicesOn))
	fmt.Fprintf(out, "  processes:       %s\n", onOff(rep.ProcessesOn))
	fmt.Fprintf(out, "  smart_attrs:     %s\n", onOff(rep.SmartAttrsOn))
	fmt.Fprintf(out, "time-series: %s\n", rep.StoreStats)
	return 0
}

// runHost prints the static host hardware/OS inventory (#100), the same view
// the web "Host" panel shows, so a terminal-only operator has parity.
func runHost(api core.API, out io.Writer, jsonOut bool) int {
	h, err := api.HostInfo()
	if err != nil {
		fmt.Fprintf(out, "host: %v\n", err)
		return 1
	}
	if jsonOut {
		return emitJSON(out, h)
	}
	fmt.Fprintf(out, "host:     %s\n", orDash(h.Hostname))
	fmt.Fprintf(out, "os:       %s\n", orDash(h.OS))
	fmt.Fprintf(out, "kernel:   %s\n", orDash(h.Kernel))
	cpu := orDash(h.CPUModel)
	if h.CPUSockets > 1 && h.CPUModel != "" {
		cpu = fmt.Sprintf("%d× %s", h.CPUSockets, h.CPUModel)
	}
	if h.CPUThreads > 0 {
		if h.CPUSockets > 1 {
			cpu += fmt.Sprintf("  (%d sockets / %d cores / %d threads", h.CPUSockets, h.CPUCores, h.CPUThreads)
		} else {
			cpu += fmt.Sprintf("  (%d cores / %d threads", h.CPUCores, h.CPUThreads)
		}
		if h.CPUBaseMHz > 0 {
			cpu += fmt.Sprintf(" @ %.0f MHz", h.CPUBaseMHz)
		}
		cpu += ")"
	}
	fmt.Fprintf(out, "cpu:      %s\n", cpu)
	fmt.Fprintf(out, "memory:   %s\n", hostBytes(h.MemTotalBytes))
	fmt.Fprintf(out, "uptime:   %s\n", hostUptime(h.UptimeSec))
	if h.LocalIP != "" {
		fmt.Fprintf(out, "local ip: %s\n", h.LocalIP)
	}
	if h.PublicIP != "" {
		fmt.Fprintf(out, "public ip: %s\n", h.PublicIP)
	}
	if len(h.Disks) > 0 {
		fmt.Fprintf(out, "disks:\n")
		for _, d := range h.Disks {
			kind := "SSD"
			if d.Rotational {
				kind = "HDD"
			}
			fmt.Fprintf(out, "  %-10s %-20s %-4s %10s  %-6s %s\n",
				orDash(d.Device), orDash(d.Model), kind, hostBytes(d.SizeBytes), orDash(d.FSType), d.Mount)
		}
	}
	return 0
}

// runLogs prints a docker container's recent logs (#115 parity):
// `serverwatch-ctl logs <container> [--tail N]`.
func runLogs(api core.API, args []string, out io.Writer) int {
	name := ""
	tail := 200
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tail", "-n":
			if i+1 >= len(args) {
				fmt.Fprintln(out, "logs: --tail needs a number")
				return 2
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				fmt.Fprintf(out, "logs: invalid --tail %q\n", args[i+1])
				return 2
			}
			tail = n
			i++
		default:
			if name == "" {
				name = args[i]
			}
		}
	}
	if name == "" {
		fmt.Fprintln(out, "usage: serverwatch-ctl logs <container> [--tail N]")
		return 2
	}
	logs, err := api.ContainerLogs(name, tail)
	if err != nil {
		fmt.Fprintf(out, "logs: %v\n", err)
		return 1
	}
	fmt.Fprint(out, logs)
	if logs != "" && !strings.HasSuffix(logs, "\n") {
		fmt.Fprintln(out)
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// hostBytes renders a byte count in binary units (KiB/MiB/GiB/TiB).
func hostBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// hostUptime renders a duration in seconds as "Nd Nh Nm" (or "Ns" under a
// minute), dropping leading zero units.
func hostUptime(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	d := sec / 86400
	hh := (sec % 86400) / 3600
	mm := (sec % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, hh, mm)
	case hh > 0:
		return fmt.Sprintf("%dh %dm", hh, mm)
	case mm > 0:
		return fmt.Sprintf("%dm", mm)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// runAlerts lists the currently active alerts, or a note when there are none;
// with jsonOut it emits the alert records as a JSON array (always an array,
// [] when none, so consumers need not special-case the empty state).
func runAlerts(api core.API, out io.Writer, jsonOut bool) int {
	alerts, err := api.ActiveAlerts()
	if err != nil {
		fmt.Fprintf(out, "alerts: %v\n", err)
		return 1
	}
	if jsonOut {
		if alerts == nil {
			alerts = []core.AlertRecord{}
		}
		return emitJSON(out, alerts)
	}
	if len(alerts) == 0 {
		fmt.Fprintln(out, "no active alerts")
		return 0
	}
	for _, a := range alerts {
		ack := ""
		if a.Acked {
			ack = " (acked)"
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s%s\n",
			a.Severity, a.Key, a.Source, formatTime(a.Time), ack)
	}
	return 0
}

// runConfig implements the `config get`/`config set` scriptable verbs, the
// non-interactive counterpart to the TUI's "all settings" screen. Both go
// through the same config.Get/config.Set the TUI uses, so a value the CLI
// rejects is exactly one the TUI would reject too; `set` commits with
// ApplyConfig so the change is live (subject to the same restart-required
// caveats for web.enabled/storage.* the handbook documents).
func runConfig(api core.API, args []string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(out, "usage: serverwatch-ctl config get <key> | set <key> <value>\n")
		return 2
	}
	switch args[0] {
	case "get":
		if len(args) != 2 {
			fmt.Fprint(out, "usage: serverwatch-ctl config get <key>\n")
			return 2
		}
		cfg, err := api.Config()
		if err != nil {
			fmt.Fprintf(out, "config: %v\n", err)
			return 1
		}
		val, ok := cfg.Get(args[1])
		if !ok {
			fmt.Fprintf(out, "unknown config key %q (see `serverwatch-ctl` -> manage -> all settings for the catalog)\n", args[1])
			return 2
		}
		fmt.Fprintln(out, val)
		return 0
	case "set":
		if len(args) != 3 {
			fmt.Fprint(out, "usage: serverwatch-ctl config set <key> <value>\n")
			return 2
		}
		cfg, err := api.Config()
		if err != nil {
			fmt.Fprintf(out, "config: %v\n", err)
			return 1
		}
		if err := cfg.Set(args[1], args[2]); err != nil {
			fmt.Fprintf(out, "set %s: %v\n", args[1], err)
			return 1
		}
		if err := api.ApplyConfig(cfg); err != nil {
			fmt.Fprintf(out, "apply: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "set %s = %s\n", args[1], args[2])
		return 0
	default:
		fmt.Fprintf(out, "unknown config subcommand %q (want get or set)\n", args[0])
		return 2
	}
}

// runChannels implements the `channels test <name>` scriptable verb: it asks
// the daemon to send a live test notification through the named channel, the
// same TestChannel the TUI's Channels screen 't' action uses.
func runChannels(api core.API, args []string, out io.Writer) int {
	if len(args) == 0 || args[0] != "test" {
		fmt.Fprint(out, "usage: serverwatch-ctl channels test <name>\n")
		return 2
	}
	if len(args) != 2 {
		fmt.Fprint(out, "usage: serverwatch-ctl channels test <name>\n")
		return 2
	}
	if err := api.TestChannel(args[1]); err != nil {
		fmt.Fprintf(out, "test %s: %v\n", args[1], err)
		return 1
	}
	fmt.Fprintf(out, "test notification sent via %q\n", args[1])
	return 0
}

// formatTime renders a Unix-seconds timestamp in local time, or a dash when
// it is zero (no timestamp).
func formatTime(ts int64) string {
	if ts == 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
