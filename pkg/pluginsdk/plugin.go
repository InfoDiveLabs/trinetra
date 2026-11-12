package pluginsdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// Options configures a Plugin.
type Options struct {
	HostAPI string // default plugin.HostAPIString()
	Version string
}

// Plugin is one plugin process's connection to the Trinetra host.
type Plugin struct {
	opts Options

	mu       sync.Mutex
	getenv   func(string) string
	slots    map[string]func(ctx context.Context, req plugin.SlotRequest) ([]plugin.Widget, error)
	actions  map[string]func(ctx context.Context, req plugin.ActionRequest) (plugin.ActionResult, error)
	validate func(cfg Config) plugin.FieldErrors
	event    func(plugin.EventNotice)
	onConfig func(Config)
	hello    plugin.HelloResponse
	cfg      Config
	host     *hostConn
}

// New returns a Plugin.
func New(opts Options) *Plugin {
	if opts.HostAPI == "" {
		opts.HostAPI = plugin.HostAPIString()
	}
	return &Plugin{
		opts: opts, getenv: os.Getenv,
		slots:   map[string]func(context.Context, plugin.SlotRequest) ([]plugin.Widget, error){},
		actions: map[string]func(context.Context, plugin.ActionRequest) (plugin.ActionResult, error){},
	}
}

// SetEnv replaces the environment lookup Run uses; test harnesses use it to avoid process-wide env.
func (p *Plugin) SetEnv(f func(string) string) {
	p.mu.Lock()
	p.getenv = f
	p.mu.Unlock()
}

// HandleSlot registers the renderer for a declared slot id.
func (p *Plugin) HandleSlot(slotID string, f func(ctx context.Context, req plugin.SlotRequest) ([]plugin.Widget, error)) {
	p.mu.Lock()
	p.slots[slotID] = f
	p.mu.Unlock()
}

// HandleAction registers the handler for a declared action id.
func (p *Plugin) HandleAction(actionID string, f func(ctx context.Context, req plugin.ActionRequest) (plugin.ActionResult, error)) {
	p.mu.Lock()
	p.actions[actionID] = f
	p.mu.Unlock()
}

// HandleValidateConfig registers the validator the host consults before saving settings.
func (p *Plugin) HandleValidateConfig(f func(cfg Config) plugin.FieldErrors) {
	p.mu.Lock()
	p.validate = f
	p.mu.Unlock()
}

// HandleEvent registers the handler for host event notices.
func (p *Plugin) HandleEvent(f func(plugin.EventNotice)) {
	p.mu.Lock()
	p.event = f
	p.mu.Unlock()
}

// OnConfig registers a callback run with the initial config and after every change.
func (p *Plugin) OnConfig(f func(Config)) {
	p.mu.Lock()
	p.onConfig = f
	p.mu.Unlock()
}

// Hello returns the host's hello response (zero before Run has connected).
func (p *Plugin) Hello() plugin.HelloResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hello
}

// Config returns the current settings.
func (p *Plugin) Config() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

func (p *Plugin) setConfig(c plugin.ConfigResponse) {
	p.mu.Lock()
	p.cfg = Config{Revision: c.Revision, Values: c.Values}
	cb := p.onConfig
	cfg := p.cfg
	p.mu.Unlock()
	if cb != nil {
		cb(cfg)
	}
}

func (p *Plugin) hostConn() (*hostConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == nil {
		return nil, errors.New("pluginsdk: not connected to the host")
	}
	return p.host, nil
}

func (p *Plugin) hostCall(method string, params, result any) error {
	h, err := p.hostConn()
	if err != nil {
		return err
	}
	return h.call(method, params, result)
}

// Run reads the TRINETRA_PLUGIN_* environment, connects to the host, serves the core's calls and blocks until ctx ends.
func (p *Plugin) Run(ctx context.Context) error {
	p.mu.Lock()
	getenv := p.getenv
	p.mu.Unlock()
	hostSock, plugSock, tokenFile := getenv(plugin.EnvHostSocket), getenv(plugin.EnvPluginSocket), getenv(plugin.EnvTokenFile)
	if hostSock == "" || plugSock == "" || tokenFile == "" {
		return fmt.Errorf("pluginsdk: %s, %s and %s must be set", plugin.EnvHostSocket, plugin.EnvPluginSocket, plugin.EnvTokenFile)
	}
	tb, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("pluginsdk: read token: %w", err)
	}
	token := strings.TrimSpace(string(tb))

	_ = os.Remove(plugSock)
	ln, err := net.Listen("unix", plugSock)
	if err != nil {
		return fmt.Errorf("pluginsdk: listen: %w", err)
	}
	_ = os.Chmod(plugSock, 0o660)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		p.serve(ctx, ln, token)
	}()
	defer func() {
		cancel()
		<-served
		_ = os.Remove(plugSock)
	}()

	h := &hostConn{path: hostSock, token: token}
	defer h.closeAll()
	if err := p.connect(ctx, h); err != nil {
		return err
	}
	var cfg plugin.ConfigResponse
	if err := h.call(plugin.MethodGetConfig, struct{}{}, &cfg); err != nil {
		return fmt.Errorf("pluginsdk: get config: %w", err)
	}
	p.setConfig(cfg)

	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		backoff := 250 * time.Millisecond
		for ctx.Err() == nil {
			_ = watchConfig(ctx, hostSock, token, p.setConfig)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
		}
	}()
	<-ctx.Done()
	<-watchDone
	return nil
}

func (p *Plugin) connect(ctx context.Context, h *hostConn) error {
	deadline := time.Now().Add(30 * time.Second)
	delay := 100 * time.Millisecond
	for {
		var resp plugin.HelloResponse
		err := h.call(plugin.MethodHello, plugin.HelloRequest{HostAPI: p.opts.HostAPI, PluginVersion: p.opts.Version}, &resp)
		if err == nil {
			p.mu.Lock()
			p.hello, p.host = resp, h
			p.mu.Unlock()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pluginsdk: hello: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// Publish sends metrics for ts (0 = now); names are plugin-local.
func (p *Plugin) Publish(ts int64, metrics map[string]float64) (plugin.PublishResponse, error) {
	var out plugin.PublishResponse
	err := p.hostCall(plugin.MethodPublishSeries, plugin.PublishRequest{TS: ts, Metrics: metrics}, &out)
	return out, err
}

// Snapshot returns the host's live metrics view; it needs metrics.read.
func (p *Plugin) Snapshot() (core.DashboardView, error) {
	var out core.DashboardView
	err := p.hostCall(plugin.MethodSnapshot, struct{}{}, &out)
	return out, err
}

// FleetNodes returns the fleet roster (the node itself alone when not a master); it needs fleet.read.
func (p *Plugin) FleetNodes() ([]core.NodeSummary, error) {
	var out []core.NodeSummary
	err := p.hostCall(plugin.MethodFleetNodes, struct{}{}, &out)
	return out, err
}

// RaiseAlert raises (or updates) the alert key with severity info, warning or critical.
func (p *Plugin) RaiseAlert(key, severity, title, body string) error {
	return p.hostCall(plugin.MethodRaiseAlert, plugin.AlertRequest{Key: key, Severity: severity, Title: title, Body: body}, nil)
}

// ResolveAlert resolves the alert key.
func (p *Plugin) ResolveAlert(key, body string) error {
	return p.hostCall(plugin.MethodResolveAlert, plugin.ResolveRequest{Key: key, Body: body}, nil)
}

// ReportStatus reports health: state is ok, degraded or error.
func (p *Plugin) ReportStatus(state, message string) error {
	return p.hostCall(plugin.MethodReportStatus, plugin.StatusReport{State: state, Message: message}, nil)
}

// Logf sends a log line to the host (level debug, info, warn or error); it falls back to stderr when the host is unreachable.
func (p *Plugin) Logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if err := p.hostCall(plugin.MethodLog, plugin.LogEntry{Level: level, Message: msg}, nil); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", level, msg)
	}
}
