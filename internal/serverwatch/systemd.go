package serverwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/control"
	"serverwatch/internal/core"
)

const unitPath = "/etc/systemd/system/serverwatch.service"

// secondaryBinPath is a symlink install adds alongside the real
// /usr/local/bin/serverwatch so that `sudo serverwatch ...` resolves on
// distros whose sudo secure_path omits /usr/local/bin (see cmdInstall).
const secondaryBinPath = "/usr/bin/serverwatch"

// renderUnit renders the systemd unit file installed by cmdInstall.
// WatchdogSec=90 pairs with the sdNotify("WATCHDOG=1") ping now sent by a
// dedicated, liveness-gated watchdog goroutine (runWatchdog, watchdog.go) --
// NOT inline on the sampler loop anymore. That goroutine pings at a third of
// this window only while both the sampler loop and the slow collector have
// made recent progress (livenessGate), so a merely-slow slow collection can
// no longer starve the ping and trip a spurious restart, while a genuinely
// wedged loop or a permanently stuck collector still stops the pings and lets
// systemd restart the unit.
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
// os.Executable(), whichever binary is currently running, to
// /usr/local/bin/serverwatch and writes this exact same unit around it. So
// installing the `serverwatch-web` binary (`go build -o
// /usr/local/bin/serverwatch-web ./cmd/serverwatch-web`, no build tag) and
// then `serverwatch config set web.enabled true` is the rest of the path to
// a web-capable service: the daemon's own supervisor verifies and spawns
// serverwatch-web as a child once web.enabled is set and the daemon
// restarts, so still no unit change is needed. See
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
	// Copy any companion plugin binaries (serverwatch-ctl, serverwatch-web)
	// sitting next to the SOURCE binary (self) into the same directory dst
	// was just installed into, so the manifest scan below actually finds
	// something. Per-plugin non-fatal, like everything else here: a copy
	// hiccup on one plugin should not block installing the daemon itself,
	// and a plugin that simply is not present next to self is not an error
	// (see writePluginManifest's identical "absence is not failure" note).
	if err := copyPluginsAlongside(filepath.Dir(self), filepath.Dir(dst)); err != nil {
		fmt.Fprintf(stderr, "warning: could not copy plugin binaries: %v\n", err)
	}
	// Record the checksum manifest the safe plugin launcher (plugin_launch.go)
	// verifies companion binaries against before exec'ing them. Scanning the
	// SAME directory dst was just copied into (rather than, say, os.Executable
	// of this process) matters: dst is exactly where pluginPath will look for
	// serverwatch-ctl/serverwatch-web once this binary is running as
	// /usr/local/bin/serverwatch. Non-fatal: a manifest hiccup should not
	// block installing the daemon itself, since the front-door already fails
	// closed (refuses to exec) when the manifest is missing or incomplete.
	if err := writePluginManifest(filepath.Dir(dst)); err != nil {
		fmt.Fprintf(stderr, "warning: could not write plugin manifest: %v\n", err)
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
	// enable + restart (not `enable --now`): `enable --now` only STARTS a
	// stopped service, so on an upgrade of an already-running serverwatch the
	// new binary and unit would sit on disk while the old daemon kept running
	// until a manual restart. `restart` starts a stopped unit and reloads a
	// running one, so a fresh install and an in-place upgrade both end on the
	// just-installed binary with the freshly-written unit.
	for _, a := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "serverwatch"},
		{"systemctl", "restart", "serverwatch"},
	} {
		if out, err := x.Run(a[0], a[1:]...); err != nil {
			fmt.Fprintf(stderr, "%v: %v\n%s\n", a, err, out)
			return 1
		}
	}
	fmt.Fprintln(stdout, installedPluginsMessage()+
		" installed and started. set a token: serverwatch telegram set-token <token>")
	return 0
}

// installedPluginsMessage reports which companion plugins ended up recorded
// in the plugin manifest after copyPluginsAlongside/writePluginManifest ran,
// so the install success line tells the operator whether the plugins they
// expect actually made it in (or that none were found next to the source
// binary and copied). Reads the manifest rather than re-deriving the list
// from copyPluginsAlongside's own return value so it reflects exactly what
// verifyPlugin will later check against.
func installedPluginsMessage() string {
	manifest, err := loadPluginManifest()
	if err != nil || len(manifest) == 0 {
		return "no plugin binaries found alongside the source binary (serverwatch-ctl/serverwatch-web skipped);"
	}
	var names []string
	for _, name := range pluginManifestNames {
		if _, ok := manifest[name]; ok {
			names = append(names, "serverwatch-"+name)
		}
	}
	if len(names) == 0 {
		return "no plugin binaries found alongside the source binary (serverwatch-ctl/serverwatch-web skipped);"
	}
	return "installed plugins: " + strings.Join(names, ", ") + ";"
}

// copyPluginsAlongside copies each companion plugin binary
// ("serverwatch-<name>" for every name in pluginManifestNames) found in
// srcDir into dstDir at mode 0755, using the same atomic copyFile as the
// daemon binary itself. srcDir is the directory of the SOURCE binary
// (filepath.Dir(self) in cmdInstall) -- the natural place an operator drops
// the plugin binaries alongside the daemon binary before running install --
// not dstDir, which is /usr/local/bin, the install destination.
//
// A plugin that is absent from srcDir is skipped, not an error: most
// installs only ship the daemon, or only some of the plugins. A path that
// exists but is not a regular file (a symlink or directory left behind by
// something else) is likewise skipped, mirroring writePluginManifest's
// IsRegular guard immediately below -- only real plugin binaries get
// installed and only real plugin binaries get hashed into the manifest.
// A copy failure on one plugin (permissions, disk full, ...) is reported
// back as a single joined error for the caller to log as a warning; it does
// not stop the loop from attempting the remaining plugins, so one bad
// companion never blocks another good one from installing.
func copyPluginsAlongside(srcDir, dstDir string) error {
	var errs []string
	for _, name := range pluginManifestNames {
		binName := "serverwatch-" + name
		src := filepath.Join(srcDir, binName)
		info, err := os.Lstat(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Sprintf("stat %s: %v", src, err))
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		dst := filepath.Join(dstDir, binName)
		if err := copyFile(src, dst, 0o755); err != nil {
			errs = append(errs, fmt.Sprintf("copy %s: %v", src, err))
			continue
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
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

// pluginManifestNames lists the companion binary name suffixes (matching the
// "serverwatch-<name>" convention pluginPath/verifyPlugin use in
// plugin_launch.go) that writePluginManifest looks for next to the daemon
// binary. Keep this in sync with the launcher's `cli`/`web` front-doors.
var pluginManifestNames = []string{"ctl", "web"}

// writePluginManifest scans binDir (the directory the daemon binary was just
// installed into) for companion plugin binaries and writes
// <stateDir>/plugins.json (mode 0600, so only root -- or whichever uid runs
// install -- can read or write it: it is the trust anchor loadPluginManifest
// and verifyPlugin check plugin checksums against) mapping each plugin name
// found to the hex SHA-256 of its file content.
//
// A companion binary that is absent at install time is simply omitted from
// the manifest, not an error: loadPluginManifest/verifyPlugin then correctly
// report that plugin as "not installed" (see errPluginNotInstalled) rather
// than treating its absence as a verification failure. A companion that IS
// present but somehow not recorded here still fails closed as intended,
// which is the whole point of the manifest.
func writePluginManifest(binDir string) error {
	manifest := make(map[string]string)
	for _, name := range pluginManifestNames {
		path := filepath.Join(binDir, "serverwatch-"+name)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat %s: %w", path, err)
		}
		// Skip anything that is not a plain regular file (e.g. a symlink or
		// directory left behind by something else): only hash the exact
		// bytes verifyPlugin will later Lstat and hash itself.
		if !info.Mode().IsRegular() {
			continue
		}
		sum, err := sha256File(path)
		if err != nil {
			return fmt.Errorf("checksum %s: %w", path, err)
		}
		manifest[name] = sum
	}

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal plugin manifest: %w", err)
	}
	if err := os.WriteFile(pluginManifestPath(), b, 0o600); err != nil {
		return fmt.Errorf("write plugin manifest %s: %w", pluginManifestPath(), err)
	}
	// os.WriteFile only applies the mode argument when CREATING the file; if
	// plugins.json already existed (a re-install / upgrade) it is truncated
	// and rewritten WITHOUT its permissions being touched, so a pre-existing
	// manifest with looser perms would silently keep them. Force 0600 here,
	// the same way copyFile (above) force-chmods dst after os.WriteFile for
	// the identical reason, so the "root-only trust anchor" guarantee holds
	// on every install, not just the first one.
	if err := os.Chmod(pluginManifestPath(), 0o600); err != nil {
		return fmt.Errorf("chmod plugin manifest %s: %w", pluginManifestPath(), err)
	}
	return nil
}

// removeInstalledPlugins removes each companion plugin binary
// ("serverwatch-<name>" for every name in pluginManifestNames) from binDir.
// Best-effort and symmetric with the other cmdUninstall cleanups: it never
// returns an error, so a plugin that is already absent (never installed, or
// removed by hand) is simply a no-op for that name, not a failure that could
// abort the rest of uninstall.
func removeInstalledPlugins(binDir string) {
	for _, name := range pluginManifestNames {
		_ = os.Remove(filepath.Join(binDir, "serverwatch-"+name))
	}
}

func cmdUninstall(args []string) int {
	x := osExec{}
	_, _ = x.Run("systemctl", "disable", "--now", "serverwatch")
	_ = os.Remove(unitPath)
	// Remove the /usr/bin shortcut, but only if it is still OUR symlink into
	// /usr/local/bin (never a distro-provided real binary).
	unlinkOnPath("/usr/local/bin/serverwatch", secondaryBinPath)
	// Best-effort, like the other uninstall cleanups above: a plugin the
	// front-door can no longer verify against is safer than a stale manifest
	// left lying around after uninstall. --purge below already removes the
	// whole stateDir, so this matters mainly for a non-purge uninstall.
	_ = os.Remove(pluginManifestPath())
	// Symmetric with cmdInstall's copyPluginsAlongside: remove the plugin
	// binaries install placed next to the daemon, so an uninstall does not
	// leave orphaned serverwatch-ctl/serverwatch-web binaries -- now
	// unverifiable anyway since the manifest above was just removed -- sitting
	// in /usr/local/bin.
	removeInstalledPlugins("/usr/local/bin")
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
// currently-running executable -- a plain truncating write (os.WriteFile over
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
		printEnrollmentPIN()
		return 0
	}
	fmt.Fprintln(stderr, "usage: telegram set-token <token>")
	return 2
}

// enrollPINFetchAttempts/enrollPINFetchDelay bound how long
// fetchEnrollmentPIN retries dialing the control socket after set-token's
// reloadDaemon() SIGHUP: about 1.5s total across a few attempts, enough for
// a running daemon to finish reloading its config (and so start reporting a
// pin for the token just saved) without making `set-token` feel slow.
const (
	enrollPINFetchAttempts = 5
	enrollPINFetchDelay    = 300 * time.Millisecond
)

// fetchEnrollmentPINFn is the seam printEnrollmentPIN calls to learn the
// daemon's current enrollment pin: the real implementation (below) dials
// the control socket with a brief retry; tests override this var with a
// canned result so cmdTelegram's print behavior can be exercised without a
// real socket/daemon.
var fetchEnrollmentPINFn = dialEnrollmentPIN

// dialEnrollmentPIN resolves the control socket path/token the same way
// cmdFrontDoor does (plugin_launch.go) and calls EnrollmentPIN over it,
// retrying up to enrollPINFetchAttempts times (enrollPINFetchDelay apart) so
// a daemon that is still applying the SIGHUP reloadDaemon() just sent has
// time to pick up the new token before this gives up.
func dialEnrollmentPIN() (pin string, enrolled bool, err error) {
	socketPath := controlSocketPath()
	tokenBytes, _ := os.ReadFile(controlTokenPath())
	token := strings.TrimSpace(string(tokenBytes))

	for attempt := 0; attempt < enrollPINFetchAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(enrollPINFetchDelay)
		}
		client, dialErr := control.Dial(socketPath, token)
		if dialErr != nil {
			err = dialErr
			continue
		}
		pin, enrolled, err = client.EnrollmentPIN(context.Background())
		client.Close()
		if err == nil {
			return pin, enrolled, nil
		}
	}
	return "", false, err
}

// printEnrollmentPIN prints the /start <pin> instruction after `telegram
// set-token` has already saved the token, using fetchEnrollmentPINFn to
// learn the daemon's current pin. The token is saved and reloadDaemon()
// already sent by the time this runs, so a fetch failure (daemon not
// installed/running, or the retries in dialEnrollmentPIN all erroring) is
// NEVER treated as set-token's own failure -- it only changes which message
// is printed, never the exit code.
func printEnrollmentPIN() {
	pin, enrolled, err := fetchEnrollmentPINFn()
	switch {
	case err == nil && pin != "":
		fmt.Fprintln(stdout, "Telegram token saved. To finish enrollment, from your Telegram account message the bot:")
		fmt.Fprintf(stdout, "  /start %s\n", pin)
	case err == nil && enrolled:
		fmt.Fprintln(stdout, "Telegram token saved. The bot is already enrolled.")
	default:
		fmt.Fprintln(stdout, "Telegram token saved. The daemon will log the enrollment PIN on start:")
		fmt.Fprintln(stdout, "  journalctl -u serverwatch | grep /start")
	}
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
