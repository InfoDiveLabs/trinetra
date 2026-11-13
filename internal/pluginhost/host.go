package pluginhost

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const maxContainerLogLines = 2000

// HostDeps are the host's collaborators.
type HostDeps struct {
	Inventory   func() plugin.Inventory
	Registrar   *Registrar
	Series      *SeriesGate
	Alerts      *AlertBook
	Logs        *LogSink
	Core        core.API
	Fleet       func() core.FleetAPI
	NodeID      string
	CoreVersion string
	Now         func() time.Time
}

type watcher struct{ ch chan plugin.ConfigResponse }

// Host serves Plugin.* for authenticated plugins.
type Host struct {
	d         HostDeps
	mu        sync.Mutex
	watchers  map[string]map[*watcher]struct{}
	connected map[string]bool
}

var _ plugin.Host = (*Host)(nil)

// NewHost returns a Host over d.
func NewHost(d HostDeps) *Host {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Host{d: d, watchers: map[string]map[*watcher]struct{}{}, connected: map[string]bool{}}
}

func (h *Host) inventory() plugin.Inventory {
	if h.d.Inventory == nil {
		return nil
	}
	return h.d.Inventory()
}

func (h *Host) schema(id string) *plugin.ConfigSchema {
	if inv := h.inventory(); inv != nil {
		if p, ok := inv.Get(id); ok {
			return p.Manifest.ConfigSchema
		}
	}
	return nil
}

func (h *Host) need(id string, perms ...plugin.Permission) error {
	granted, _, ok := h.d.Registrar.Granted(id)
	if !ok {
		return fmt.Errorf("%w: %s is not registered", plugin.ErrPermissionDenied, id)
	}
	if missing := plugin.Missing(granted, perms); len(missing) > 0 {
		return fmt.Errorf("%w: %s lacks %s", plugin.ErrPermissionDenied, id, missing[0])
	}
	return nil
}

// Hello admits id when its host_api range is served and it is enabled (or a dev session), and marks it connected.
func (h *Host) Hello(id string, req plugin.HelloRequest) (plugin.HelloResponse, error) {
	if err := plugin.HostAPICompatible("^" + strings.TrimPrefix(req.HostAPI, "^")); err != nil {
		return plugin.HelloResponse{}, err
	}
	granted, dev, ok := h.d.Registrar.Granted(id)
	if !ok {
		return plugin.HelloResponse{}, fmt.Errorf("%w: %s is not registered", plugin.ErrPermissionDenied, id)
	}
	if !dev {
		inv := h.inventory()
		if inv == nil {
			return plugin.HelloResponse{}, plugin.ErrUnavailable
		}
		p, found := inv.Get(id)
		if !found || p.State != plugin.StateEnabled {
			return plugin.HelloResponse{}, fmt.Errorf("%w: %s is not enabled", plugin.ErrPermissionDenied, id)
		}
	}
	h.mu.Lock()
	h.connected[id] = true
	h.mu.Unlock()
	return plugin.HelloResponse{
		HostAPI: plugin.HostAPIString(), PluginID: id, Granted: granted, NodeID: h.d.NodeID,
		TrinetraVersion: h.d.CoreVersion, Dev: dev,
	}, nil
}

// Connected reports whether id has said hello and its config stream has not since closed.
func (h *Host) Connected(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected[id]
}

// Disconnect marks id disconnected and closes its config streams.
func (h *Host) Disconnect(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.connected, id)
	for w := range h.watchers[id] {
		close(w.ch)
	}
	delete(h.watchers, id)
}

// GetConfig returns id's stored config with schema defaults applied.
func (h *Host) GetConfig(id string) (plugin.ConfigResponse, error) {
	if err := h.need(id); err != nil {
		return plugin.ConfigResponse{}, err
	}
	inv := h.inventory()
	if inv == nil {
		return plugin.ConfigResponse{}, plugin.ErrUnavailable
	}
	cfg, err := inv.ConfigValues(id)
	if err != nil {
		return plugin.ConfigResponse{}, err
	}
	cfg.Values = plugin.WithDefaults(h.schema(id), cfg.Values)
	return cfg, nil
}

// WatchConfig streams config updates to id; the stream is latest-wins.
func (h *Host) WatchConfig(ctx context.Context, id string) (<-chan plugin.ConfigResponse, error) {
	if err := h.need(id); err != nil {
		return nil, err
	}
	w := &watcher{ch: make(chan plugin.ConfigResponse, 1)}
	h.mu.Lock()
	if h.watchers[id] == nil {
		h.watchers[id] = map[*watcher]struct{}{}
	}
	h.watchers[id][w] = struct{}{}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.watchers[id][w]; !ok {
			return
		}
		delete(h.watchers[id], w)
		close(w.ch)
		if len(h.watchers[id]) == 0 {
			delete(h.watchers, id)
			delete(h.connected, id)
		}
	}()
	return w.ch, nil
}

// Notify pushes cfg to every WatchConfig stream of id (latest-wins, buffer 1).
func (h *Host) Notify(id string, cfg plugin.ConfigResponse) {
	cfg.Values = plugin.WithDefaults(h.schema(id), cfg.Values)
	h.mu.Lock()
	defer h.mu.Unlock()
	for w := range h.watchers[id] {
		select {
		case w.ch <- cfg:
		default:
			select {
			case <-w.ch:
			default:
			}
			select {
			case w.ch <- cfg:
			default:
			}
		}
	}
}

// ReportStatus records id's self-reported health.
func (h *Host) ReportStatus(id string, r plugin.StatusReport) error {
	if err := h.need(id); err != nil {
		return err
	}
	return h.d.Logs.SetStatus(id, r, h.d.Now().Unix())
}

// Log records one log line from id.
func (h *Host) Log(id string, e plugin.LogEntry) error {
	if err := h.need(id); err != nil {
		return err
	}
	return h.d.Logs.Log(id, e)
}

// PublishSeries validates and submits id's metrics.
func (h *Host) PublishSeries(id string, r plugin.PublishRequest) (plugin.PublishResponse, error) {
	if err := h.need(id, plugin.PermMetricsPublish); err != nil {
		return plugin.PublishResponse{}, err
	}
	return h.d.Series.Publish(id, r), nil
}

// RaiseAlert raises one of id's alerts.
func (h *Host) RaiseAlert(id string, r plugin.AlertRequest) error {
	if err := h.need(id, plugin.PermAlertsRaise); err != nil {
		return err
	}
	return h.d.Alerts.Raise(id, r)
}

// ResolveAlert resolves one of id's alerts.
func (h *Host) ResolveAlert(id string, r plugin.ResolveRequest) error {
	if err := h.need(id, plugin.PermAlertsRaise); err != nil {
		return err
	}
	return h.d.Alerts.Resolve(id, r)
}

// Snapshot returns the dashboard view, without container data unless id also holds docker.read.
func (h *Host) Snapshot(id string) (core.DashboardView, error) {
	if err := h.need(id, plugin.PermMetricsRead); err != nil {
		return core.DashboardView{}, err
	}
	v, err := h.d.Core.Snapshot()
	if err != nil {
		return core.DashboardView{}, err
	}
	if h.need(id, plugin.PermDockerRead) != nil {
		v.TopCPUContainers, v.TopMemContainers = nil, nil
		v.ContainersRunning, v.ContainersTotal = 0, 0
	}
	return v, nil
}

// Series reads a metric history; other plugins' namespaces are refused.
func (h *Host) Series(id string, r plugin.SeriesRequest) ([]core.SeriesPoint, error) {
	if err := h.need(id, plugin.PermMetricsRead); err != nil {
		return nil, err
	}
	if strings.HasPrefix(r.Metric, "plugin.") && !strings.HasPrefix(r.Metric, "plugin."+id+".") {
		return nil, fmt.Errorf("%w: %s may not read another plugin's series", plugin.ErrPermissionDenied, id)
	}
	res := core.Resolution(r.Res)
	if res < core.ResRaw || res > core.ResAuto {
		res = core.ResAuto
	}
	return h.d.Core.Series(r.Metric, r.From, r.To, res)
}

// ActiveAlerts lists the active alerts.
func (h *Host) ActiveAlerts(id string) ([]core.AlertRecord, error) {
	if err := h.need(id, plugin.PermAlertsRead); err != nil {
		return nil, err
	}
	return h.d.Core.ActiveAlerts()
}

// Containers lists containers with their live stats.
func (h *Host) Containers(id string) ([]core.ContainerView, error) {
	if err := h.need(id, plugin.PermDockerRead); err != nil {
		return nil, err
	}
	m, err := h.d.Core.Monitoring()
	if err != nil {
		return nil, err
	}
	out := make([]core.ContainerView, 0, len(m.Containers))
	for _, c := range m.Containers {
		out = append(out, core.ContainerView{Name: c.Name, State: c.State, CPUPct: c.CPUPct, MemMiB: c.MemMiB})
	}
	return out, nil
}

// ContainerLogs returns up to 2000 recent log lines of a container.
func (h *Host) ContainerLogs(id string, r plugin.ContainerLogsRequest) (string, error) {
	if err := h.need(id, plugin.PermDockerRead, plugin.PermLogsRead); err != nil {
		return "", err
	}
	n := r.Lines
	if n <= 0 {
		n = 100
	}
	if n > maxContainerLogLines {
		n = maxContainerLogLines
	}
	return h.d.Core.ContainerLogs(r.Name, n)
}

// FleetNodes lists fleet nodes on a master.
func (h *Host) FleetNodes(id string) ([]core.NodeSummary, error) {
	if err := h.need(id, plugin.PermFleetRead); err != nil {
		return nil, err
	}
	if h.d.Fleet == nil {
		return nil, core.ErrNotMaster
	}
	f := h.d.Fleet()
	if f == nil {
		return nil, core.ErrNotMaster
	}
	return f.Nodes(core.NodeFilter{})
}
