package plugin

// Slot names a place in the UI a plugin can fill.
type Slot string

const (
	SlotSettingsForm     Slot = "settings.form"
	SlotNavPage          Slot = "nav.page"
	SlotDashboardPanel   Slot = "dashboard.panel"
	SlotContainerSection Slot = "container.section"
	SlotContainerAction  Slot = "container.action"
	SlotHostSection      Slot = "host.section"
	SlotAlertRules       Slot = "alert.rules"
)

// V1Slots lists every v1 slot.
var V1Slots = []Slot{SlotSettingsForm, SlotNavPage, SlotDashboardPanel, SlotContainerSection, SlotContainerAction, SlotHostSection, SlotAlertRules}

// HostRendered reports whether the host renders s from config_schema without calling the plugin.
func (s Slot) HostRendered() bool { return s == SlotSettingsForm || s == SlotAlertRules }

// WidgetKind names a widget type.
type WidgetKind string

const (
	WidgetTimeseries WidgetKind = "timeseries"
	WidgetStat       WidgetKind = "stat"
	WidgetTable      WidgetKind = "table"
	WidgetKV         WidgetKind = "kv"
	WidgetLog        WidgetKind = "log"
	WidgetForm       WidgetKind = "form"
	WidgetAction     WidgetKind = "action"
	WidgetText       WidgetKind = "text"
	WidgetStatus     WidgetKind = "status"
)

const (
	MaxWidgetsPerSlot = 20
	MaxTableRows      = 500
	MaxTableColumns   = 12
	MaxLogLines       = 1000
	MaxWidgetString   = 4096
	MaxSeriesPerChart = 12
)

type UserContext struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
}

type SlotContext struct {
	Node      string            `json:"node,omitempty"`
	Container string            `json:"container,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
}

type SlotRequest struct {
	Slot    Slot        `json:"slot"`
	SlotID  string      `json:"slot_id"`
	Context SlotContext `json:"context"`
	User    UserContext `json:"user"`
}

type RenderResult struct {
	Widgets []Widget `json:"widgets"`
}

type ActionRequest struct {
	Action    string            `json:"action"`
	Context   SlotContext       `json:"context"`
	User      UserContext       `json:"user"`
	Params    map[string]string `json:"params,omitempty"`
	Confirmed bool              `json:"confirmed,omitempty"`
}

type ActionResult struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message,omitempty"`
	Widgets []Widget `json:"widgets,omitempty"`
}

// Widget is one host-rendered UI element; exactly the field matching Kind is set.
type Widget struct {
	Kind       WidgetKind        `json:"kind"`
	Title      string            `json:"title,omitempty"`
	Timeseries *TimeseriesWidget `json:"timeseries,omitempty"`
	Stat       *StatWidget       `json:"stat,omitempty"`
	Table      *TableWidget      `json:"table,omitempty"`
	KV         *KVWidget         `json:"kv,omitempty"`
	Log        *LogWidget        `json:"log,omitempty"`
	Form       *FormWidget       `json:"form,omitempty"`
	Action     *ActionWidget     `json:"action,omitempty"`
	Text       *TextWidget       `json:"text,omitempty"`
	Status     *StatusWidget     `json:"status,omitempty"`
}

type SeriesRef struct {
	Metric string `json:"metric"`
	Label  string `json:"label,omitempty"`
}

type TimeseriesWidget struct {
	Series []SeriesRef `json:"series"`
	Range  string      `json:"range,omitempty"` // 1h | 6h | 24h | 7d (default 6h)
	Unit   string      `json:"unit,omitempty"`
}

type StatWidget struct {
	Value string `json:"value"`
	Unit  string `json:"unit,omitempty"`
	Hint  string `json:"hint,omitempty"`
	Tone  string `json:"tone,omitempty"` // "" | ok | warn | crit
}

type TableWidget struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type KVWidget struct {
	Items []KV `json:"items"`
}

type LogWidget struct {
	Lines []string `json:"lines"`
}

type FormField struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // string | integer | number | boolean | enum | secret
	Value    string   `json:"value,omitempty"`
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required,omitempty"`
}

type FormWidget struct {
	Action string      `json:"action"`
	Fields []FormField `json:"fields"`
	Submit string      `json:"submit,omitempty"`
}

type ActionWidget struct {
	Action string            `json:"action"`
	Params map[string]string `json:"params,omitempty"`
}

type TextWidget struct {
	Text   string `json:"text"`
	Format string `json:"format,omitempty"` // plain | lite
}

type StatusItem struct {
	Name   string `json:"name"`
	State  string `json:"state"` // up | down | unknown
	Detail string `json:"detail,omitempty"`
}

type StatusWidget struct {
	Items []StatusItem `json:"items"`
}
