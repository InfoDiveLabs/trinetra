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
