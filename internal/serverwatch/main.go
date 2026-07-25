package serverwatch

import (
	"fmt"
	"io"
	"os"

	"serverwatch/internal/config"
)

// Default paths. Exported so other packages (install scripts, docs) can
// reference the canonical locations.
const (
	ConfigPath = "/etc/serverwatch/config.json"
	StateDir   = "/var/lib/serverwatch"
)

// Overridable for tests.
var (
	cfgPath            = ConfigPath
	stateDir           = StateDir
	stdout   io.Writer = os.Stdout
	stderr   io.Writer = os.Stderr
)

// Main dispatches CLI subcommands and returns an exit code.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
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
	case "status":
		return cmdStatus(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s\n", args[0], usage)
		return 2
	}
}

const usage = `serverwatch — home server monitor
usage:
  serverwatch config get [key]
  serverwatch config set <key> <value>
  serverwatch config unset <key>
  serverwatch install
  serverwatch uninstall [--purge]
  serverwatch daemon
  serverwatch telegram set-token <token>
  serverwatch monitor list|enable|disable|threshold
  serverwatch schedule daily HH:MM | weekly dow@HH:MM | off
  serverwatch quiet-hours <HH-HH>|off
  serverwatch healthchecks set <url> | off
  serverwatch status
  serverwatch doctor`

func loadCfg() (*config.Config, error) { return config.Load(cfgPath) }

func saveCfg(c *config.Config) error { return c.Save(cfgPath) }

func cmdConfig(args []string) int {
	c, err := loadCfg()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: config get|set|unset")
		return 2
	}
	switch args[0] {
	case "get":
		if len(args) == 1 {
			b, _ := jsonMarshalIndent(c)
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		v, ok := c.Get(args[1])
		if !ok {
			fmt.Fprintf(stderr, "unknown key %q\n", args[1])
			return 1
		}
		fmt.Fprintln(stdout, v)
		return 0
	case "set":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "usage: config set <key> <value>")
			return 2
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
