package trinetra

import (
	"fmt"
	"html"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// maxFailureDetailItems bounds every "only failures" detail list rendered by renderStatus
// (failing disks, down containers, failed units, FAILED SMART devices).
const maxFailureDetailItems = 10

// maxDiskTableRows bounds the /disk command's full mount table.
const maxDiskTableRows = 15

// severity classifies val against a graduated warn/crit pair: crit if val >= critAt, warn
// if val >= warnAt (a softer threshold below critAt).
func severity(val, warnAt, critAt float64) (warn, crit bool) {
	if critAt <= 0 {
		return false, false
	}
	if val >= critAt {
		return true, true
	}
	if warnAt > 0 && val >= warnAt {
		return true, false
	}
	return false, false
}

// pctSeverity is severity for the common case of a single crit threshold (as
// config.Thresholds stores): warn kicks in at 90% of crit.
func pctSeverity(val, critAt float64) (warn, crit bool) {
	return severity(val, critAt*0.9, critAt)
}

func markStr(warn, crit bool) string {
	switch {
	case crit:
		return "❌"
	case warn:
		return "⚠️"
	default:
		return "✅"
	}
}

// capList bounds items to max entries, appending a "+N more" summary of the rest instead of
// silently truncating.
func capList(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	out := make([]string, 0, max+1)
	out = append(out, items[:max]...)
	out = append(out, fmt.Sprintf("+%d more", len(items)-max))
	return out
}

// diskFailure is one non-ok mount for renderStatus's only-failures detail
// list: mount name, usage%, and whether it's crit (vs merely warn).
type diskFailure struct {
	mount string
	pct   float64
	crit  bool
}

// diskSeverityCounts classifies every mount in s.Disks against its configured threshold (a
// per-target override via c.TargetThreshold, or the global c.Thresholds.DiskPct).
func diskSeverityCounts(s Snapshot, c *config.Config) (ok, warn, crit int, failures []diskFailure) {
	mounts := make([]string, 0, len(s.Disks))
	for m := range s.Disks {
		mounts = append(mounts, m)
	}
	sort.Strings(mounts) // stable iteration order before the by-severity sort below
	for _, m := range mounts {
		pct := s.Disks[m]
		threshold := c.Thresholds.DiskPct
		if o, hasO := c.TargetThreshold("disk:" + m); hasO {
			threshold = o
		}
		w, cr := pctSeverity(pct, threshold)
		switch {
		case cr:
			crit++
			failures = append(failures, diskFailure{m, pct, true})
		case w:
			warn++
			failures = append(failures, diskFailure{m, pct, false})
		default:
			ok++
		}
	}
	sort.SliceStable(failures, func(i, j int) bool { return failures[i].pct > failures[j].pct })
	return
}

// dockerSummary counts running vs total containers and lists (name-sorted) the non-running
// ones, for renderStatus's docker summary + only-failures detail.
func dockerSummary(cs map[string]string) (running, total int, down []string) {
	names := make([]string, 0, len(cs))
	for n := range cs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		total++
		if cs[n] == "running" {
			running++
		} else {
			down = append(down, n)
		}
	}
	return
}

// smartSummary counts PASSED vs FAILED SMART devices and lists (device- sorted) the FAILED
// ones, for renderStatus's smart summary + only-failures detail.
func smartSummary(h map[string]string) (ok int, failed []string) {
	devs := make([]string, 0, len(h))
	for d := range h {
		devs = append(devs, d)
	}
	sort.Strings(devs)
	for _, d := range devs {
		if h[d] == "FAILED" {
			failed = append(failed, d)
		} else {
			ok++
		}
	}
	return
}

// humanBytes formats a byte count as a short human-readable size (e.g.
// "1.5GiB"), for renderDisks' free-space column.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// DiskDetail is the live per-mount filesystem detail beyond the usage percentage in
// Snapshot.Disks: device path, filesystem type, inode usage, free/total bytes.
type DiskDetail struct {
	Device, FsType       string
	UsagePct, InodePct   float64
	FreeBytes, SizeBytes uint64
	// DaysToFull/DaysToFullKnown are a linear-projection estimate of how many days remain
	// until the mount reaches 100% used.
	DaysToFull      float64
	DaysToFullKnown bool
}

// SmartAttr is a device's parsed `smartctl -A` attributes: temperature, wear/life-remaining
// percentage, and reallocated-sector count.
type SmartAttr struct {
	TempC          int
	WearPct        int
	ReallocSectors int
}

type Snapshot struct {
	TS      int64              `json:"ts"`
	CPU     float64            `json:"cpu"`
	MemPct  float64            `json:"mem_pct"`
	SwapPct float64            `json:"swap_pct"`
	Load1   float64            `json:"load1"`
	Load5   float64            `json:"load5"`
	Load15  float64            `json:"load15"`
	TempC   float64            `json:"temp_c"`
	Disks   map[string]float64 `json:"disks"`
	Online  bool               `json:"online"`
	// SlowStale is set when the slow-collector goroutine missed its deadline and the slow-tier
	// fields on this snapshot are the last-good values, not freshly collected this cycle.
	SlowStale    bool              `json:"slow_stale,omitempty"`
	DockerAccess string            `json:"docker_access"`
	Containers   map[string]string `json:"containers,omitempty"`   // name -> state (e.g. "running","exited")
	FailedUnits  []string          `json:"failed_units,omitempty"` // systemctl --failed unit names
	SmartHealth  map[string]string `json:"smart_health,omitempty"` // device -> "PASSED"|"FAILED"|"UNKNOWN"
	// DiskDetail is the live per-mount device/fstype/inode%/size detail (see the DiskDetail
	// type doc comment above), keyed by mount.
	DiskDetail map[string]DiskDetail `json:"disk_detail,omitempty"`
	// SmartAttrs is the live per-device SMART attribute detail (temperature, wear%,
	// reallocated sectors; see the SmartAttr type doc comment above), keyed by device path.
	SmartAttrs map[string]SmartAttr `json:"smart_attrs,omitempty"`
	// ContainerStats is the live per-container cpu%/mem/net snapshot from `docker stats
	// --no-stream` (opt-in via collect.container_stats, slow-tier only).
	ContainerStats map[string]ContainerStat `json:"container_stats,omitempty"`
	// NetRates is the live per-interface network throughput (bytes/sec), computed by
	// NetRateCalc from consecutive /proc/net/dev samples.
	NetRates map[string]IfaceRate `json:"net_rates,omitempty"`
	// Units is the live, full systemd service-unit inventory (opt-in via collect.services,
	// slow-tier only, see daemon.go collectSlow and discover.go listUnits/parseUnits).
	Units []UnitInfo `json:"units,omitempty"`
	// Processes is the live process-table overview (counts + top-N by CPU/mem), computed by
	// collectProcesses.
	Processes ProcSnapshot `json:"processes,omitempty"`
	// CollectorErrors is the set of slow-tier collectors that were ATTEMPTED this cycle but
	// failed (key -> error text), e.g. "docker"/"disk"/ "services"/"smart" (#110).
	CollectorErrors map[string]string `json:"collector_errors,omitempty"`
	// CollectorHealth is the rolling per-collector health (#110): consecutive failure count,
	// last-success time, and last error.
	CollectorHealth map[string]CollectorStat `json:"collector_health,omitempty"`
	// collectorsAttempted is the set of slow-tier collectors collectSlow actually ran this
	// cycle (#110).
	collectorsAttempted map[string]bool
}

// renderStatus builds the /stats,/status overview: a header giving the overall status, a
// compact resource table (CPU/Mem/Swap/Load/Temp with an ok/warn/crit marker).
func renderStatus(s Snapshot, c *config.Config) string {
	if c == nil {
		c = config.Default()
	}
	var warnN, critN int

	type row struct{ label, value, mark string }
	var rows []row
	addRow := func(label, value string, warn, crit bool) {
		if crit {
			critN++
		} else if warn {
			warnN++
		}
		rows = append(rows, row{label, value, markStr(warn, crit)})
	}

	{
		w, cr := pctSeverity(s.CPU, c.Thresholds.CPUPct)
		addRow("CPU", fmt.Sprintf("%.0f%%", s.CPU), w, cr)
	}
	{
		w, cr := pctSeverity(s.MemPct, c.Thresholds.MemPct)
		addRow("Mem", fmt.Sprintf("%.0f%%", s.MemPct), w, cr)
	}
	{
		w, cr := pctSeverity(s.SwapPct, c.Thresholds.SwapPct)
		addRow("Swap", fmt.Sprintf("%.0f%%", s.SwapPct), w, cr)
	}
	{
		// Load has no configured threshold anywhere else in this codebase (unlike
		// cpu/mem/swap/temp/disk, which all have a config.Thresholds field).
		nc := float64(runtime.NumCPU())
		if nc < 1 {
			nc = 1
		}
		w, cr := severity(s.Load1, nc, nc*2)
		addRow("Load", fmt.Sprintf("%.2f", s.Load1), w, cr)
	}
	if s.TempC > 0 {
		w, cr := pctSeverity(s.TempC, c.Thresholds.TempC)
		addRow("Temp", fmt.Sprintf("%.0f°C", s.TempC), w, cr)
	}

	diskOK, diskWarn, diskCrit, diskFailures := diskSeverityCounts(s, c)
	warnN += diskWarn
	critN += diskCrit

	dockerRunning, dockerTotal, dockerDown := dockerSummary(s.Containers)
	critN += len(dockerDown)

	servicesFailed := len(s.FailedUnits)
	critN += servicesFailed

	smartOK, smartFailed := smartSummary(s.SmartHealth)
	critN += len(smartFailed)

	if !s.Online {
		critN++
	}

	var b strings.Builder
	switch {
	case critN > 0:
		fmt.Fprintf(&b, "❌ %d critical\n\n", critN)
	case warnN > 0:
		fmt.Fprintf(&b, "⚠️ %d warning%s\n\n", warnN, plural(warnN))
	default:
		b.WriteString("✅ all clear\n\n")
	}

	b.WriteString("<pre>\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.label, r.value, r.mark)
	}
	tw.Flush()
	b.WriteString("</pre>\n")

	fmt.Fprintf(&b, "Disks: %d ok / %d warn / %d CRIT (of %d)\n", diskOK, diskWarn, diskCrit, diskOK+diskWarn+diskCrit)
	if dockerTotal > 0 {
		fmt.Fprintf(&b, "Docker: %d/%d running (%d down)\n", dockerRunning, dockerTotal, len(dockerDown))
	} else {
		b.WriteString("Docker: no containers (or unavailable)\n")
	}
	fmt.Fprintf(&b, "systemd: %d failed\n", servicesFailed)
	if len(s.SmartHealth) > 0 {
		fmt.Fprintf(&b, "SMART: %d ok / %d FAILED\n", smartOK, len(smartFailed))
	}
	fmt.Fprintf(&b, "Internet: %s\n", onlineStr(s.Online))

	hasFailures := len(diskFailures) > 0 || len(dockerDown) > 0 || servicesFailed > 0 || len(smartFailed) > 0 || !s.Online
	if !hasFailures {
		b.WriteString("\n✅ all systems normal")
		return b.String()
	}

	b.WriteString("\n")
	if len(diskFailures) > 0 {
		b.WriteString("<b>Disks:</b>\n")
		items := make([]string, len(diskFailures))
		for i, f := range diskFailures {
			mark := "⚠️"
			if f.crit {
				mark = "❌"
			}
			items[i] = fmt.Sprintf("%s %s %.0f%%", mark, html.EscapeString(f.mount), f.pct)
		}
		for _, it := range capList(items, maxFailureDetailItems) {
			fmt.Fprintf(&b, "  %s\n", it)
		}
	}
	if len(dockerDown) > 0 {
		b.WriteString("<b>Docker (down):</b>\n")
		items := make([]string, len(dockerDown))
		for i, n := range dockerDown {
			items[i] = fmt.Sprintf("❌ %s (%s)", html.EscapeString(n), html.EscapeString(s.Containers[n]))
		}
		for _, it := range capList(items, maxFailureDetailItems) {
			fmt.Fprintf(&b, "  %s\n", it)
		}
	}
	if servicesFailed > 0 {
		b.WriteString("<b>Failed units:</b>\n")
		items := make([]string, len(s.FailedUnits))
		for i, u := range s.FailedUnits {
			items[i] = "❌ " + html.EscapeString(u)
		}
		for _, it := range capList(items, maxFailureDetailItems) {
			fmt.Fprintf(&b, "  %s\n", it)
		}
	}
	if len(smartFailed) > 0 {
		b.WriteString("<b>SMART FAILED:</b>\n")
		items := make([]string, len(smartFailed))
		for i, d := range smartFailed {
			items[i] = "❌ " + html.EscapeString(d)
		}
		for _, it := range capList(items, maxFailureDetailItems) {
			fmt.Fprintf(&b, "  %s\n", it)
		}
	}
	if !s.Online {
		fmt.Fprintf(&b, "<b>Internet:</b> %s\n", onlineStr(s.Online))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderDisks builds the /disk command's compact table: a summary count line, then a <pre>
// table of mount/use%/free sorted by use% descending.
func renderDisks(disks map[string]float64, detail map[string]DiskDetail) string {
	if len(disks) == 0 {
		return "no filesystems discovered"
	}
	type row struct {
		mount string
		pct   float64
		free  string
	}
	rows := make([]row, 0, len(disks))
	mounts := make([]string, 0, len(disks))
	for m := range disks {
		mounts = append(mounts, m)
	}
	sort.Strings(mounts) // stable order before the by-usage sort below
	for _, m := range mounts {
		free := "?"
		if d, ok := detail[m]; ok {
			free = humanBytes(d.FreeBytes)
		}
		rows = append(rows, row{m, disks[m], free})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].pct > rows[j].pct })

	var b strings.Builder
	fmt.Fprintf(&b, "Disks (%d filesystem%s)\n<pre>\n", len(rows), plural(len(rows)))
	shown := rows
	var extra int
	if len(rows) > maxDiskTableRows {
		extra = len(rows) - maxDiskTableRows
		shown = rows[:maxDiskTableRows]
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, r := range shown {
		fmt.Fprintf(tw, "%s\t%.0f%%\t%s\n", html.EscapeString(r.mount), r.pct, r.free)
	}
	tw.Flush()
	b.WriteString("</pre>")
	if extra > 0 {
		fmt.Fprintf(&b, "\n+%d more", extra)
	}
	return b.String()
}

func renderDocker(cs map[string]string) string {
	if len(cs) == 0 {
		return "docker: no containers (or unavailable)"
	}
	names := make([]string, 0, len(cs))
	for n := range cs {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("containers:")
	for _, n := range names {
		mark := "✅"
		if cs[n] != "running" {
			mark = "❌"
		}
		fmt.Fprintf(&b, "\n  %s %s (%s)", mark, html.EscapeString(n), html.EscapeString(cs[n]))
	}
	return b.String()
}

func renderServices(units []string) string {
	if len(units) == 0 {
		return "systemd: no failed units ✅"
	}
	var b strings.Builder
	b.WriteString("failed units:")
	for _, u := range units {
		fmt.Fprintf(&b, "\n  ❌ %s", html.EscapeString(u))
	}
	return b.String()
}

func onlineStr(up bool) string {
	if up {
		return "up ✅"
	}
	return "DOWN ❌"
}
