package pluginhost

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const (
	pluginGroup  = "trinetra-plugin"
	hostSockName = "host.sock"
)

// ServiceDeps are the daemon facilities a Service is built from.
type ServiceDeps struct {
	StateDir    string
	Inventory   func() plugin.Inventory
	Core        core.API
	Fleet       func() core.FleetAPI
	NodeID      string
	CoreVersion string
	Submit      func(ts int64, ms map[string]float64) bool
	Deliver     func(AlertOut)
	Logf        func(format string, args ...any)
	Now         func() time.Time
}

// Service bundles the host pieces and implements plugin.HostRegistry.
type Service struct {
	Host      *Host
	UI        *UI
	Caller    *Caller
	Registrar *Registrar
	Alerts    *AlertBook
	Series    *SeriesGate
	Logs      *LogSink

	tokens *control.Tokens
	core   core.API
	mu     sync.Mutex
	ln     net.Listener
	sock   string
}

var _ plugin.HostRegistry = (*Service)(nil)

// NewService builds the host pieces; the alert book is loaded from StateDir/plugins/alerts.json.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	book, err := OpenAlertBook(filepath.Join(d.StateDir, "plugins", "alerts.json"), d.Deliver, d.Now)
	if err != nil {
		return nil, err
	}
	tokens := control.NewTokens()
	reg := NewRegistrar(tokens)
	series := NewSeriesGate(d.StateDir, d.Submit, d.Now)
	logs := NewLogSink(d.Logf, d.Now)
	caller := NewCaller(d.Inventory)
	host := NewHost(HostDeps{
		Inventory: d.Inventory, Registrar: reg, Series: series, Alerts: book, Logs: logs, Core: d.Core,
		Fleet: d.Fleet, NodeID: d.NodeID, CoreVersion: d.CoreVersion, Now: d.Now,
	})
	return &Service{Host: host, UI: NewUI(d.Inventory, caller, reg), Caller: caller, Registrar: reg, Alerts: book,
		Series: series, Logs: logs, tokens: tokens, core: d.Core}, nil
}

// Listen creates hostDir (0750) and serves hostDir/host.sock (0660), group-owned by trinetra-plugin when that group exists.
func (s *Service) Listen(hostDir string) error {
	if err := os.MkdirAll(hostDir, 0o750); err != nil {
		return err
	}
	if err := os.Chmod(hostDir, 0o750); err != nil {
		return err
	}
	gid := pluginGID()
	if gid >= 0 {
		_ = os.Chown(hostDir, -1, gid)
	}
	sock := filepath.Join(hostDir, hostSockName)
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o660); err != nil {
		ln.Close()
		return err
	}
	if gid >= 0 {
		_ = os.Chown(sock, -1, gid)
	}
	s.mu.Lock()
	s.ln, s.sock = ln, sock
	s.mu.Unlock()
	go func() { _ = control.ServeAuth(s.core, s.Host, ln, s.tokens) }()
	return nil
}

func pluginGID() int {
	g, err := user.LookupGroup(pluginGroup)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(g.Gid)
	if err != nil {
		return -1
	}
	return n
}

// Close stops the listener and removes the socket.
func (s *Service) Close() error {
	s.mu.Lock()
	ln, sock := s.ln, s.sock
	s.ln = nil
	s.mu.Unlock()
	if ln == nil {
		return nil
	}
	err := ln.Close()
	_ = os.Remove(sock)
	return err
}

// Register admits id to the host socket and returns its token.
func (s *Service) Register(id string, granted []plugin.Permission, dev bool) (string, error) {
	return s.Registrar.Register(id, granted, dev)
}

// Unregister revokes id's token and ends its streams and queued events.
func (s *Service) Unregister(id string) {
	s.Registrar.Unregister(id)
	s.Host.Disconnect(id)
	s.Caller.Stop(id)
}

// NotifyConfig pushes cfg to id's config streams and tells it a change happened.
func (s *Service) NotifyConfig(id string, cfg plugin.ConfigResponse) {
	s.Host.Notify(id, cfg)
	s.Caller.Event(id, plugin.EventNotice{Kind: plugin.EventConfigChanged, Time: time.Now().Unix()})
}

// ValidateConfig asks the running plugin to validate values.
func (s *Service) ValidateConfig(id string, values map[string]json.RawMessage) (plugin.FieldErrors, error) {
	return s.Caller.ValidateConfig(context.Background(), id, values)
}

// Health summarizes id's host-side observations.
func (s *Service) Health(id string) plugin.Health {
	st, at := s.Logs.Status(id)
	return plugin.Health{
		Status: st, ReportedAt: at, Connected: s.Host.Connected(id), SeriesCount: s.Series.Count(id),
		SeriesRejected: s.Series.Rejected(id), LogsDropped: s.Logs.Dropped(id), EventsDropped: s.Caller.EventsDropped(id),
		RecentLogs: s.Logs.Recent(id),
	}
}

// Forget drops id's series cap state; the runtime calls it when a plugin is removed.
func (s *Service) Forget(id string) { s.Series.Forget(id) }

// ResolveAll resolves every active alert of id.
func (s *Service) ResolveAll(id, reason string) { s.Alerts.ResolveAll(id, reason) }

// HostSocket returns the listening socket path, or "" before Listen.
func (s *Service) HostSocket() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sock
}
