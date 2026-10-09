// Command trinetra-web serves the internal/web dashboard as a separate process from the
// trinetra daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/web"
)

// defaultRuntimeDir mirrors internal/trinetra/control_socket.go's defaultRuntimeDir: where
// the control socket + token live when systemd.
const defaultRuntimeDir = "/run/trinetra"

// defaultStateDir mirrors internal/trinetra.StateDir (main.go), the daemon's default
// on-disk state directory.
const defaultStateDir = "/var/lib/trinetra"

// configPath is where `users` reads web.* when the daemon isn't running.
var configPath = "/etc/trinetra/config.json"

const daemonStartWait = 30 * time.Second

// connConfig holds this binary's own settings, resolved from flags and
// environment (resolveConnConfig) before anything is dialed.
type connConfig struct {
	// socketPath is the control socket to dial (control.Dial's path arg).
	socketPath string
	// token is the control-socket auth token, either taken directly.
	token string
	// tokenFile is where to read the token from when it wasn't given directly.
	tokenFile string
	// stateDir backs web.Deps.StateDir: the web plugin's OWN local storage (its user store,
	// sessions, enrollment tokens).
	stateDir string
}

// resolveConnConfig parses args against a fresh FlagSet and layers in environment defaults:
// an explicit flag always wins.
func resolveConnConfig(args []string, getenv func(string) string) (connConfig, error) {
	fs := flag.NewFlagSet("trinetra-web", flag.ContinueOnError)
	socket := fs.String("socket", "", "control socket path (default: $TRINETRA_CONTROL_SOCKET, else $SERVERWATCH_CONTROL_SOCKET, else $RUNTIME_DIRECTORY/control.sock, else /run/trinetra/control.sock)")
	token := fs.String("token", "", "control socket auth token (default: $TRINETRA_CONTROL_TOKEN, else $SERVERWATCH_CONTROL_TOKEN, else read from -token-file)")
	tokenFile := fs.String("token-file", "", "path to a file containing the control socket auth token (default: sibling \"token\" file next to the socket)")
	stateDir := fs.String("state-dir", "", "web UI state directory, e.g. sessions.json (default: /var/lib/trinetra)")
	if err := fs.Parse(args); err != nil {
		return connConfig{}, err
	}

	runtimeDir := getenv("RUNTIME_DIRECTORY")
	if runtimeDir == "" {
		runtimeDir = defaultRuntimeDir
	}

	cc := connConfig{
		socketPath: *socket,
		token:      *token,
		tokenFile:  *tokenFile,
		stateDir:   *stateDir,
	}
	if cc.socketPath == "" {
		cc.socketPath = getenv("TRINETRA_CONTROL_SOCKET")
	}
	if cc.socketPath == "" {
		cc.socketPath = getenv("SERVERWATCH_CONTROL_SOCKET")
	}
	if cc.socketPath == "" {
		cc.socketPath = filepath.Join(runtimeDir, "control.sock")
	}
	if cc.token == "" {
		cc.token = getenv("TRINETRA_CONTROL_TOKEN")
	}
	if cc.token == "" {
		cc.token = getenv("SERVERWATCH_CONTROL_TOKEN")
	}
	if cc.tokenFile == "" {
		cc.tokenFile = filepath.Join(runtimeDir, "token")
	}
	// Only read tokenFile when no token was given directly (flag or env): a missing token file
	// is fine in that case, it just means no-auth.
	if cc.token == "" {
		if b, err := os.ReadFile(cc.tokenFile); err == nil {
			cc.token = string(bytesTrimNewline(b))
		}
	}
	if cc.stateDir == "" {
		cc.stateDir = defaultStateDir
	}
	return cc, nil
}

// adaptLiveEvent is THE ONLY place core.Event ever turns into a web.LiveEvent.
func adaptLiveEvent(ev core.Event) web.LiveEvent {
	return web.LiveEvent{
		Kind:     ev.Kind,
		Severity: ev.Severity,
		Source:   ev.Source,
		Title:    ev.Title,
		Time:     ev.Time,
	}
}

// bytesTrimNewline trims a single trailing newline (and any preceding carriage return) from
// a token file's contents -- writeTokenFile.
func bytesTrimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// buildDeps assembles web.Deps around client, the control-socket core.API implementation
// client.Dial returns. api.Snapshot/api.Events back both Deps.API.
func buildDeps(client *control.Client, cc connConfig) web.Deps {
	cfg := func() *config.Config {
		c, err := client.Config()
		if err != nil || c == nil {
			// A Config() failure must never surface as a nil Cfg(): every handler in internal/web
			// calls d.Cfg() unconditionally and dereferences the result (see web.Deps.Cfg's doc).
			return config.Default()
		}
		return c
	}

	deps := web.Deps{
		Cfg: cfg,
		// Reload backs the /public settings save path.
		Reload: client.ApplyConfig,
		API:    client,
		// Events: client.Events already has the exact signature web.EventsStore wants (see this
		// func's doc), so client is passed directly rather than through an adapter.
		Events: client,
		Snapshot: func() web.DashboardView {
			v, err := client.Snapshot()
			if err != nil {
				return web.DashboardView{}
			}
			return v
		},
		StateDir:    cc.stateDir,
		TestChannel: client.TestChannel,
		// ValidateChannel is now a core.API method (core-contract-s1 task A1, #79): it dry-runs
		// buildNotifier against the daemon's live config over the socket.
		ValidateChannel: func(cc config.ChannelConfig, _ *config.Config) error {
			return client.ValidateChannel(cc)
		},
		// Subscribe wires web.Deps' live-push seam (Task 3) to client.Subscribe --
		// internal/control's dedicated-connection streaming client (Task 2).
		Subscribe: func(ctx context.Context) (<-chan web.LiveEvent, error) {
			ch, err := client.Subscribe(ctx)
			if err != nil {
				return nil, err
			}
			out := make(chan web.LiveEvent)
			go func() {
				defer close(out)
				for ev := range ch {
					select {
					case out <- adaptLiveEvent(ev):
					case <-ctx.Done():
						return
					}
				}
			}()
			return out, nil
		},
		// Fleet/NodeAPI (fleet-web-a task 1): client.Fleet is always unrouted (Fleet.* calls run
		// against the master regardless of node scope, see internal/control's Client.Fleet doc).
		Fleet:      client.Fleet,
		StatusPage: client.StatusPage,
		NodeAPI: func(id string) core.API {
			return client.ForNode(id)
		},
	}
	c := cfg()
	deps.Enabled = c.Web.Enabled
	deps.Listen = c.Web.Listen
	return deps
}

func run(args []string, getenv func(string) string, stderr *os.File) int {
	return runTo(args, getenv, os.Stdout, stderr)
}

func runTo(args []string, getenv func(string) string, stdout io.Writer, stderr *os.File) int {
	for i, a := range args {
		if a == "users" {
			return runUsers(args[:i], args[i+1:], getenv, stdout, stderr)
		}
	}
	cc, err := resolveConnConfig(args, getenv)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		fmt.Fprintln(stderr, "trinetra-web:", err)
		return 2
	}

	token := func() (string, error) {
		again, err := resolveConnConfig(args, getenv)
		return again.token, err
	}
	client, err := control.DialWait(cc.socketPath, token, daemonStartWait, func() {
		fmt.Fprintln(stderr, "trinetra-web: waiting for the trinetra daemon to start...")
	})
	if err != nil {
		fmt.Fprintf(stderr, "trinetra-web: dialing control socket %s: %v\n", cc.socketPath, err)
		return 1
	}
	defer client.Close()

	deps := buildDeps(client, cc)
	stop, err := web.Start(deps)
	if err != nil {
		fmt.Fprintln(stderr, "trinetra-web: starting web server:", err)
		return 1
	}
	defer stop()

	if !deps.Enabled {
		fmt.Fprintln(stderr, "trinetra-web: web.enabled is false in the daemon's config; nothing to serve, exiting")
		return 0
	}

	fmt.Fprintf(stderr, "trinetra-web: serving on %s (control socket %s)\n", deps.Listen, cc.socketPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	return 0
}

// runUsers serves `trinetra-web [flags] users …`. web.* comes from the daemon when it is
// reachable, else from config.json, so it works with the daemon stopped.
func runUsers(flags, args []string, getenv func(string) string, stdout io.Writer, stderr *os.File) int {
	cc, err := resolveConnConfig(flags, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "trinetra-web:", err)
		return 2
	}
	var cfg *config.Config
	if client, err := control.Dial(cc.socketPath, cc.token); err == nil {
		cfg, _ = client.Config()
		client.Close()
	}
	if cfg == nil {
		if c, err := config.Load(configPath); err == nil {
			cfg = c
		} else {
			cfg = config.Default()
		}
	}
	actor := "cli"
	if u := getenv("SUDO_USER"); u != "" {
		actor = "cli:" + u
	} else if u := getenv("USER"); u != "" {
		actor = "cli:" + u
	}
	return web.RunUsersCommand(args, cc.stateDir, cfg, actor, stdout, stderr)
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}
