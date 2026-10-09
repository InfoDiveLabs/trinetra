// Command trinetra-ctl is a separate-process client for a running
// trinetra daemon: it dials the daemon's control socket (internal/control)
// to obtain a core.API and drives it. This file wires the transport
// (resolve socket + token, control.Dial, defer Close) to either run() --
// which holds the non-interactive subcommand logic (status/doctor/alerts),
// tested against a fake core.API without a real socket -- or, when invoked
// with no subcommand at all, runInteractive() (tui.go): a Bubble Tea TUI
// with a live-status home screen and a guided "set up the web UI" wizard.
//
// This binary is the one place in the module allowed to import third-party
// terminal UI packages (github.com/charmbracelet/bubbletea/bubbles/
// lipgloss, see tui.go); nothing cmd/trinetra reaches imports this
// package, so the default daemon build stays stdlib-only (enforced by
// internal/trinetra/buildtag_test.go's TestDefaultBuildIsStdlibOnly,
// scoped to cmd/trinetra's own graph for exactly this reason).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/control"
)

const daemonStartWait = 30 * time.Second

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain parses the global flags, resolves and dials the control socket,
// and hands the resulting core.API to run (for a named subcommand) or
// runInteractive (for none, i.e. `trinetra-ctl` on its own -- see
// tui.go). It is split out from main so the os.Exit lives in exactly one
// place; the socket dialing here is what run's and the TUI model's tests
// replace with a fake core.API.
func realMain(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("trinetra-ctl", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socketFlag := fs.String("socket", "", "control socket path (overrides $TRINETRA_CONTROL_SOCKET (or $SERVERWATCH_CONTROL_SOCKET) and the default)")
	tokenFlag := fs.String("token", "", "control token file path (overrides $TRINETRA_CONTROL_TOKEN (or $SERVERWATCH_CONTROL_TOKEN) and the default)")
	// --json is accepted as a global flag here so it works BEFORE the subcommand (`--json
	// status`); flag.Parse stops at the first non-flag arg, so the after-subcommand form.
	jsonFlag := fs.Bool("json", false, "emit JSON for status/doctor/alerts")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	sock := resolveSocketPath(*socketFlag)
	var tokenErr error
	token := func() (string, error) {
		tok, err := resolveToken(*tokenFlag, sock)
		tokenErr = err
		return tok, err
	}
	client, err := control.DialWait(sock, token, daemonStartWait, func() {
		fmt.Fprintln(errOut, "trinetra-ctl: waiting for the trinetra daemon to start...")
	})
	if err != nil {
		if tokenErr != nil {
			fmt.Fprintf(errOut, "trinetra-ctl: reading control token: %v\n", err)
			return 1
		}
		fmt.Fprintf(errOut, "trinetra-ctl: dialing control socket %s: %v\n", sock, err)
		fmt.Fprintln(errOut, "is the daemon running? check: systemctl status trinetra; journalctl -u trinetra -n 50")
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
