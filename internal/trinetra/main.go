package trinetra

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

// Default paths. Exported so other packages (install scripts, docs) can
// reference the canonical locations.
const (
	ConfigPath = "/etc/trinetra/config.json"
	StateDir   = "/var/lib/trinetra"
)

// Overridable for tests.
var (
	cfgPath            = ConfigPath
	stateDir           = StateDir
	stdout   io.Writer = os.Stdout
	stderr   io.Writer = os.Stderr
	// argv0 is the name this binary was invoked as (overridable for tests).
	argv0 = func() string { return os.Args[0] }
)

// Main dispatches CLI subcommands and returns an exit code.
func Main(args []string) int {
	// Invoked through the compat symlink /usr/local/bin/serverwatch left by
	// the migration (see migrate_legacy.go): work normally, but say so.
	if filepath.Base(argv0()) == "serverwatch" {
		fmt.Fprintln(stderr, "serverwatch is now trinetra; this name will be removed in the next release")
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	// Commands that write the config or state must not create the trinetra
	// paths next to an unmigrated serverwatch install (install would then
	// refuse to merge the two). Read-only commands are not affected.
	if writesConfigOrState(args) {
		if err := legacyWriteGuard(defaultMigrationPaths()); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	switch args[0] {
	case "config":
		return cmdConfig(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	case "install":
		return cmdInstall(args[1:])
	case "uninstall":
		return cmdUninstall(args[1:])
	case "daemon":
		return cmdDaemon(args[1:])
	case "telegram":
		return cmdTelegram(args[1:])
	case "monitor":
		return cmdMonitor(args[1:])
	case "schedule":
		return cmdSchedule(args[1:])
	case "quiet-hours":
		return cmdQuietHours(args[1:])
	case "healthchecks":
		return cmdHealthchecks(args[1:])
	case "channel":
		return cmdChannel(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "version":
		if len(args) > 1 && args[1] == "--json" {
			b, _ := json.Marshal(map[string]string{"version": version.String()})
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		fmt.Fprintln(stdout, version.String())
		return 0
	case "doctor":
		return cmdDoctor(args[1:])
	case "migrate":
		return cmdMigrate(args[1:])
	case "dump":
		return cmdDump(args[1:])
	case "downtime":
		return cmdDowntime(args[1:])
	case "alerts":
		return cmdAlerts(args[1:])
	case "fleet":
		return cmdFleet(args[1:])
	case "status-page":
		return cmdStatusPage(args[1:])
	case "update":
		return cmdUpdate(args[1:])
	case "cli":
		return cmdFrontDoor("cli", "ctl", args[1:])
	case "web":
		return cmdFrontDoor("web", "web", args[1:])
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s\n", args[0], usage)
		return 2
	}
}

// writesConfigOrState reports whether args is a CLI command that writes the
// config file or the state dir itself (not through the running daemon).
// doctor and dump count: opening the sample store creates the state dir's
// ts/ tree, which would make a later install refuse with "found both".
func writesConfigOrState(args []string) bool {
	sub := ""
	if len(args) > 1 {
		sub = args[1]
	}
	in := func(opts ...string) bool {
		for _, o := range opts {
			if sub == o {
				return true
			}
		}
		return false
	}
	switch args[0] {
	case "config":
		return in("set", "unset")
	case "telegram":
		return in("set-token")
	case "monitor":
		return in("enable", "disable", "threshold")
	case "schedule", "quiet-hours", "healthchecks":
		return sub != ""
	case "channel":
		return in("add", "remove", "set")
	case "downtime":
		return in("purge")
	case "alerts":
		return in("ack", "unack")
	case "migrate", "doctor", "dump":
		return true
	case "fleet":
		return in("init", "join", "leave", "disable")
	case "status-page":
		return (in("service") && len(args) > 2 && (args[2] == "add" || args[2] == "rm")) ||
			(in("incident") && len(args) > 2 && (args[2] == "open" || args[2] == "update" || args[2] == "resolve"))
	case "update":
		return in("apply", "rollback", "guard")
	}
	return false
}

const usage = `trinetra -- home server monitor
usage:
  trinetra config get [key]
  trinetra config set <key> <value>
  trinetra config unset <key>
  trinetra install [--force] [--require-signed] [--state-already-at-new-path]
                                        # --force: migrate even if systemctl cannot confirm the old serverwatch service stopped,
                                        #          or a serverwatch daemon is running outside it
                                        # --require-signed: refuse to install unless a signed manifest.json (+ manifest.ci.sig,
                                        #          manifest.maint.sig) sits next to the binary being installed
                                        # --state-already-at-new-path: adopt a serverwatch state volume you remounted at /var/lib/trinetra
  trinetra uninstall [--purge]
  trinetra daemon
  trinetra telegram set-token <token>
  trinetra monitor list|enable|disable|threshold
  trinetra schedule daily HH:MM | weekly dow@HH:MM | off
  trinetra quiet-hours <HH-HH>|off
  trinetra healthchecks set <url> | off
  trinetra channel list|add|remove|set|test
  trinetra status
  trinetra version [--json]
  trinetra doctor
  trinetra migrate [--force]
  trinetra dump --metric <id> [--since 24h] [--res raw|1m] [--format csv|json]
  trinetra downtime purge [--type power_down] [--max-seconds 300]
  trinetra alerts [list] [--since 24h] [--limit 20]
  trinetra alerts ack <key>
  trinetra alerts unack <key>
  trinetra fleet init --address HOST[,IP] [--port 9443]
  trinetra fleet join <code> [--name NAME] | leave [--purge] | disable [--purge]
  trinetra fleet status | nodes [--tag T] [--state S] [--q TEXT]
  trinetra fleet node revoke|remove|rename|tag <node> [value]
  trinetra fleet token create [--tags a,b] [--ttl 1h] [--uses 1] | list | delete <id>
  trinetra status-page service|incident ...   # manage the public status page (services, incidents)
  trinetra update status [--json] | check | apply [--version V] [--bundle DIR] [--channel C] [--force] | rollback
  trinetra cli                       # interactive management (trinetra-ctl)
  trinetra web                       # web UI (trinetra-web)`

func loadCfg() (*config.Config, error) { return config.Load(cfgPath) }

func saveCfg(c *config.Config) error { return c.Save(cfgPath) }

// configForDisplay returns a shallow copy of c with every collect.* toggle
// resolved to its effective value (nil -> the documented default, true).
// c.Collect's *bool fields carry `omitempty` so Save/Load can tell "never
// set" from "explicitly false" apart on disk; a raw json.Marshal of c would
// therefore silently drop any toggle still at its default, which is exactly
// the cardinality/disk-cost information `config get` (full dump) exists to
// surface. The copy is shallow (maps/slices like Targets/Channels stay
// shared with c) since only Collect's value fields are mutated here, and c
// itself is never touched.
func configForDisplay(c *config.Config) *config.Config {
	d := *c
	containerStats := c.ContainerStatsEnabled()
	netThroughput := c.NetThroughputEnabled()
	services := c.ServicesEnabled()
	processes := c.ProcessesEnabled()
	smartAttrs := c.SmartAttrsEnabled()
	d.Collect.ContainerStats = &containerStats
	d.Collect.NetThroughput = &netThroughput
	d.Collect.Services = &services
	d.Collect.Processes = &processes
	d.Collect.SmartAttrs = &smartAttrs
	d.Collect.SmartInterval = c.SmartIntervalSec()
	// Secrets never appear in the clear in a full-config display (CLI dump,
	// eventually the web config page): "(set)"/"(not set)" instead, matching
	// how a single-key `config get <secret key>` redacts (see cmdConfig).
	if d.Update.GitHubToken != "" {
		d.Update.GitHubToken = "(set)"
	}
	if d.Telegram.Token != "" {
		d.Telegram.Token = "(set)"
	}
	return &d
}

func cmdConfig(args []string) int {
	c, err := loadCfg()
	if len(args) == 0 {
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stderr, "usage: config get|set|unset")
		return 2
	}
	// A corrupt config must stay CLI-repairable: for the mutating set/unset
	// subcommands, fall back to defaults on a load error so `config set ...` can
	// rewrite a clean file. `get` keeps erroring (nothing to repair by reading).
	if err != nil {
		switch args[0] {
		case "set", "unset":
			fmt.Fprintf(stderr, "warning: existing config unreadable (%v); starting from defaults\n", err)
			c = config.Default()
		default:
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	switch args[0] {
	case "get":
		if len(args) == 1 {
			b, _ := jsonMarshalIndent(configForDisplay(c))
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		v, ok := c.Get(args[1])
		if !ok {
			fmt.Fprintf(stderr, "unknown key %q\n", args[1])
			return 1
		}
		if config.IsSecretKey(args[1]) {
			if v != "" {
				v = "(set)"
			} else {
				v = "(not set)"
			}
		}
		fmt.Fprintln(stdout, v)
		return 0
	case "set":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "usage: config set <key> <value>")
			return 2
		}
		if id, managed := ManagedFragmentFor(stateDir, args[1]); managed {
			fmt.Fprintf(stderr, "%s: managed by the fleet master (fragment %s); change it on the master\n", args[1], id)
			return 1
		}
		if err := c.Set(args[1], args[2]); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := saveCfg(c); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		reloadDaemon() // best-effort SIGHUP; no-op if not running
		return 0
	case "unset":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: config unset <key>")
			return 2
		}
		if id, managed := ManagedFragmentFor(stateDir, args[1]); managed {
			fmt.Fprintf(stderr, "%s: managed by the fleet master (fragment %s); change it on the master\n", args[1], id)
			return 1
		}
		if err := c.Unset(args[1]); err != nil {
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
	fmt.Fprintf(stderr, "unknown config subcommand %q\n", args[0])
	return 2
}

func jsonMarshalIndent(v any) ([]byte, error) {
	return jsonIndent(v)
}

// saveDaemonCfg is saveCfg for every save that is NOT a `trinetra fleet`
// command (daemon reload/ApplyConfig, the Telegram chat-id capture, other
// plugins' config writes): it first overlays the fleet identity keys from
// the config currently on disk onto c, so a config built before a
// `fleet init|join|leave|disable` (or by a plugin that knows nothing about
// fleet) can never wipe or change them. c is modified in place so the
// in-memory config matches what was saved.
func saveDaemonCfg(c *config.Config) error {
	// A missing file loads as defaults (no fleet identity). An unreadable
	// or corrupt file cannot be trusted either way, so c is saved as given.
	if onDisk, err := config.Load(cfgPath); err == nil {
		c.KeepFleetIdentity(onDisk)
	}
	return saveCfg(c)
}
