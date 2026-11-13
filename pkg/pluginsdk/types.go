package pluginsdk

import (
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// Wire types re-exported so plugins built outside this module never import internal packages.
type (
	SlotRequest      = plugin.SlotRequest
	SlotContext      = plugin.SlotContext
	UserContext      = plugin.UserContext
	RenderResult     = plugin.RenderResult
	ActionRequest    = plugin.ActionRequest
	ActionResult     = plugin.ActionResult
	FieldErrors      = plugin.FieldErrors
	EventNotice      = plugin.EventNotice
	HelloResponse    = plugin.HelloResponse
	PublishResponse  = plugin.PublishResponse
	Permission       = plugin.Permission
	Role             = plugin.Role
	Widget           = plugin.Widget
	WidgetKind       = plugin.WidgetKind
	SeriesRef        = plugin.SeriesRef
	KVItem           = plugin.KV
	StatusItem       = plugin.StatusItem
	FormField        = plugin.FormField
	TimeseriesWidget = plugin.TimeseriesWidget
	StatWidget       = plugin.StatWidget
	TableWidget      = plugin.TableWidget
	KVWidget         = plugin.KVWidget
	LogWidget        = plugin.LogWidget
	FormWidget       = plugin.FormWidget
	ActionWidget     = plugin.ActionWidget
	TextWidget       = plugin.TextWidget
	StatusWidget     = plugin.StatusWidget
	DashboardView    = core.DashboardView
	ProcessCounts    = core.ProcessCounts
	ContainerView    = core.ContainerView
	DiskView         = core.DiskView
	NetIfaceView     = core.NetIfaceView
	NodeSummary      = core.NodeSummary
	SeriesPoint      = core.SeriesPoint
	AlertRecord      = core.AlertRecord
)

const (
	PermMetricsRead    = plugin.PermMetricsRead
	PermMetricsPublish = plugin.PermMetricsPublish
	PermAlertsRead     = plugin.PermAlertsRead
	PermAlertsRaise    = plugin.PermAlertsRaise
	PermDockerRead     = plugin.PermDockerRead
	PermLogsRead       = plugin.PermLogsRead
	PermFleetRead      = plugin.PermFleetRead
	PermNetOutbound    = plugin.PermNetOutbound
	PermNetListen      = plugin.PermNetListen

	RoleViewer    = plugin.RoleViewer
	RoleResponder = plugin.RoleResponder
	RoleAdmin     = plugin.RoleAdmin

	WidgetTimeseries = plugin.WidgetTimeseries
	WidgetStat       = plugin.WidgetStat
	WidgetTable      = plugin.WidgetTable
	WidgetKV         = plugin.WidgetKV
	WidgetLog        = plugin.WidgetLog
	WidgetForm       = plugin.WidgetForm
	WidgetAction     = plugin.WidgetAction
	WidgetText       = plugin.WidgetText
	WidgetStatus     = plugin.WidgetStatus

	MaxWidgetsPerSlot = plugin.MaxWidgetsPerSlot
	MaxTableRows      = plugin.MaxTableRows
	MaxTableColumns   = plugin.MaxTableColumns
	MaxLogLines       = plugin.MaxLogLines
	MaxWidgetString   = plugin.MaxWidgetString
	MaxSeriesPerChart = plugin.MaxSeriesPerChart
)
