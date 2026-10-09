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

// defaultRuntimeDir is where the control socket and token live when systemd or
// the supervisor has not set RUNTIME_DIRECTORY. Duplicated from internal/trinetra
// so this binary does not pull in the whole daemon's dependency surface.
const defaultRuntimeDir = "/run/trinetra"

// defaultStateDir is the daemon's default state directory; duplicated for the
// same reason as defaultRuntimeDir.
const defaultStateDir = "/var/lib/trinetra"

const daemonStartWait = 30 * time.Second

// connConfig holds this binary's own settings, resolved from flags and
// environment (resolveConnConfig) before anything is dialed.
type connConfig struct {
	// socketPath is the control socket to dial (control.Dial's path arg).
	socketPath string
	// token is the control-socket auth token, given directly (flag or
	// TRINETRA_CONTROL_TOKEN/SERVERWATCH_CONTROL_TOKEN env, as the supervisor passes
	// it) or read from tokenFile.
	token string
	// tokenFile is read when no token was given directly: the sibling "token" file
	// next to the socket, written 0600 by the daemon.
	tokenFile string
	// stateDir backs web.Deps.StateDir: this plugin's OWN storage (user store,
	// sessions, enrollment tokens). It is private auth material, not daemon state, so
	// it is resolved locally rather than over the socket. Alert data all goes through
	// the socket client.
	stateDir string
}

// resolveConnConfig parses args against a fresh FlagSet and layers environment
// defaults: an explicit flag wins, then TRINETRA_CONTROL_SOCKET/TOKEN (as the
// supervisor sets them), then the old SERVERWATCH_CONTROL_SOCKET/TOKEN names
// (compat for one release), then the daemon's own RUNTIME_DIRECTORY /
// defaultRuntimeDir resolution.
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
	// Only read tokenFile when no token was given. A missing file just means no-auth
	// (as in the daemon), so the error is intentionally ignored.
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

// adaptLiveEvent is the only place core.Event becomes web.LiveEvent: internal/web
// never imports internal/core, so the field copy happens here.
func adaptLiveEvent(ev core.Event) web.LiveEvent {
	return web.LiveEvent{
		Kind:     ev.Kind,
		Severity: ev.Severity,
		Source:   ev.Source,
		Title:    ev.Title,
		Time:     ev.Time,
	}
}

// bytesTrimNewline trims one trailing newline (and preceding CR) from a token
// file; writeTokenFile writes none, but a hand-edited file often has one.
func bytesTrimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// buildDeps assembles web.Deps around client, the control-socket core.API.
// api.Snapshot/api.Events back both Deps.API and the closure-based
// Deps.Snapshot/Deps.Events (SSE, nav counts, /public, the alerts uptime tile).
// client satisfies web.EventsStore directly because core.DownEventView and
// web.DownEventView are the same alias type.
//
// cc's local fields (StateDir/AlertLogPath/AlertStatePath) name paths on THIS
// machine that core.API cannot supply, so they come from cc; every other Deps
// field is backed by a client call.
func buildDeps(client *control.Client, cc connConfig) web.Deps {
	cfg := func() *config.Config {
		c, err := client.Config()
		if err != nil || c == nil {
			// A Config() failure must not surface as a nil Cfg(): web handlers dereference it
			// unconditionally, so degrade to defaults rather than panic on a transient socket
			// error.
			return config.Default()
		}
		return c
	}

	deps := web.Deps{
		Cfg: cfg,
		// Reload is the /public page's save path; ApplyConfig is the same
		// validate-persist-apply write that client.Config reads back.
		Reload: client.ApplyConfig,
		API:    client,
		// client.Events already has web.EventsStore's signature, so it is passed directly.
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
		// ValidateChannel dry-runs buildNotifier against the daemon's live config over the
		// socket. The passed *config.Config is ignored: it validates against the daemon's
		// config, not this process' copy (see core.API.ValidateChannel).
		ValidateChannel: func(cc config.ChannelConfig, _ *config.Config) error {
			return client.ValidateChannel(cc)
		},
		// Subscribe wires web.Deps' live-push seam to client.Subscribe, adapting each
		// core.Event into a web.LiveEvent. The goroutine exits when client's channel
		// closes (daemon dropped) or ctx is done (browser disconnected), so it never
		// outlives the subscription.
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

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}
