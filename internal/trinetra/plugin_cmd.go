package trinetra

import (
	"fmt"
	"os"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const pluginUsage = `usage:
  trinetra plugin catalog [--refresh]
  trinetra plugin install <id[@version] | file | url>
  trinetra plugin enable <id>
  trinetra plugin disable <id>
  trinetra plugin remove <id>
  trinetra plugin update [<id> | --all]
  trinetra plugin set <id> key=value [key=value ...]
  trinetra plugin list
  trinetra plugin status <id>
  trinetra plugin push <id> (--nodes a,b | --tag T) [--remove]
  trinetra plugin dev <dir>`

var (
	pluginCatalogHandler = cmdPluginCatalog
	pluginPushHandler    = cmdPluginPush
	pluginRuntimeHandler = cmdPluginRuntime
)

// cmdPlugin dispatches `trinetra plugin <sub>`.
func cmdPlugin(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, pluginUsage)
		return 2
	}
	switch sub := args[0]; sub {
	case "catalog":
		return pluginCatalogHandler(args[1:])
	case "push":
		return pluginPushHandler(args[1:])
	case "install", "enable", "disable", "remove", "update", "set", "list", "status", "dev":
		return pluginRuntimeHandler(sub, args[1:])
	}
	fmt.Fprintln(stderr, pluginUsage)
	return 2
}

// pluginWithClient runs f against the daemon's plugin manager (test seam).
var pluginWithClient = func(f func(plugin.Manager) error) error {
	tok, _ := os.ReadFile(controlTokenPath())
	c, err := control.Dial(controlSocketPath(), strings.TrimSpace(string(tok)))
	if err != nil {
		return errDaemonDown
	}
	defer c.Close()
	return f(c.Plugins())
}
