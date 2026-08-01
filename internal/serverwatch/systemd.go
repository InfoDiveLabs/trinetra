package serverwatch

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

const unitPath = "/etc/systemd/system/serverwatch.service"

// secondaryBinPath is a symlink install adds alongside the real
// /usr/local/bin/serverwatch so that `sudo serverwatch ...` resolves on
// distros whose sudo secure_path omits /usr/local/bin (see cmdInstall).
const secondaryBinPath = "/usr/bin/serverwatch"

// renderUnit renders the systemd unit file installed by cmdInstall.
// WatchdogSec=90 pairs with the sdNotify("WATCHDOG=1") ping the sampler
// loop sends every fast tick (default 5s), far inside this 90s window; if
// the sampler loop wedges, no ping is sent and systemd restarts the unit.
// Type=simple still works here: WATCHDOG=1 from the main PID is accepted
// regardless of Type, unlike READY=1 which needs Type=notify.
//
// RuntimeDirectory=serverwatch tells systemd to create /run/serverwatch
// (tmpfs, mode 0755, owned by User=root above) before starting the unit and
// remove it when the unit stops, and to export
// RUNTIME_DIRECTORY=/run/serverwatch into the service's environment. That is
// where serveControlSocket (control_socket.go) puts the control socket the
// daemon's core.API is served over, so the directory always exists with the
// right lifetime instead of the daemon having to create/clean it up itself.
//
// There is no separate "web" unit: cmdInstall (below) always copies
// os.Executable() — whichever binary is currently running — to
// /usr/local/bin/serverwatch and writes this exact same unit around it. So
// installing the `serverwatch-web` binary (`go build -tags web`, `make
// web`/`web-cross`) and then `serverwatch config set web.enabled true` is
// the entire path to a web-capable service; no unit change is needed. See
// docs/handbook/08-web-ui.md for the web.*/public.* config keys and serving modes.
func renderUnit(binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=server-watcher host monitor
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s daemon
Restart=always
RestartSec=5
WatchdogSec=90
User=root
RuntimeDirectory=serverwatch
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, binPath)
}

func cmdInstall(args []string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	dst := "/usr/local/bin/serverwatch"
	if err := copyFile(self, dst, 0o755); err != nil {
		fmt.Fprintf(stderr, "copy binary: %v\n", err)
		return 1
	}
	// Also expose the binary on /usr/bin, which is on sudo's secure_path on
	// every common distro (unlike /usr/local/bin, absent on RHEL/CentOS 7 and
	// some minimal images). Without this, `sudo serverwatch ...` fails with
	// "command not found" there even though the service itself runs fine off
	// the absolute ExecStart. Non-fatal: the binary and unit are already in
	// place, so a link failure only affects the `sudo serverwatch` shortcut.
	if err := linkOnPath(dst, secondaryBinPath); err != nil {
		fmt.Fprintf(stderr, "warning: could not link %s -> %s: %v\n", secondaryBinPath, dst, err)
	}
	if err := os.WriteFile(unitPath, []byte(renderUnit(dst)), 0o644); err != nil {
		fmt.Fprintf(stderr, "write unit: %v\n", err)
		return 1
	}
	// seed config if absent
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if c, _ := loadCfg(); c != nil {
			_ = saveCfg(c)
		}
	}
	x := osExec{}
	for _, a := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", "serverwatch"},
	} {
		if out, err := x.Run(a[0], a[1:]...); err != nil {
			fmt.Fprintf(stderr, "%v: %v\n%s\n", a, err, out)
			return 1
		}
	}
	fmt.Fprintln(stdout, "installed and started. set a token: serverwatch telegram set-token <token>")
	return 0
}

// linkOnPath ensures link is a symlink to target, so `sudo <name>` resolves
// on distros whose secure_path omits target's directory. It never clobbers an
// existing path: if anything already lives at link (a distro-provided real
// binary, or any symlink), it is left untouched. Creating the symlink only
// when link is absent keeps install idempotent and safe. A nil return means
// link now resolves the command (freshly created, or already present).
func linkOnPath(target, link string) error {
	if target == link {
		return nil
	}
	if _, err := os.Lstat(link); err == nil {
		// Something already exists at link; do not clobber it.
		return nil
	}
	return os.Symlink(target, link)
}

// unlinkOnPath removes link only when it is still OUR symlink pointing at
// target, so uninstall never deletes a distro-provided real binary or a
// symlink someone else created.
func unlinkOnPath(target, link string) {
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return
	}
	if dest, _ := os.Readlink(link); dest == target {
		_ = os.Remove(link)
	}
}

func cmdUninstall(args []string) int {
	x := osExec{}
	_, _ = x.Run("systemctl", "disable", "--now", "serverwatch")
	_ = os.Remove(unitPath)
	// Remove the /usr/bin shortcut, but only if it is still OUR symlink into
	// /usr/local/bin (never a distro-provided real binary).
	unlinkOnPath("/usr/local/bin/serverwatch", secondaryBinPath)
	_, _ = x.Run("systemctl", "daemon-reload")
	purge := len(args) > 0 && args[0] == "--purge"
	if purge {
		_ = os.RemoveAll(stateDir)
		_ = os.Remove(cfgPath)
	}
	fmt.Fprintln(stdout, "uninstalled")
	return 0
}

// copyFile copies src to dst atomically: it writes a temp file in dst's
// directory then renames it into place. rename(2) swaps the directory entry
// without truncating the existing file, so this succeeds even when dst is a
// currently-running executable — a plain truncating write (os.WriteFile over
// dst) fails there with ETXTBSY "text file busy". This is what lets
// `serverwatch install` upgrade the binary of a live daemon in place.
func copyFile(src, dst string, perm os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp-install"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	// os.WriteFile honors perm only when creating; force it in case tmp
	// pre-existed with different bits, so the renamed dst is executable.
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func cmdTelegram(args []string) int {
	if len(args) == 2 && args[0] == "set-token" {
		c, err := loadCfg()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		_ = c.Set("telegram.token", args[1])
		if err := saveCfg(c); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		reloadDaemon()
		return 0
	}
	fmt.Fprintln(stderr, "usage: telegram set-token <token>")
	return 2
}

func cmdMonitor(args []string) int {
	c, err := loadCfg()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: monitor list|enable|disable|threshold")
		return 2
	}
	switch args[0] {
	case "list":
		for _, tg := range Discover(osExec{}, osFS{}) {
			state := "on"
			if !c.TargetEnabled(tg.ID) {
				state = "off"
			}
			if !tg.Available {
				state = "unavailable"
			}
			fmt.Fprintf(stdout, "%-28s %s\n", tg.ID, state)
		}
		return 0
	case "enable", "disable":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: monitor enable|disable <target>")
			return 2
		}
		c.SetTarget(args[1], args[0] == "enable")
	case "threshold":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "usage: monitor threshold <target> <value>")
			return 2
		}
		v, err := strconv.ParseFloat(args[2], 64)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		c.SetTargetThreshold(args[1], v)
	default:
		fmt.Fprintln(stderr, "usage: monitor list|enable|disable|threshold")
		return 2
	}
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	reloadDaemon()
	return 0
}

func cmdSchedule(args []string) int {
	c, err := loadCfg()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: schedule daily HH:MM | weekly dow@HH:MM | off")
		return 2
	}
	switch args[0] {
	case "daily":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: schedule daily HH:MM | weekly dow@HH:MM | off")
			return 2
		}
		val := ""
		if args[1] != "off" {
			val = args[1]
		}
		if err := c.Set("schedule.daily", val); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	case "weekly":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: schedule daily HH:MM | weekly dow@HH:MM | off")
			return 2
		}
		val := ""
		if args[1] != "off" {
			val = args[1]
		}
		if err := c.Set("schedule.weekly", val); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	default:
		fmt.Fprintln(stderr, "usage: schedule daily HH:MM | weekly dow@HH:MM | off")
		return 2
	}
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	reloadDaemon()
	return 0
}

func cmdQuietHours(args []string) int {
	c, err := loadCfg()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: quiet-hours <HH-HH> | off")
		return 2
	}
	val := ""
	if args[0] != "off" {
		val = args[0]
	}
	if err := c.Set("quiet_hours", val); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	reloadDaemon()
	return 0
}

func cmdHealthchecks(args []string) int {
	c, err := loadCfg()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(args) == 2 && args[0] == "set" {
		_ = c.Set("healthchecks.url", args[1])
	} else if len(args) == 1 && args[0] == "off" {
		_ = c.Set("healthchecks.url", "")
	} else {
		fmt.Fprintln(stderr, "usage: healthchecks set <url> | off")
		return 2
	}
	if err := saveCfg(c); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	reloadDaemon()
	return 0
}

func cmdStatus(args []string) int {
	b, err := os.ReadFile(stateDir + "/status.json")
	if err != nil {
		fmt.Fprintln(stderr, "no status yet:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}

func cmdDoctor(args []string) int {
	x := osExec{}
	fs := osFS{}

	// Cardinality/disk guardrail visibility (docs/handbook/12-roadmap-and-status.md Epic #69 x7): a
	// corrupt config just falls back to defaults here (same as cmdConfig's
	// set/unset repair path) since doctor is a read-only diagnostic, not
	// worth failing over.
	c, err := loadCfg()
	if err != nil {
		c = config.Default()
	}
	var store SampleStore
	if s, serr := openConfiguredStore(c); serr == nil {
		store = s
		defer store.Close()
	}

	renderDoctorReport(stdout, buildDoctorReport(x, fs, c, store))
	return 0
}

// buildDoctorReport runs the same probes `serverwatch doctor` has always run
// inline (docker reachability via probeDocker, smartctl availability via
// `smartctl --scan`, the thermal-zone glob, target discovery via Discover,
// and the collector on/off toggles plus SampleStore stats via
// collectorSummary's underlying logic) and packages the results into a
// core.DoctorReport, so both cmdDoctor and core.API.Doctor() (coreapi_inproc.go,
// coreapi_file.go) share one probe implementation instead of two copies that
// could drift. store may be nil (the configured backend failed to open, or
// store-writes are disabled), in which case StoreStats reads "unavailable"
// -- the same degrade collectorSummary has always applied.
func buildDoctorReport(x Exec, fs FileSource, c *config.Config, store SampleStore) core.DoctorReport {
	da := probeDocker(x, fs)
	smartOK := false
	if _, err := runMaybeSudo(x, "smartctl", "--scan"); err == nil {
		smartOK = true
	}
	zones, _ := fs.Glob("/sys/class/thermal/thermal_zone*/temp")
	targets := Discover(x, fs)

	rep := core.DoctorReport{
		DockerAccess:      fmt.Sprintf("available=%v method=%s", da.available, da.method),
		SmartctlAvailable: smartOK,
		ThermalZones:      len(zones),
		TargetsDiscovered: len(targets),
		ContainerStatsOn:  c.ContainerStatsEnabled(),
		NetThroughputOn:   c.NetThroughputEnabled(),
		ServicesOn:        c.ServicesEnabled(),
		ProcessesOn:       c.ProcessesEnabled(),
		SmartAttrsOn:      c.SmartAttrsEnabled(),
	}

	if store == nil {
		rep.StoreStats = "unavailable"
		return rep
	}
	n, diskBytes, err := store.Stats()
	if err != nil {
		rep.StoreStats = "unavailable"
		return rep
	}
	rep.StoreStats = fmt.Sprintf("%d series, %.1f MB on disk (raw+1m)", n, float64(diskBytes)/(1024*1024))
	return rep
}

// renderDoctorReport writes rep to w in the exact line-for-line format
// cmdDoctor has always printed -- reconstructed from the DoctorReport DTO
// now that the probe orchestration that fills it in lives in
// buildDoctorReport. Kept as its own function (rather than inlined back into
// cmdDoctor) so a golden test can pin the print format against a
// hand-built core.DoctorReport, independent of whatever a fakeExec/fakeFS
// probe run produces.
func renderDoctorReport(w io.Writer, rep core.DoctorReport) {
	fmt.Fprintf(w, "docker: %s\n", rep.DockerAccess)
	if rep.SmartctlAvailable {
		fmt.Fprintln(w, "smartctl: ok")
	} else {
		fmt.Fprintln(w, "smartctl: unavailable")
	}
	fmt.Fprintf(w, "thermal zones: %d\n", rep.ThermalZones)
	fmt.Fprintf(w, "targets discovered: %d\n", rep.TargetsDiscovered)
	fmt.Fprintf(w, "collectors: container_stats=%s net_throughput=%s services=%s processes=%s smart_attrs=%s\n",
		onOff(rep.ContainerStatsOn), onOff(rep.NetThroughputOn), onOff(rep.ServicesOn),
		onOff(rep.ProcessesOn), onOff(rep.SmartAttrsOn))
	fmt.Fprintf(w, "time-series: %s\n", rep.StoreStats)
}

// onOff renders a bool as "on"/"off" for the doctor collector summary.
func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}
