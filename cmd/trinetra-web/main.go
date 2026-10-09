// Command trinetra-web serves the internal/web dashboard as a separate process from the
// trinetra daemon.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/web"
)

// defaultRuntimeDir is where the control socket and token live when systemd or the
// supervisor has not set RUNTIME_DIRECTORY.
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
	// token is the control-socket auth token, given directly.
	token string
	// tokenFile is read when no token was given directly: the sibling "token" file
	// next to the socket, written 0600 by the daemon.
	tokenFile string
	// stateDir backs web.Deps.StateDir: this plugin's OWN storage (user store, sessions,
	// enrollment tokens).
	stateDir string
}

// resolveConnConfig parses args against a fresh FlagSet and layers environment defaults: an
// explicit flag wins, then TRINETRA_CONTROL_SOCKET/TOKEN (as the supervisor sets them).
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
	// Only read tokenFile when no token was given.
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

// buildDeps assembles web.Deps around client.
func buildDeps(client *control.Client, cc connConfig) web.Deps {
	cfg := lastGoodConfig(client.Config)

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
		// socket.
		ValidateChannel: func(cc config.ChannelConfig, _ *config.Config) error {
			return client.ValidateChannel(cc)
		},
		// Subscribe wires web.Deps' live-push seam to client.Subscribe, adapting each core.Event
		// into a web.LiveEvent.
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
		// Fleet/NodeAPI: client.Fleet is always unrouted (Fleet.* calls run against the master
		// regardless of node scope, see internal/control's Client.Fleet doc).
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

// lastGoodConfig returns the daemon's config, or the last one read
// successfully when a read fails, so a restarting daemon never makes the
// settings pages show (and save) built-in defaults. Each call gets a copy.
func lastGoodConfig(fetch func() (*config.Config, error)) func() *config.Config {
	var mu sync.Mutex
	var last []byte
	return func() *config.Config {
		c, err := fetch()
		mu.Lock()
		defer mu.Unlock()
		if err == nil && c != nil {
			if b, jerr := json.Marshal(c); jerr == nil {
				last = b
			}
			return c
		}
		if last != nil {
			var cp config.Config
			if json.Unmarshal(last, &cp) == nil {
				return &cp
			}
		}
		return config.Default()
	}
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
	client.SetTokenSource(token)

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
