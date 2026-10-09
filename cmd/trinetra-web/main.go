// Command trinetra-web serves the internal/web dashboard as a separate
// process from the trinetra daemon. It never reads daemon state
// in-process; instead it dials the daemon's control socket (internal/control)
// and uses the resulting *control.Client -- a core.API implementation -- as
// internal/web.Deps.API. The core daemon supervises this binary: when
// web.enabled is set, internal/trinetra/web_supervisor.go verifies and
// spawns it as a child process, restarts it with capped backoff if it exits,
// and stops it on daemon shutdown.
//
// No build tag: this is a plain, standalone binary built like any other
// command under ./cmd, `go build -o /usr/local/bin/trinetra-web
// ./cmd/trinetra-web`. The default `trinetra` binary stays
// stdlib-only (TestDefaultBuildIsStdlibOnly in internal/trinetra) simply
// because it does not import internal/web or this package, not because of a
// build tag.
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

// defaultRuntimeDir mirrors internal/trinetra/control_socket.go's
// defaultRuntimeDir: where the control socket + token live when systemd (or
// a supervisor, see Task 3) hasn't set RUNTIME_DIRECTORY. Duplicated here
// rather than imported so this binary doesn't need to pull in
// internal/trinetra (which is untagged but carries the whole daemon's
// dependency surface) just for two path constants.
const defaultRuntimeDir = "/run/trinetra"

// defaultStateDir mirrors internal/trinetra.StateDir (main.go), the
// daemon's default on-disk state directory. Same duplication rationale as
// defaultRuntimeDir above.
const defaultStateDir = "/var/lib/trinetra"

// configPath is where `users` reads web.* when the daemon isn't running.
var configPath = "/etc/trinetra/config.json"

const daemonStartWait = 30 * time.Second

// connConfig holds this binary's own settings, resolved from flags and
// environment (resolveConnConfig) before anything is dialed.
type connConfig struct {
	// socketPath is the control socket to dial (control.Dial's path arg).
	socketPath string
	// token is the control-socket auth token, either taken directly (flag
	// or TRINETRA_CONTROL_TOKEN/SERVERWATCH_CONTROL_TOKEN env, the form
	// Task 3's supervisor passes a spawned child) or read from tokenFile.
	token string
	// tokenFile is where to read the token from when it wasn't given
	// directly. Mirrors internal/trinetra/control_socket.go's
	// controlTokenPath: the sibling "token" file next to the socket,
	// written 0600 by the daemon that created the socket.
	tokenFile string
	// stateDir backs web.Deps.StateDir: the web plugin's OWN local storage
	// (its user store, sessions, enrollment tokens). It is not part of
	// core.API (it is this plugin's private auth material, not daemon state),
	// so it is resolved locally rather than over the socket. Alert data
	// (active alerts, history, acks) is NOT sourced from disk anymore: it all
	// goes through the socket client (core.API), so there are no alert-file
	// paths to resolve here.
	stateDir string
}

// resolveConnConfig parses args against a fresh FlagSet and layers in
// environment defaults: an explicit flag always wins, then the
// TRINETRA_CONTROL_SOCKET/TRINETRA_CONTROL_TOKEN env vars (the form Task 3's
// core supervisor launches this binary with), then (compat, for one release)
// the old SERVERWATCH_CONTROL_SOCKET/SERVERWATCH_CONTROL_TOKEN names, then
// finally the mirrored systemd/by-hand resolution (RUNTIME_DIRECTORY, or
// defaultRuntimeDir) that internal/trinetra/control_socket.go uses for
// the daemon side of the same socket.
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
	// Only read tokenFile when no token was given directly (flag or env):
	// a missing token file is fine in that case, it just means no-auth
	// (mirroring control_socket.go's own no-token fallback), so the error
	// is intentionally ignored here rather than propagated.
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

// adaptLiveEvent is THE ONLY place core.Event ever turns into a
// web.LiveEvent (see web.Deps.Subscribe's doc: internal/web never imports
// internal/core, so this adaptation has to happen here, in
// cmd/trinetra-web, rather than inside internal/web itself). It's a
// trivial field copy -- core.Event and web.LiveEvent share the same
// Kind/Severity/Source/Title/Time shape by design.
func adaptLiveEvent(ev core.Event) web.LiveEvent {
	return web.LiveEvent{
		Kind:     ev.Kind,
		Severity: ev.Severity,
		Source:   ev.Source,
		Title:    ev.Title,
		Time:     ev.Time,
	}
}

// bytesTrimNewline trims a single trailing newline (and any preceding
// carriage return) from a token file's contents -- writeTokenFile
// (internal/trinetra/control_socket.go) itself writes no trailing
// newline, but a token typed/echoed into a file by hand often picks one up.
func bytesTrimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// buildDeps assembles web.Deps around client, the control-socket core.API
// implementation client.Dial returns. api.Snapshot/api.Events back both
// Deps.API (what dashboardHandler/monitoringHandler/etc. read through, task
// 5's core.API boundary) and the still-closure-based Deps.Snapshot/Deps.Events
// (SSE, nav counts, the /public page, the alerts page's uptime tile -- see
// web.Deps' own doc) -- client already satisfies web.EventsStore directly,
// since core.DownEventView and web.DownEventView are the same type (a Go
// alias, events_store.go), so no adapter is needed there.
//
// cc's local, on-disk fields (StateDir/AlertLogPath/AlertStatePath) can't be
// sourced from the socket client -- they name paths on THIS machine that
// core.API has no method for -- so they come from cc (flags/defaults)
// instead; every other Deps field is backed by a client call.
func buildDeps(client *control.Client, cc connConfig) web.Deps {
	cfg := func() *config.Config {
		c, err := client.Config()
		if err != nil || c == nil {
			// A Config() failure must never surface as a nil Cfg(): every
			// handler in internal/web calls d.Cfg() unconditionally and
			// dereferences the result (see web.Deps.Cfg's doc), so this
			// must degrade to defaults rather than let a transient socket
			// error panic every request.
			return config.Default()
		}
		return c
	}

	deps := web.Deps{
		Cfg: cfg,
		// Reload backs the /public settings save path (see web.Deps.Reload's
		// doc: config/channels writes go through API.ApplyConfig directly
		// now, but Reload is still the /public page's own save path).
		// ApplyConfig is the same validate-persist-apply write client.Config
		// reads back, so wiring it here keeps that save path working too.
		Reload: client.ApplyConfig,
		API:    client,
		// Events: client.Events already has the exact signature
		// web.EventsStore wants (see this func's doc), so client is passed
		// directly rather than through an adapter.
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
		// ValidateChannel is now a core.API method (core-contract-s1 task
		// A1, #79): it dry-runs buildNotifier against the daemon's live
		// config over the socket, the same check TestChannel above already
		// crosses the socket for. The passed *config.Config is ignored --
		// client.ValidateChannel validates against the daemon's own current
		// config, not this process' copy (see core.API.ValidateChannel's
		// doc for that accepted live-config-vs-in-flight-edit limitation).
		ValidateChannel: func(cc config.ChannelConfig, _ *config.Config) error {
			return client.ValidateChannel(cc)
		},
		// Subscribe wires web.Deps' live-push seam (Task 3) to
		// client.Subscribe -- internal/control's dedicated-connection
		// streaming client (Task 2) -- adapting each core.Event it delivers
		// into a web.LiveEvent (adaptLiveEvent, above) as it goes. The
		// adapting goroutine exits (closing out) either when client's
		// channel closes (the daemon connection dropped) or ctx is done
		// (the SSE handler's request context, i.e. the browser
		// disconnected) -- whichever happens first -- so it never leaks
		// past the subscription it belongs to.
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
		// Fleet/NodeAPI (fleet-web-a task 1): client.Fleet is always
		// unrouted (Fleet.* calls run against the master regardless of node
		// scope, see internal/control's Client.Fleet doc); client.ForNode
		// returns a routed view that never fails locally -- an unknown id
		// only errors once a method is called through it, which is exactly
		// why internal/web's node router validates {node} against
		// Fleet().Nodes(...) itself before ever calling NodeAPI.
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

// runUsers serves `trinetra-web [flags] users …`. web.* comes from the daemon
// when it is reachable, else from config.json, so it works with the daemon
// stopped.
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
