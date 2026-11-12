package trinetra

import (
	"fmt"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const pluginDataRoot = "/var/lib/trinetra-plugins"

// cmdPluginExec <id> <binPath> <tokenPath>: apply Rlimits, PR_SET_NO_NEW_PRIVS, setgid/setuid(trinetra-plugin), set Env*, exec.
func cmdPluginExec(args []string) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "usage: trinetra plugin-exec <id> <binary> <token-file>")
		return 2
	}
	if !plugin.IDPattern.MatchString(args[0]) {
		fmt.Fprintf(stderr, "trinetra plugin-exec: invalid plugin id %q\n", args[0])
		return 2
	}
	return runPluginExec(args[0], args[1], args[2])
}
