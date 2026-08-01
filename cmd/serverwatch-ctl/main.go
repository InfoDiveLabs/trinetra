// Command serverwatch-ctl is a separate-process client for a running
// serverwatch daemon: it dials the daemon's control socket (internal/control)
// to obtain a core.API and drives it. This file wires the transport
// (resolve socket + token, control.Dial, defer Close) to run(), which holds
// the subcommand logic so it can be tested against a fake core.API without a
// real socket.
//
// Task 2 keeps this binary stdlib + internal/control + internal/core +
// internal/config only, with a few non-interactive passthrough subcommands
// (status/doctor/alerts) proving the socket path. The interactive Bubble Tea
// TUI, and the only third-party dependency this binary will ever carry,
// arrive in Task 3; nothing cmd/serverwatch reaches imports this package, so
// the default daemon build stays stdlib-only.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"serverwatch/internal/control"
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain parses the global flags, resolves and dials the control socket,
// and hands the resulting core.API to run. It is split out from main so the
// os.Exit lives in exactly one place; the socket dialing here is what run's
// test replaces with a fake core.API.
func realMain(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("serverwatch-ctl", flag.ContinueOnError)
	fs.SetOutput(errOut)
	socketFlag := fs.String("socket", "", "control socket path (overrides $SERVERWATCH_CONTROL_SOCKET and the default)")
	tokenFlag := fs.String("token", "", "control token file path (overrides $SERVERWATCH_CONTROL_TOKEN and the default)")
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

	return run(client, fs.Args(), out)
}
