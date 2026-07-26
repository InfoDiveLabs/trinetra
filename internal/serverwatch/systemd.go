package serverwatch

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"serverwatch/internal/config"
)

const unitPath = "/etc/systemd/system/serverwatch.service"

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
User=root
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

func cmdUninstall(args []string) int {
	x := osExec{}
	_, _ = x.Run("systemctl", "disable", "--now", "serverwatch")
	_ = os.Remove(unitPath)
	_, _ = x.Run("systemctl", "daemon-reload")
	purge := len(args) > 0 && args[0] == "--purge"
	if purge {
		_ = os.RemoveAll(stateDir)
		_ = os.Remove(cfgPath)
	}
	fmt.Fprintln(stdout, "uninstalled")
	return 0
}

func copyFile(src, dst string, perm os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, perm)
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
	da := probeDocker(x, fs)
	fmt.Fprintf(stdout, "docker: available=%v method=%s\n", da.available, da.method)
	if _, err := runMaybeSudo(x, "smartctl", "--scan"); err == nil {
		fmt.Fprintln(stdout, "smartctl: ok")
	} else {
		fmt.Fprintln(stdout, "smartctl: unavailable")
	}
	zones, _ := fs.Glob("/sys/class/thermal/thermal_zone*/temp")
	fmt.Fprintf(stdout, "thermal zones: %d\n", len(zones))
	fmt.Fprintf(stdout, "targets discovered: %d\n", len(Discover(x, fs)))

	// Cardinality/disk guardrail visibility (docs/ROADMAP.md Epic #69 x7): a
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
	fmt.Fprint(stdout, collectorSummary(c, store))
	return 0
}

// onOff renders a bool as "on"/"off" for the doctor collector summary.
func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

// collectorSummary renders the `serverwatch doctor` cardinality/disk
// guardrail output (docs/ROADMAP.md Epic #69 x7): the on/off state of every
// opt-in extended collector, plus the configured SampleStore's series count
// and on-disk footprint. store may be nil (the configured backend failed to
// open), in which case the series/disk line reads "unavailable" instead of
// panicking or erroring.
func collectorSummary(c *config.Config, store SampleStore) string {
	var b strings.Builder
	fmt.Fprintf(&b, "collectors: container_stats=%s net_throughput=%s services=%s processes=%s smart_attrs=%s\n",
		onOff(c.ContainerStatsEnabled()), onOff(c.NetThroughputEnabled()), onOff(c.ServicesEnabled()),
		onOff(c.ProcessesEnabled()), onOff(c.SmartAttrsEnabled()))
	if store == nil {
		b.WriteString("time-series: unavailable\n")
		return b.String()
	}
	n, diskBytes, err := store.Stats()
	if err != nil {
		b.WriteString("time-series: unavailable\n")
		return b.String()
	}
	fmt.Fprintf(&b, "time-series: %d series, %.1f MB on disk (raw+1m)\n", n, float64(diskBytes)/(1024*1024))
	return b.String()
}
