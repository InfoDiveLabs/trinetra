package trinetra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

const unitPath = "/etc/systemd/system/trinetra.service"

// secondaryBinPath is a symlink install adds alongside the real /usr/local/bin/trinetra so
// that `sudo trinetra ...` resolves on distros whose sudo secure_path omits /usr/local/bin.
const secondaryBinPath = "/usr/bin/trinetra"

// renderUnit renders the systemd unit file installed by cmdInstall.
func renderUnit(binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Trinetra — self-hosted server & fleet monitor
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s daemon
Restart=always
RestartSec=5
WatchdogSec=90
User=root
RuntimeDirectory=trinetra
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, binPath)
}

func cmdInstall(args []string) int {
	force := false
	requireSigned := false
	var opts planOptions
	for _, a := range args {
		switch a {
		case "--force":
			// Only relaxes the serverwatch migration's "is the old daemon really stopped?" checks:
			// systemctl cannot answer, or a serverwatch daemon runs outside serverwatch.service.
			force = true
		case "--state-already-at-new-path":
			// The operator moved the serverwatch state volume to the
			// trinetra state path; adopt it instead of refusing.
			opts.stateAtNewPath = true
		case "--require-signed":
			// Refuse an unsigned install outright instead of warning and continuing (see
			// installBinaryAndUnit/verifyInstallBundle).
			requireSigned = true
		default:
			fmt.Fprintf(stderr, "unknown install flag %q\nusage: install [--force] [--require-signed] [--state-already-at-new-path]\n", a)
			return 2
		}
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// An existing serverwatch install is migrated in place first (see
	// migrate_legacy.go); planning only looks, so a refusal changes nothing.
	plan, err := planLegacyMigration(defaultMigrationPaths(), opts)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	install := func() error { return installBinaryAndUnit(self, requireSigned) }
	var summary *migrationSummary
	if plan != nil {
		plan.force = force
		progressOut = stdout
		fmt.Fprintln(stdout, "found a serverwatch install; migrating it to trinetra")
		summary, err = applyLegacyMigration(plan, osMigrationOps{}, install)
	} else {
		err = install()
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	waitErr := waitForDaemonFn()
	switch {
	case waitErr == nil:
		fmt.Fprintln(stdout, installedPluginsMessage()+" installed and started; monitoring this server.")
		if h := telegramInstallHint(); h != "" {
			fmt.Fprintln(stdout, h)
		}
		fmt.Fprint(stdout, installNextSteps())
	case errors.Is(waitErr, errServiceSlow):
		fmt.Fprintln(stdout, installedPluginsMessage()+" installed; the daemon is still starting. Give it a moment before `trinetra cli` (systemctl status trinetra).")
	default:
		fmt.Fprintln(stdout, installedPluginsMessage()+" installed.")
	}
	if summary != nil {
		fmt.Fprint(stdout, summary.String())
	}
	if waitErr != nil && !errors.Is(waitErr, errServiceSlow) {
		fmt.Fprintf(stderr, "%v: see `journalctl -u trinetra -n 50` for why it failed to start\n", waitErr)
		return 1
	}
	return 0
}

// installBinaryAndUnit is the normal install: verify a signed release manifest (if present)
// against self and any companion plugins sitting next to it, copy the running binary.
func installBinaryAndUnit(self string, requireSigned bool) error {
	names := companionInstallNames(self)
	m, verified, err := verifyInstallSignature(self, names, requireSigned)
	if err != nil {
		return err
	}
	// updatePaths -- NOT the bare package-level stateDir -- is where install's floor
	// check/raise must read and write (fix round 1, Ruling R8): the update state.
	paths := defaultUpdatePaths()
	unlock, err := installPreflight(paths)
	if err != nil {
		return err
	}
	defer unlock()
	if verified {
		if err := checkInstallPolicy(paths, m, currentInstalledVersion()); err != nil {
			return err
		}
	}

	dst := "/usr/local/bin/trinetra"
	if err := copyFile(self, dst, 0o755); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}
	// Also expose the binary on /usr/bin, which is on sudo's secure_path on every common
	// distro (unlike /usr/local/bin, absent on RHEL/CentOS 7 and some minimal images).
	if err := linkOnPath(dst, secondaryBinPath); err != nil {
		fmt.Fprintf(stderr, "warning: could not link %s -> %s: %v\n", secondaryBinPath, dst, err)
	}
	// Copy any companion plugin binaries (trinetra-ctl, trinetra-web) sitting next to the
	// SOURCE binary (self) into the same directory dst was just installed into.
	if err := copyPluginsAlongside(filepath.Dir(self), filepath.Dir(dst)); err != nil {
		fmt.Fprintf(stderr, "warning: could not copy plugin binaries: %v\n", err)
	}
	// Record the checksum manifest the safe plugin launcher (plugin_launch.go) verifies
	// companion binaries against before exec'ing them.
	if err := writePluginManifest(filepath.Dir(dst)); err != nil {
		fmt.Fprintf(stderr, "warning: could not write plugin manifest: %v\n", err)
	}
	if err := os.WriteFile(unitPath, []byte(renderUnit(dst)), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	// The self-update safety net (R14): the pinned guard binary (a copy of this binary) and
	// the watchdog timer that runs it every minute to resolve any pending update.
	if err := writePinnedGuard(paths); err != nil {
		return err
	}
	if err := ensureWatchdog(paths, osExec{}); err != nil {
		return err
	}
	// seed config if absent
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if c, _ := loadCfg(); c != nil {
			_ = saveCfg(c)
		}
	}
	x := osExec{}
	// enable + restart (not `enable --now`): `enable --now` only STARTS a stopped service.
	for _, a := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "trinetra"},
		{"systemctl", "restart", "trinetra"},
	} {
		if out, err := x.Run(a[0], a[1:]...); err != nil {
			return fmt.Errorf("%v: %v\n%s", a, err, out)
		}
	}
	if verified {
		// Best-effort: a floor-raise failure must not undo an otherwise successful install (the
		// binaries are already copied and the unit already (re)started above).
		raiseInstallFloor(paths, m.Version)
	}
	return nil
}

// installPreflight claims the self-update apply lock for the whole install (R16) and
// refuses while an update is pending.
func installPreflight(paths updatePaths) (unlock func(), err error) {
	unlock, err = takeApplyLock(paths)
	if err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	st, err := update.LoadState(paths.dir())
	if err != nil {
		unlock()
		return nil, fmt.Errorf("install: %w", err)
	}
	if st.Pending != nil {
		unlock()
		return nil, fmt.Errorf("install: an update to %s is pending; wait until it is confirmed or rolled back (trinetra update status), then install again", st.Pending.Version)
	}
	return unlock, nil
}

// errNoSignedManifest is returned by verifyInstallBundle when manifest.json is absent from
// filepath.Dir(self).
var errNoSignedManifest = errors.New("trinetra: no signed manifest found next to the binary")

// companionInstallNames lists the binary names verifyInstallSignature should verify:
// "trinetra" (self) always, plus each companion plugin.
func companionInstallNames(self string) []string {
	names := []string{"trinetra"}
	dir := filepath.Dir(self)
	for _, n := range pluginManifestNames {
		p := filepath.Join(dir, "trinetra-"+n)
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			names = append(names, "trinetra-"+n)
		}
	}
	return names
}

// verifyInstallBundle looks for manifest.json/manifest.ci.sig/ manifest.maint.sig next to
// self (filepath.Dir(self)), verifies both signatures via update.VerifyRelease.
func verifyInstallBundle(keys update.KeySet, self string, names []string) (update.Manifest, error) {
	dir := filepath.Dir(self)
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return update.Manifest{}, errNoSignedManifest
		}
		return update.Manifest{}, fmt.Errorf("install: read manifest.json: %w", err)
	}
	ciSig, err := os.ReadFile(filepath.Join(dir, "manifest.ci.sig"))
	if err != nil {
		return update.Manifest{}, fmt.Errorf("install: read manifest.ci.sig: %w", err)
	}
	maintSig, err := os.ReadFile(filepath.Join(dir, "manifest.maint.sig"))
	if err != nil {
		return update.Manifest{}, fmt.Errorf("install: read manifest.maint.sig: %w", err)
	}
	m, err := update.VerifyRelease(keys, mb, ciSig, maintSig)
	if err != nil {
		return update.Manifest{}, fmt.Errorf("install: %w", err)
	}

	arch := runtime.GOARCH
	for _, name := range names {
		path := self
		if name != "trinetra" {
			path = filepath.Join(dir, name)
		}
		want := name + "-linux-" + arch
		var file *update.File
		for i := range m.Files {
			if m.Files[i].Name == want && m.Files[i].Arch == arch {
				file = &m.Files[i]
				break
			}
		}
		if file == nil {
			return update.Manifest{}, fmt.Errorf("install: manifest has no entry for %s", want)
		}
		got, herr := update.HashFile(path)
		if herr != nil {
			return update.Manifest{}, fmt.Errorf("install: %s: %w", path, herr)
		}
		if got != file.SHA256 {
			return update.Manifest{}, fmt.Errorf("install: %s does not match the signed manifest (sha256 %s, expected %s); refusing", path, got, file.SHA256)
		}
	}
	return m, nil
}

// verifyInstallSignature wraps verifyInstallBundle with cmdInstall's
// present/absent/require-signed policy: a present-and-valid manifest returns.
func verifyInstallSignature(self string, names []string, requireSigned bool) (update.Manifest, bool, error) {
	m, err := verifyInstallBundle(update.ProductionKeys(), self, names)
	switch {
	case err == nil:
		return m, true, nil
	case errors.Is(err, errNoSignedManifest):
		if requireSigned {
			return update.Manifest{}, false, fmt.Errorf("install: no signed manifest next to %s; download manifest.json, manifest.ci.sig and manifest.maint.sig with the binaries", self)
		}
		fmt.Fprintln(stderr, "warning: installing an unsigned build (no manifest.json next to the binary)")
		return update.Manifest{}, false, nil
	default:
		return update.Manifest{}, false, err
	}
}

// checkInstallPolicy applies update.CheckPolicy to a verified install manifest: AllowEqual
// true (reinstalling the currently installed version is a legitimate repair).
func checkInstallPolicy(paths updatePaths, m update.Manifest, running update.Version) error {
	st, err := update.LoadState(paths.dir())
	if err != nil {
		return err
	}
	floor, hasFloor, err := st.FloorVersion(running)
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	pol := update.Policy{Channel: m.Channel, Floor: floor, HasFloor: hasFloor, Running: running, AllowEqual: true}
	if running == (update.Version{}) {
		pol.Running, _ = update.ParseVersion(m.MinUpgradeFrom)
	}
	if err := update.CheckPolicy(m, pol); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	return nil
}

// currentInstalledVersion reports the version of whatever binary is currently at
// /usr/local/bin/trinetra.
func currentInstalledVersion() update.Version {
	const dst = "/usr/local/bin/trinetra"
	fi, err := os.Stat(dst)
	if err != nil || !fi.Mode().IsRegular() {
		return update.Version{}
	}
	out, err := osExec{}.Run(dst, "version", "--json")
	if err != nil {
		return update.Version{}
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(out, &v) != nil {
		return update.Version{}
	}
	ver, _ := update.ParseVersion(strings.TrimPrefix(v.Version, "v"))
	return ver
}

// raiseInstallFloor records m's version as the new update floor.
func raiseInstallFloor(paths updatePaths, version string) {
	v, err := update.ParseVersion(version)
	if err != nil {
		return
	}
	_ = update.WithState(paths.dir(), func(st *update.State) error {
		st.RaiseFloor(v)
		return nil
	})
}

// telegramInstallHint is "" unless a Telegram token is set but no chat has
// been enrolled yet: Telegram is one optional channel among several.
func telegramInstallHint() string {
	c, err := loadCfg()
	if err != nil || c == nil || c.Telegram.Token == "" || c.Telegram.ChatID != "" {
		return ""
	}
	return "Telegram token already set; enroll the chat by messaging the bot /start <pin>."
}

func installNextSteps() string {
	return "Next:\n" +
		"  sudo trinetra cli                          guided setup: web UI, first admin, alerts\n" +
		"  sudo trinetra users invite --role admin    just the first admin's enroll link\n"
}

// noChannelReminder is "" once any alert channel is enabled.
func noChannelReminder(c *config.Config) string {
	for _, ch := range c.Channels {
		if ch.Enabled {
			return ""
		}
	}
	return "No alert channel yet: alerts only show in the web UI and trinetra cli. Add one there, or with: trinetra channel add"
}

// installedPluginsMessage reports which companion plugins ended up recorded in the plugin
// manifest after copyPluginsAlongside/writePluginManifest ran.
func installedPluginsMessage() string {
	manifest, err := loadPluginManifest()
	if err != nil || len(manifest) == 0 {
		return "no plugin binaries found alongside the source binary (trinetra-ctl/trinetra-web skipped);"
	}
	var names []string
	for _, name := range pluginManifestNames {
		if _, ok := manifest[name]; ok {
			names = append(names, "trinetra-"+name)
		}
	}
	if len(names) == 0 {
		return "no plugin binaries found alongside the source binary (trinetra-ctl/trinetra-web skipped);"
	}
	return "installed plugins: " + strings.Join(names, ", ") + ";"
}

// copyPluginsAlongside copies each companion plugin binary ("trinetra-<name>" for every
// name in pluginManifestNames) found in srcDir into dstDir at mode 0755.
func copyPluginsAlongside(srcDir, dstDir string) error {
	var errs []string
	for _, name := range pluginManifestNames {
		binName := "trinetra-" + name
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

// linkOnPath ensures link is a symlink to target, so `sudo <name>` resolves on distros
// whose secure_path omits target's directory.
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

// unlinkOnPath removes link only when it is still OUR symlink pointing at target, so
// uninstall never deletes a distro-provided real binary or a symlink someone else created.
func unlinkOnPath(target, link string) {
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return
	}
	if dest, _ := os.Readlink(link); dest == target {
		_ = os.Remove(link)
	}
}

// pluginManifestNames lists the companion binary name suffixes.
var pluginManifestNames = []string{"ctl", "web"}

// writePluginManifest scans binDir (the directory the daemon binary was just installed
// into) for companion plugin binaries and writes <stateDir>/plugins.json.
func writePluginManifest(binDir string) error {
	manifest := make(map[string]string)
	for _, name := range pluginManifestNames {
		path := filepath.Join(binDir, "trinetra-"+name)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat %s: %w", path, err)
		}
		// Skip anything that is not a plain regular file (e.g. a symlink or directory left behind
		// by something else).
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
	// update.WriteFileAtomic writes a fresh temp file at exactly 0600, fsyncs it, renames it
	// over plugins.json and fsyncs the directory.
	if err := update.WriteFileAtomic(pluginManifestPath(), b, 0o600); err != nil {
		return fmt.Errorf("write plugin manifest %s: %w", pluginManifestPath(), err)
	}
	return nil
}

// removeInstalledPlugins removes each companion plugin binary ("trinetra-<name>" for every
// name in pluginManifestNames) from binDir.
func removeInstalledPlugins(binDir string) {
	for _, name := range pluginManifestNames {
		_ = os.Remove(filepath.Join(binDir, "trinetra-"+name))
	}
}

func cmdUninstall(args []string) int {
	x := osExec{}
	_, _ = x.Run("systemctl", "disable", "--now", "trinetra")
	_ = os.Remove(unitPath)
	removeWatchdog(defaultUpdatePaths(), x)
	// Remove the /usr/bin shortcut, but only if it is still OUR symlink into
	// /usr/local/bin (never a distro-provided real binary).
	unlinkOnPath("/usr/local/bin/trinetra", secondaryBinPath)
	// The serverwatch compat links a migration left (only if still ours).
	unlinkOnPath(legacyBinFilePath, legacyUsrBinPath)
	unlinkOnPath("/usr/local/bin/trinetra", legacyBinFilePath)
	// Best-effort, like the other uninstall cleanups above: a plugin the front-door can no
	// longer verify against is safer than a stale manifest left lying around after uninstall.
	_ = os.Remove(pluginManifestPath())
	// Symmetric with cmdInstall's copyPluginsAlongside: remove the plugin binaries install
	// placed next to the daemon.
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

// copyFile copies src to dst atomically and durably (update.CopyFile): a random
// same-directory temp file, fsync, rename.
func copyFile(src, dst string, perm os.FileMode) error {
	return update.CopyFile(src, dst, perm)
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

// enrollPINFetchAttempts/enrollPINFetchDelay bound how long fetchEnrollmentPIN retries
// dialing the control socket after set-token's reloadDaemon() SIGHUP.
const (
	enrollPINFetchAttempts = 5
	enrollPINFetchDelay    = 300 * time.Millisecond
)

// fetchEnrollmentPINFn is the seam printEnrollmentPIN calls to learn the daemon's current
// enrollment pin: the real implementation.
var fetchEnrollmentPINFn = dialEnrollmentPIN

// dialEnrollmentPIN resolves the control socket path/token the same way cmdFrontDoor does
// (plugin_launch.go) and calls EnrollmentPIN over it.
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

// printEnrollmentPIN prints the /start <pin> instruction after `telegram set-token` has
// already saved the token, using fetchEnrollmentPINFn to learn the daemon's current pin.
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
		fmt.Fprintln(stdout, "  journalctl -u trinetra | grep /start")
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
	case "off":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: schedule daily HH:MM | weekly dow@HH:MM | off")
			return 2
		}
		for _, k := range []string{"schedule.daily", "schedule.weekly"} {
			if err := c.Set(k, ""); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
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
	// Round-1 review, IMPORTANT 2: this dedicated command is a second, separate path onto
	// quiet_hours besides `config set`/the web config page (both already guarded).
	if id, managed := ManagedFragmentFor(stateDir, "quiet_hours"); managed {
		fmt.Fprintf(stderr, "quiet_hours: managed by the fleet master (fragment %s); change it on the master\n", id)
		return 1
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
	if c, err := loadCfg(); err == nil {
		if r := noChannelReminder(c); r != "" {
			fmt.Fprintln(stderr, r)
		}
	}
	return 0
}

func cmdDoctor(args []string) int {
	x := osExec{}
	fs := osFS{}

	// Cardinality/disk guardrail visibility (docs/handbook/12-roadmap-and-status.md Epic #69
	// x7): a corrupt config just falls back to defaults here.
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

// buildDoctorReport runs the same probes `trinetra doctor` has always run inline.
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
	if n >= seriesCountWarnThreshold {
		rep.StoreWarning = fmt.Sprintf(
			"high series cardinality: %d series (healthy is low hundreds). "+
				"Dead series are reaped after retention; a persistently high count usually means "+
				"ephemeral targets churning (e.g. Swarm task-keyed containers, see #118).", n)
	}
	return rep
}

// seriesCountWarnThreshold is the doctor guardrail line for tsfile cardinality (#112): a
// healthy host tracks its live targets (low hundreds of series).
const seriesCountWarnThreshold = 1000

// renderDoctorReport writes rep to w in the exact line-for-line format cmdDoctor has always
// printed.
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
	if rep.StoreWarning != "" {
		fmt.Fprintf(w, "WARNING: %s\n", rep.StoreWarning)
	}
}

// onOff renders a bool as "on"/"off" for the doctor collector summary.
func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}
