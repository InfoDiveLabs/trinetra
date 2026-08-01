package main

import (
	"fmt"
	"io"
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
	if len(args) == 0 {
		printUsage(out)
		return 2
	}
	switch args[0] {
	case "status":
		return runStatus(api, out)
	case "doctor":
		return runDoctor(api, out)
	case "alerts":
		return runAlerts(api, out)
	case "help", "-h", "--help":
		printUsage(out)
		return 0
	default:
		fmt.Fprintf(out, "unknown subcommand %q\n\n", args[0])
		printUsage(out)
		return 2
	}
}

// printUsage writes the list of supported subcommands.
func printUsage(out io.Writer) {
	fmt.Fprint(out, "usage: serverwatch-ctl [--socket PATH] [--token PATH] <command>\n\n"+
		"commands:\n"+
		"  status   print the current dashboard snapshot\n"+
		"  doctor   print the daemon's diagnostic report\n"+
		"  alerts   print the currently active alerts\n")
}

// runStatus renders the DashboardView snapshot as a compact key/value list.
func runStatus(api core.API, out io.Writer) int {
	snap, err := api.Snapshot()
	if err != nil {
		fmt.Fprintf(out, "status: %v\n", err)
		return 1
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

// runDoctor renders the DoctorReport as a readable diagnostic block.
func runDoctor(api core.API, out io.Writer) int {
	rep, err := api.Doctor()
	if err != nil {
		fmt.Fprintf(out, "doctor: %v\n", err)
		return 1
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

// runAlerts lists the currently active alerts, or a note when there are none.
func runAlerts(api core.API, out io.Writer) int {
	alerts, err := api.ActiveAlerts()
	if err != nil {
		fmt.Fprintf(out, "alerts: %v\n", err)
		return 1
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
