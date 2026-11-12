package plugin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// plugin -> core methods, served on the plugin host socket.
const (
	MethodHello         = "Plugin.Hello"
	MethodGetConfig     = "Plugin.GetConfig"
	MethodWatchConfig   = "Plugin.WatchConfig"
	MethodReportStatus  = "Plugin.ReportStatus"
	MethodLog           = "Plugin.Log"
	MethodPublishSeries = "Plugin.PublishSeries"
	MethodRaiseAlert    = "Plugin.RaiseAlert"
	MethodResolveAlert  = "Plugin.ResolveAlert"
	MethodSnapshot      = "Plugin.Snapshot"
	MethodSeries        = "Plugin.Series"
	MethodActiveAlerts  = "Plugin.ActiveAlerts"
	MethodContainers    = "Plugin.Containers"
	MethodContainerLogs = "Plugin.ContainerLogs"
	MethodFleetNodes    = "Plugin.FleetNodes"
)

// MethodPermissions maps every Plugin.* method to the permissions it requires (all must be granted).
var MethodPermissions = map[string][]Permission{
	MethodHello: nil, MethodGetConfig: nil, MethodWatchConfig: nil, MethodReportStatus: nil, MethodLog: nil,
	MethodPublishSeries: {PermMetricsPublish},
	MethodRaiseAlert:    {PermAlertsRaise},
	MethodResolveAlert:  {PermAlertsRaise},
	MethodSnapshot:      {PermMetricsRead},
	MethodSeries:        {PermMetricsRead},
	MethodActiveAlerts:  {PermAlertsRead},
	MethodContainers:    {PermDockerRead},
	MethodContainerLogs: {PermDockerRead, PermLogsRead},
	MethodFleetNodes:    {PermFleetRead},
}

// AllowedMethods returns the Plugin.* methods whose required permissions are all in granted.
func AllowedMethods(granted []Permission) map[string]bool {
	have := map[Permission]bool{}
	for _, p := range granted {
		have[p] = true
	}
	out := map[string]bool{}
	for m, need := range MethodPermissions {
		ok := true
		for _, p := range need {
			ok = ok && have[p]
		}
		if ok {
			out[m] = true
		}
	}
	return out
}

// core -> plugin calls, served by the plugin on $TRINETRA_PLUGIN_SOCKET.
const (
	CallRenderSlot     = "RenderSlot"
	CallInvokeAction   = "InvokeAction"
	CallValidateConfig = "ValidateConfig"
	CallOnEvent        = "OnEvent"
)

const (
	RenderSlotTimeout        = 2 * time.Second
	InvokeActionTimeout      = 10 * time.Second
	ValidateConfigTimeout    = 2 * time.Second
	OnEventTimeout           = 1 * time.Second
	MaxInflightCalls         = 4
	EventQueueSize           = 64
	DefaultSeriesCap         = 200
	FleetPluginSeriesBudget  = 1000
	MaxPublishPerCall        = 1000
	MaxActiveAlertsPerPlugin = 200
	LogRatePerSec            = 10
	LogBurst                 = 50
	RecentLogLines           = 50
)

type HelloRequest struct {
	HostAPI       string `json:"host_api"`
	PluginVersion string `json:"plugin_version"`
}

type HelloResponse struct {
	HostAPI         string       `json:"host_api"`
	PluginID        string       `json:"plugin_id"`
	Granted         []Permission `json:"granted"`
	NodeID          string       `json:"node_id"`
	TrinetraVersion string       `json:"trinetra_version"`
	Dev             bool         `json:"dev,omitempty"`
}

type ConfigResponse struct {
	Revision int64                      `json:"revision"`
	Values   map[string]json.RawMessage `json:"values"`
}

type StatusReport struct {
	State   string `json:"state"` // ok | degraded | error
	Message string `json:"message,omitempty"`
}

type LogEntry struct {
	Time    int64  `json:"time,omitempty"`
	Level   string `json:"level"` // debug | info | warn | error
	Message string `json:"message"`
}

type PublishRequest struct {
	TS      int64              `json:"ts"`
	Metrics map[string]float64 `json:"metrics"`
}

type PublishResponse struct {
	Accepted int      `json:"accepted"`
	Rejected []string `json:"rejected,omitempty"`
	Dropped  bool     `json:"dropped,omitempty"`
}

type AlertRequest struct {
	Key      string `json:"key"`
	Severity string `json:"severity"` // info | warning | critical
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
}

type ResolveRequest struct {
	Key  string `json:"key"`
	Body string `json:"body,omitempty"`
}

type SeriesRequest struct {
	Metric string `json:"metric"`
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	Res    int    `json:"res"`
}

type ContainerLogsRequest struct {
	Name  string `json:"name"`
	Lines int    `json:"lines"`
}

// Host serves Plugin.* for the authenticated plugin id; the control layer has already applied the method allowlist.
type Host interface {
	Hello(id string, req HelloRequest) (HelloResponse, error)
	GetConfig(id string) (ConfigResponse, error)
	WatchConfig(ctx context.Context, id string) (<-chan ConfigResponse, error)
	ReportStatus(id string, r StatusReport) error
	Log(id string, e LogEntry) error
	PublishSeries(id string, r PublishRequest) (PublishResponse, error)
	RaiseAlert(id string, r AlertRequest) error
	ResolveAlert(id string, r ResolveRequest) error
	Snapshot(id string) (core.DashboardView, error)
	Series(id string, r SeriesRequest) ([]core.SeriesPoint, error)
	ActiveAlerts(id string) ([]core.AlertRecord, error)
	Containers(id string) ([]core.ContainerView, error)
	ContainerLogs(id string, r ContainerLogsRequest) (string, error)
	FleetNodes(id string) ([]core.NodeSummary, error)
}

type ValidateConfigRequest struct {
	Values map[string]json.RawMessage `json:"values"`
}

type ValidateConfigResult struct {
	Errors map[string]string `json:"errors,omitempty"`
}

type EventNotice struct {
	Kind    string          `json:"kind"`
	Time    int64           `json:"time"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

const (
	EventConfigChanged  = "config.changed"
	EventAlertFired     = "alert.fire"
	EventAlertRecovered = "alert.recover"
	EventShutdown       = "shutdown"
)
