// Command serverwatch-ctl is a separate-process client for a running
// serverwatch daemon: it dials the daemon's control socket (internal/control)
// to obtain a core.API and drives it. This file wires the transport
// (resolve socket + token, control.Dial, defer Close) to either run() --
// which holds the non-interactive subcommand logic (status/doctor/alerts),
// tested against a fake core.API without a real socket -- or, when invoked
// with no subcommand at all, runInteractive() (tui.go): a Bubble Tea TUI
// with a live-status home screen and a guided "set up the web UI" wizard.
//
// This binary is the one place in the module allowed to import third-party
// terminal UI packages (github.com/charmbracelet/bubbletea/bubbles/
// lipgloss, see tui.go); nothing cmd/serverwatch reaches imports this
// package, so the default daemon build stays stdlib-only (enforced by
// internal/serverwatch/buildtag_test.go's TestDefaultBuildIsStdlibOnly,
// scoped to cmd/serverwatch's own graph for exactly this reason).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/InfoDiveLabs/trinetra/internal/control"
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain parses the global flags, resolves and dials the control socket,
// and hands the resulting core.API to run (for a named subcommand) or
// runInteractive (for none, i.e. `serverwatch-ctl` on its own -- see
// tui.go). It is split out from main so the os.Exit lives in exactly one
// place; the socket dialing here is what run's and the TUI model's tests
// replace with a fake core.API.
func realMain(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("serverwatch-ctl", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socketFlag := fs.String("socket", "", "control socket path (overrides $SERVERWATCH_CONTROL_SOCKET and the default)")
	tokenFlag := fs.String("token", "", "control token file path (overrides $SERVERWATCH_CONTROL_TOKEN and the default)")
	// --json is accepted as a global flag here so it works BEFORE the
	// subcommand (`--json status`); flag.Parse stops at the first non-flag
	// arg, so the after-subcommand form (`status --json`) instead reaches run
	// as a positional arg, which run strips itself. Normalizing both to a
	// leading "--json" below means run handles the two orders identically.
	jsonFlag := fs.Bool("json", false, "emit JSON for status/doctor/alerts")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	sock := resolveSocketPath(*socketFlag)
	token, err := resolveToken(*tokenFlag, sock)
	if err != nil {
		fmt.Fprintf(errOut, "serverwatch-ctl: reading control token: %v\n", err)
		return 1
	}

	client, err := control.Dial(sock, token)
	if err != nil {
		fmt.Fprintf(errOut, "serverwatch-ctl: dialing control socket %s: %v\n", sock, err)
		return 1
	}
	defer client.Close()

	if fs.NArg() == 0 {
		return runInteractive(client, errOut)
	}
	cmdArgs := fs.Args()
	if *jsonFlag {
		cmdArgs = append([]string{"--json"}, cmdArgs...)
	}
	return run(client, cmdArgs, out)
}
