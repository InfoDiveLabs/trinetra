// Bubble Tea glue for the management menu ('m' off Home) and its four
// config-backed screens (schedule, quiet hours, healthchecks, monitor
// thresholds). The pure config mutations these screens apply live in
// manage_schedule.go/manage_quiet.go/manage_health.go/manage_monitor.go
// (unit-tested there against a fake core.API/plain *config.Config, no
// terminal involved); this file is deliberately thin, mirroring tui.go's
// own split for the web-setup wizard (setup_web.go vs. tui.go).
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// manageScreen is the management menu's own sub-step, mirroring
// webSetupStep's role for the web-setup wizard: a sub-state of the top
// level tui step (stepManage) so navigating within the menu doesn't need a
// top level step of its own for every screen.
type manageScreen int

const (
	manageMenuList manageScreen = iota
	manageScheduleMode
	manageScheduleValue
	manageQuietValue
	manageHealthValue
	manageMonitorList
	manageMonitorThreshold
	manageChannelsList
	manageChannelsName
	manageChannelsType
	manageChannelsEnabled
	manageChannelsField
	manageSettingsGroups
	manageSettingsKeys
	manageSettingsValue
	manageResult
)

// manageItems are the management menu's rows, in the order shown; their
// index is what updateManageMenuKey's "enter" case switches on. "all
// settings" (issue #91) is the generic browse/edit screen over every flat
// config key (config.Keys()), the catch-all that makes "no `config set`
// needed" true even for keys none of the dedicated screens above cover
// (sampling intervals, baseline/anomaly tuning, global thresholds,
// critical_overrides_quiet, storage.*, collection toggles, the remaining
// web.* keys, and public.*).
var manageItems = []string{"schedule", "quiet hours", "healthchecks", "monitor thresholds", "channels", "all settings"}

// scheduleModeChoices are the Schedule screen's mode-selection rows, index-
// matched against scheduleMode's off/daily/weekly constants (manage_schedule.go).
var scheduleModeChoices = []string{"off", "daily", "weekly"}

// manageModel holds every management screen's state. Only one screen is
// ever shown at a time (mgr.screen), but they all live on the same struct
// (embedded on model as m.mgr) so switching between them, or back to the
// menu, never has to reconstruct or re-fetch anything the previous screen
// already had -- the same reasoning tui.go's model gives for keeping the
// web-setup wizard's fields alongside the home screen's.
type manageModel struct {
	screen manageScreen
	cursor int // menu cursor

	// schedule
	schedModeCursor int
	schedAns        scheduleAnswers

	// shared single-line input for the schedule value / quiet hours /
	// healthchecks screens (only one of those three is ever active at once)
	valueIn textinput.Model

	// monitor thresholds
	monTargets          []core.TargetView
	monRows             []monitorTargetRow
	monCursor           int
	monThreshIn         textinput.Model
	monEditingThreshold bool
	monLoading          bool
	monErr              error

	// configLoading/configErr cover the schedule/quiet-hours/healthchecks
	// screens' pre-fill fetch (fetchScheduleConfigCmd/fetchQuietConfigCmd/
	// fetchHealthConfigCmd): true/set from the moment the menu opens one of
	// them until its *ConfigMsg lands, so a screen never shows (or lets the
	// user blindly commit) a blank/default value while the CURRENT one is
	// still in flight -- see the doc on updateScheduleModeKey/
	// updateManageValueKey's configLoading guard for why this matters (a
	// stray Enter must never wipe an existing setting).
	configLoading bool
	configErr     error

	applying bool
	applyErr error

	// channels -- state lives here (not a separate top level model) for the
	// same reason the schedule/quiet-hours/healthchecks/monitor fields do:
	// see manageModel's own doc.
	chanList     []config.ChannelConfig // sorted (sortedChannels), fetched fresh on screen open and after every add/edit/remove
	chanCursor   int
	chanErr      error  // surfaced on the list: a failed fetch, remove, or test
	chanTestMsg  string // last successful test's status line, cleared on the next action
	chanNameIn   textinput.Model
	chanTypeCur  int
	chanFieldIn  textinput.Model
	chanFieldIdx int
	chanAns      channelAnswers
	chanEditName string // "" for add; the existing channel's name for edit
	chanIsEdit   bool
	chanSaving   bool

	// settings -- the generic "all settings" screen (issue #91). Uses its
	// own loading/error/cfg fields rather than configLoading/valueIn (the
	// schedule/quiet-hours/healthchecks screens' shared fields) since this
	// screen has its own group-list -> key-list -> value-input shape and no
	// state to share with those screens.
	setLoading  bool
	setErr      error
	setCfg      *config.Config // freshly fetched on open, source of each key's CURRENT value
	setGroups   []string       // settingsGroups(), fetched once per screen visit
	setGroupCur int
	setKeys     []config.KeyInfo // settingsGroupKeys(selected group)
	setKeyCur   int
	setValueIn  textinput.Model
	setKey      string // the key mgr.setValueIn/manageResult's caveat refer to
	setRestart  bool   // setKey's RestartRequired, for the value/result screens' caveat line
}

// newManageValueInput builds a text input the same way newModel's wizard
// inputs are built (tui.go), just as a free function since manage screens
// construct one fresh per visit rather than keeping four permanently
// allocated fields the way the web wizard does.
func newManageValueInput(placeholder string) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.CharLimit = 256
	ti.Width = 40
	return ti
}

// --- messages ---

// manageAppliedMsg carries the result of applying the schedule/quiet-hours/
// healthchecks screens' single value (fetch Config, apply the pure mutator,
// ApplyConfig) back into Update.
type manageAppliedMsg struct {
	err error
}

// scheduleConfigMsg carries a freshly fetched Config back into Update for
// the Schedule screen's pre-fill (#review-fix-1): the CURRENT
// schedule.daily/schedule.weekly values, so opening the screen defaults the
// mode cursor to whatever is actually active and pre-fills its value,
// instead of always defaulting to "off" with a blank value (which made a
// stray Enter silently wipe an existing schedule).
type scheduleConfigMsg struct {
	cfg *config.Config
	err error
}

// quietHoursConfigMsg is scheduleConfigMsg's counterpart for the Quiet
// hours screen's pre-fill.
type quietHoursConfigMsg struct {
	cfg *config.Config
	err error
}

// healthchecksConfigMsg is scheduleConfigMsg's counterpart for the
// Healthchecks screen's pre-fill.
type healthchecksConfigMsg struct {
	cfg *config.Config
	err error
}

// monitorTargetsMsg carries api.MonitorTargets()'s result plus a freshly
// fetched Config back into Update, so the Monitor thresholds screen can
// merge them into display rows (buildMonitorRows, manage_monitor.go).
type monitorTargetsMsg struct {
	targets []core.TargetView
	cfg     *config.Config
	err     error
}

// monitorAppliedMsg carries the result of a single enable/disable or
// threshold edit (fetch Config, mutate, ApplyConfig) back into Update; cfg
// is the just-applied config (nil on error), used to rebuild monRows so the
// screen reflects what was actually saved rather than an optimistic local
// guess.
type monitorAppliedMsg struct {
	cfg *config.Config
	err error
}

// channelsConfigMsg carries a freshly fetched Config back into Update for
// the Channels screen's list (fetchChannelsConfigCmd), mirroring
// scheduleConfigMsg/quietHoursConfigMsg/healthchecksConfigMsg's pre-fill
// role for their own screens (see manageModel.configLoading's doc for why
// this fetch-on-open matters).
type channelsConfigMsg struct {
	cfg *config.Config
	err error
}

// channelActionMsg carries the result of an in-place list action (remove or
// test, as opposed to the add/edit flow's channelSavedMsg) back into
// Update. cfg is the freshly re-fetched, already-applied config after a
// remove (nil for a test, which never changes config); name/isTest
// identify what happened for the list's transient status line.
type channelActionMsg struct {
	cfg    *config.Config
	name   string
	isTest bool
	err    error
}

// channelSavedMsg carries the result of saveChannel (add or edit: fetch
// Config, gate + mutate via saveChannel, ApplyConfig) back into Update.
type channelSavedMsg struct {
	err error
}

// settingsConfigMsg carries a freshly fetched Config back into Update for
// the "all settings" screen's group/key lists, mirroring
// scheduleConfigMsg/channelsConfigMsg's pre-fill role for their own screens.
type settingsConfigMsg struct {
	cfg *config.Config
	err error
}

// settingsAppliedMsg carries the result of applying a single settings-
// screen key edit (fetch Config, applyConfigKey, ApplyConfig) back into
// Update. key is carried through so the result screen can show the
// restart-required caveat for the key that was just applied.
type settingsAppliedMsg struct {
	key string
	err error
}

// --- commands ---

// fetchScheduleConfigCmd fetches Config fresh so the Schedule screen can
// pre-fill its mode/value from whatever is currently active (see
// scheduleConfigMsg's doc).
func fetchScheduleConfigCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		return scheduleConfigMsg{cfg: cfg, err: err}
	}
}

// fetchQuietConfigCmd is fetchScheduleConfigCmd's counterpart for the Quiet
// hours screen.
func fetchQuietConfigCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		return quietHoursConfigMsg{cfg: cfg, err: err}
	}
}

// fetchHealthConfigCmd is fetchScheduleConfigCmd's counterpart for the
// Healthchecks screen.
func fetchHealthConfigCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		return healthchecksConfigMsg{cfg: cfg, err: err}
	}
}

// applyScheduleCmd fetches Config fresh, applies ans via applySchedule
// (manage_schedule.go), and posts the result with ApplyConfig -- the same
// fetch/mutate/apply shape applyWebSetupCmd (tui.go) uses for the web-setup
// wizard.
func applyScheduleCmd(api core.API, ans scheduleAnswers) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return manageAppliedMsg{err: fmt.Errorf("fetching current config: %w", err)}
		}
		if err := applySchedule(cfg, ans); err != nil {
			return manageAppliedMsg{err: err}
		}
		if err := api.ApplyConfig(cfg); err != nil {
			return manageAppliedMsg{err: err}
		}
		return manageAppliedMsg{}
	}
}

// applyQuietHoursCmd is applyScheduleCmd's counterpart for the Quiet hours
// screen's single raw value (applyQuietHours, manage_quiet.go).
func applyQuietHoursCmd(api core.API, raw string) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return manageAppliedMsg{err: fmt.Errorf("fetching current config: %w", err)}
		}
		if err := applyQuietHours(cfg, raw); err != nil {
			return manageAppliedMsg{err: err}
		}
		if err := api.ApplyConfig(cfg); err != nil {
			return manageAppliedMsg{err: err}
		}
		return manageAppliedMsg{}
	}
}

// applyHealthchecksCmd is applyScheduleCmd's counterpart for the
// Healthchecks screen's single raw value (applyHealthchecks, manage_health.go).
func applyHealthchecksCmd(api core.API, raw string) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return manageAppliedMsg{err: fmt.Errorf("fetching current config: %w", err)}
		}
		if err := applyHealthchecks(cfg, raw); err != nil {
			return manageAppliedMsg{err: err}
		}
		if err := api.ApplyConfig(cfg); err != nil {
			return manageAppliedMsg{err: err}
		}
		return manageAppliedMsg{}
	}
}

// discoverMonitorCmd calls api.MonitorTargets over the control socket --
// the daemon runs its own live target discovery (docker/df/smartctl probes,
// serverwatch.DiscoverLocal) and reports back a []core.TargetView, so ctl
// never has to import internal/serverwatch or run those probes itself --
// alongside a fresh Config fetch, so the Monitor thresholds screen can merge
// them via buildMonitorRows.
func discoverMonitorCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		targets, err := api.MonitorTargets(context.Background())
		if err != nil {
			return monitorTargetsMsg{err: err}
		}
		cfg, err := api.Config()
		return monitorTargetsMsg{targets: targets, cfg: cfg, err: err}
	}
}

// applyMonitorEnableCmd fetches Config fresh, flips target's enabled state
// via applyMonitorEnable (manage_monitor.go), and posts it with
// ApplyConfig -- one atomic apply per toggle, matching `serverwatch
// monitor enable|disable` doing one save per invocation.
func applyMonitorEnableCmd(api core.API, target string, enabled bool) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return monitorAppliedMsg{err: fmt.Errorf("fetching current config: %w", err)}
		}
		applyMonitorEnable(cfg, target, enabled)
		if err := api.ApplyConfig(cfg); err != nil {
			return monitorAppliedMsg{err: err}
		}
		return monitorAppliedMsg{cfg: cfg}
	}
}

// applyMonitorThresholdCmd is applyMonitorEnableCmd's counterpart for
// editing a single target's threshold override (applyMonitorThreshold,
// manage_monitor.go).
func applyMonitorThresholdCmd(api core.API, target, valueStr string) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return monitorAppliedMsg{err: fmt.Errorf("fetching current config: %w", err)}
		}
		if err := applyMonitorThreshold(cfg, target, valueStr); err != nil {
			return monitorAppliedMsg{err: err}
		}
		if err := api.ApplyConfig(cfg); err != nil {
			return monitorAppliedMsg{err: err}
		}
		return monitorAppliedMsg{cfg: cfg}
	}
}

// fetchChannelsConfigCmd fetches Config fresh so the Channels screen's list
// always reflects the daemon's CURRENT channels, never a stale snapshot
// (mirrors fetchScheduleConfigCmd's role for its own screen).
func fetchChannelsConfigCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		return channelsConfigMsg{cfg: cfg, err: err}
	}
}

// removeChannelCmd fetches Config fresh, removes name via applyChannelRemove
// (channels.go), and posts it with ApplyConfig -- one atomic apply per
// removal, matching `serverwatch channel remove` doing one save per
// invocation. The freshly-applied cfg comes back on channelActionMsg so the
// list can be rebuilt from what was actually saved.
func removeChannelCmd(api core.API, name string) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return channelActionMsg{name: name, err: fmt.Errorf("fetching current config: %w", err)}
		}
		if err := applyChannelRemove(cfg, name); err != nil {
			return channelActionMsg{name: name, err: err}
		}
		if err := api.ApplyConfig(cfg); err != nil {
			return channelActionMsg{name: name, err: err}
		}
		return channelActionMsg{cfg: cfg, name: name}
	}
}

// testChannelCmd calls api.TestChannel(name) -- the same send-a-real-test-
// notification path `channel test`/the web channels page's "send test"
// button use (sendTestNotification, internal/serverwatch/channel.go) --
// against the channel as it is CURRENTLY saved on the daemon; it does not
// touch config.
func testChannelCmd(api core.API, name string) tea.Cmd {
	return func() tea.Msg {
		err := api.TestChannel(name)
		return channelActionMsg{name: name, isTest: true, err: err}
	}
}

// saveChannelCmd fetches Config fresh, gates + mutates it via saveChannel
// (channels.go, the #79-safe validate-before-save path), and posts it with
// ApplyConfig -- the same fetch/mutate/apply shape applyScheduleCmd uses,
// except the mutate step here can itself fail a live api.ValidateChannel
// check before anything is written.
func saveChannelCmd(api core.API, name string, ans channelAnswers, isEdit bool) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return channelSavedMsg{err: fmt.Errorf("fetching current config: %w", err)}
		}
		if err := saveChannel(api, cfg, name, ans, isEdit); err != nil {
			return channelSavedMsg{err: err}
		}
		if err := api.ApplyConfig(cfg); err != nil {
			return channelSavedMsg{err: err}
		}
		return channelSavedMsg{}
	}
}

// fetchSettingsConfigCmd fetches Config fresh so the "all settings" screen's
// group/key lists always reflect the daemon's CURRENT values (mirrors
// fetchScheduleConfigCmd/fetchChannelsConfigCmd's role for their screens).
func fetchSettingsConfigCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		return settingsConfigMsg{cfg: cfg, err: err}
	}
}

// applyConfigKeyCmd fetches Config fresh, sets exactly key via
// applyConfigKey (manage_config.go, the same validated config.Set setter
// every other manage screen ultimately uses), and posts it with
// ApplyConfig -- the same fetch/mutate/apply shape applyScheduleCmd uses.
// An error from either applyConfigKey (a rejected value) or ApplyConfig
// surfaces identically on settingsAppliedMsg.err; ApplyConfig is never
// called when applyConfigKey itself failed, so an invalid value is never
// persisted.
func applyConfigKeyCmd(api core.API, key, raw string) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return settingsAppliedMsg{key: key, err: fmt.Errorf("fetching current config: %w", err)}
		}
		if err := applyConfigKey(cfg, key, raw); err != nil {
			return settingsAppliedMsg{key: key, err: err}
		}
		if err := api.ApplyConfig(cfg); err != nil {
			return settingsAppliedMsg{key: key, err: err}
		}
		return settingsAppliedMsg{key: key}
	}
}

// --- Update ---

// updateManageKey routes a keypress to whichever management screen is
// active, mirroring updateSetupKey's role for the web-setup wizard.
func (m model) updateManageKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mgr.screen {
	case manageMenuList:
		return m.updateManageMenuKey(msg)
	case manageScheduleMode:
		return m.updateScheduleModeKey(msg)
	case manageScheduleValue, manageQuietValue, manageHealthValue:
		return m.updateManageValueKey(msg)
	case manageMonitorList:
		return m.updateMonitorListKey(msg)
	case manageMonitorThreshold:
		return m.updateMonitorThresholdKey(msg)
	case manageChannelsList:
		return m.updateChannelsListKey(msg)
	case manageChannelsName:
		return m.updateChannelsNameKey(msg)
	case manageChannelsType:
		return m.updateChannelsTypeKey(msg)
	case manageChannelsEnabled:
		return m.updateChannelsEnabledKey(msg)
	case manageChannelsField:
		return m.updateChannelsFieldKey(msg)
	case manageSettingsGroups:
		return m.updateSettingsGroupsKey(msg)
	case manageSettingsKeys:
		return m.updateSettingsKeysKey(msg)
	case manageSettingsValue:
		return m.updateSettingsValueKey(msg)
	case manageResult:
		// Any key returns to the menu; per-screen state resets on next entry.
		m.mgr.screen = manageMenuList
		return m, nil
	}
	return m, nil
}

// updateManageMenuKey handles the top level management menu: up/down (or
// j/k) moves the cursor over manageItems, enter opens the selected screen,
// esc/q returns to Home.
func (m model) updateManageMenuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.mgr.cursor > 0 {
			m.mgr.cursor--
		}
	case "down", "j":
		if m.mgr.cursor < len(manageItems)-1 {
			m.mgr.cursor++
		}
	case "enter":
		m.mgr.applyErr = nil
		m.mgr.configLoading = true
		m.mgr.configErr = nil
		// Clear any leftover settings-screen context (setKey/setRestart)
		// from a PREVIOUS visit to "all settings": manageResult is shared
		// across every screen, and without this a stale setKey from an
		// earlier settings edit would wrongly show the restart caveat on
		// an unrelated schedule/quiet-hours/healthchecks/monitor/channels
		// result.
		m.mgr.setKey = ""
		m.mgr.setRestart = false
		switch m.mgr.cursor {
		case 0:
			m.mgr.screen = manageScheduleMode
			m.mgr.schedModeCursor = 0
			m.mgr.schedAns = scheduleAnswers{}
			return m, fetchScheduleConfigCmd(m.api)
		case 1:
			m.mgr.screen = manageQuietValue
			m.mgr.valueIn = newManageValueInput("22-6 or off")
			return m, fetchQuietConfigCmd(m.api)
		case 2:
			m.mgr.screen = manageHealthValue
			m.mgr.valueIn = newManageValueInput("https://hc-ping.com/... or off")
			return m, fetchHealthConfigCmd(m.api)
		case 3:
			m.mgr.configLoading = false
			m.mgr.screen = manageMonitorList
			m.mgr.monLoading = true
			m.mgr.monErr = nil
			m.mgr.monCursor = 0
			return m, discoverMonitorCmd(m.api)
		case 4:
			m.mgr.screen = manageChannelsList
			m.mgr.chanCursor = 0
			m.mgr.chanErr = nil
			m.mgr.chanTestMsg = ""
			return m, fetchChannelsConfigCmd(m.api)
		case 5:
			m.mgr.configLoading = false
			m.mgr.screen = manageSettingsGroups
			m.mgr.setLoading = true
			m.mgr.setErr = nil
			m.mgr.setGroupCur = 0
			return m, fetchSettingsConfigCmd(m.api)
		}
	case "esc", "q":
		m.step = stepHome
	}
	return m, nil
}

// updateScheduleModeKey handles the Schedule screen's mode-selection row:
// off applies immediately (no further input needed); daily/weekly move on
// to the value step for that mode's HH:MM/dow@HH:MM input, pre-filled from
// m.mgr.schedAns.Daily/Weekly (populated by scheduleConfigMsg from the
// CURRENT config, see that message's doc) so accepting the already-selected
// mode re-applies the existing value rather than an empty one. Ignores
// every key but esc while m.mgr.configLoading is still true (the pre-fill
// fetch hasn't landed yet) so a stray Enter can't act on a not-yet-loaded
// screen -- see manageModel.configLoading's doc.
func (m model) updateScheduleModeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mgr.configLoading {
		if msg.String() == "esc" {
			m.mgr.screen = manageMenuList
		}
		return m, nil
	}
	switch msg.String() {
	case "up", "k":
		if m.mgr.schedModeCursor > 0 {
			m.mgr.schedModeCursor--
		}
	case "down", "j":
		if m.mgr.schedModeCursor < len(scheduleModeChoices)-1 {
			m.mgr.schedModeCursor++
		}
	case "enter":
		switch scheduleModeChoices[m.mgr.schedModeCursor] {
		case "off":
			m.mgr.schedAns = scheduleAnswers{Mode: scheduleOff}
			m.mgr.applying = true
			return m, applyScheduleCmd(m.api, m.mgr.schedAns)
		case "daily":
			m.mgr.schedAns.Mode = scheduleDaily
			m.mgr.screen = manageScheduleValue
			m.mgr.valueIn = newManageValueInput("HH:MM")
			if m.mgr.schedAns.Daily != "" {
				m.mgr.valueIn.SetValue(m.mgr.schedAns.Daily)
				m.mgr.valueIn.CursorEnd()
			}
			return m, m.mgr.valueIn.Focus()
		case "weekly":
			m.mgr.schedAns.Mode = scheduleWeekly
			m.mgr.screen = manageScheduleValue
			m.mgr.valueIn = newManageValueInput("dow@HH:MM e.g. mon@09:00")
			if m.mgr.schedAns.Weekly != "" {
				m.mgr.valueIn.SetValue(m.mgr.schedAns.Weekly)
				m.mgr.valueIn.CursorEnd()
			}
			return m, m.mgr.valueIn.Focus()
		}
	case "esc":
		m.mgr.screen = manageMenuList
	}
	return m, nil
}

// updateManageValueKey feeds msg into the single-line value input shared by
// the schedule-value/quiet-hours/healthchecks screens: enter commits (via
// the matching applyXCmd) and esc backs out (to the schedule mode screen
// for the schedule value step, or straight to the menu for quiet-hours/
// healthchecks, which have no intermediate mode step). Mirrors tui.go's
// updateTextKey (see that function's doc for why mutation and return stay
// on the same receiver copy throughout). Ignores every key but esc while
// m.mgr.configLoading is still true (quiet-hours/healthchecks fetch their
// pre-fill directly into this screen, unlike schedule's intermediate mode
// step) -- see manageModel.configLoading's doc.
func (m model) updateManageValueKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mgr.configLoading {
		if msg.String() == "esc" {
			m.mgr.screen = manageMenuList
		}
		return m, nil
	}
	switch msg.String() {
	case "enter":
		val := m.mgr.valueIn.Value()
		switch m.mgr.screen {
		case manageScheduleValue:
			if m.mgr.schedAns.Mode == scheduleDaily {
				m.mgr.schedAns.Daily = val
			} else {
				m.mgr.schedAns.Weekly = val
			}
			m.mgr.applying = true
			return m, applyScheduleCmd(m.api, m.mgr.schedAns)
		case manageQuietValue:
			m.mgr.applying = true
			return m, applyQuietHoursCmd(m.api, val)
		case manageHealthValue:
			m.mgr.applying = true
			return m, applyHealthchecksCmd(m.api, val)
		}
		return m, nil
	case "esc":
		if m.mgr.screen == manageScheduleValue {
			m.mgr.screen = manageScheduleMode
		} else {
			m.mgr.screen = manageMenuList
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.mgr.valueIn, cmd = m.mgr.valueIn.Update(msg)
	return m, cmd
}

// updateMonitorListKey handles the Monitor thresholds list: up/down (or
// j/k) moves the cursor, enter/space toggles the target under the cursor's
// enabled state (applied immediately, one ApplyConfig per toggle), 't'
// opens the threshold-edit input for that target, esc/q returns to the menu.
func (m model) updateMonitorListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.mgr.monCursor > 0 {
			m.mgr.monCursor--
		}
	case "down", "j":
		if m.mgr.monCursor < len(m.mgr.monRows)-1 {
			m.mgr.monCursor++
		}
	case "enter", " ":
		if len(m.mgr.monRows) == 0 {
			return m, nil
		}
		row := m.mgr.monRows[m.mgr.monCursor]
		m.mgr.monErr = nil
		return m, applyMonitorEnableCmd(m.api, row.ID, !row.Enabled)
	case "t":
		if len(m.mgr.monRows) == 0 {
			return m, nil
		}
		row := m.mgr.monRows[m.mgr.monCursor]
		m.mgr.monThreshIn = newManageValueInput("threshold value")
		if row.ThresholdSet {
			m.mgr.monThreshIn.SetValue(strconv.FormatFloat(row.Threshold, 'f', -1, 64))
			m.mgr.monThreshIn.CursorEnd()
		}
		m.mgr.monEditingThreshold = true
		m.mgr.screen = manageMonitorThreshold
		return m, m.mgr.monThreshIn.Focus()
	case "esc", "q":
		m.mgr.screen = manageMenuList
	}
	return m, nil
}

// updateMonitorThresholdKey feeds msg into the threshold-edit input: enter
// commits (applyMonitorThresholdCmd) and returns to the list, esc discards.
func (m model) updateMonitorThresholdKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.mgr.monEditingThreshold = false
		m.mgr.screen = manageMonitorList
		if len(m.mgr.monRows) == 0 {
			return m, nil
		}
		row := m.mgr.monRows[m.mgr.monCursor]
		val := m.mgr.monThreshIn.Value()
		return m, applyMonitorThresholdCmd(m.api, row.ID, val)
	case "esc":
		m.mgr.monEditingThreshold = false
		m.mgr.screen = manageMonitorList
		return m, nil
	}
	var cmd tea.Cmd
	m.mgr.monThreshIn, cmd = m.mgr.monThreshIn.Update(msg)
	return m, cmd
}

// updateSettingsGroupsKey handles the "all settings" screen's top level
// group list: up/down (or j/k) moves the cursor over mgr.setGroups, enter
// opens the selected group's key list, esc/q returns to the menu. Ignores
// every key but esc while mgr.setLoading is still true (the pre-fill fetch
// hasn't landed yet), mirroring updateScheduleModeKey's configLoading guard.
func (m model) updateSettingsGroupsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mgr.setLoading {
		if msg.String() == "esc" {
			m.mgr.screen = manageMenuList
		}
		return m, nil
	}
	switch msg.String() {
	case "up", "k":
		if m.mgr.setGroupCur > 0 {
			m.mgr.setGroupCur--
		}
	case "down", "j":
		if m.mgr.setGroupCur < len(m.mgr.setGroups)-1 {
			m.mgr.setGroupCur++
		}
	case "enter":
		if len(m.mgr.setGroups) == 0 {
			return m, nil
		}
		group := m.mgr.setGroups[m.mgr.setGroupCur]
		m.mgr.setKeys = settingsGroupKeys(group)
		m.mgr.setKeyCur = 0
		m.mgr.screen = manageSettingsKeys
	case "esc", "q":
		m.mgr.screen = manageMenuList
	}
	return m, nil
}

// updateSettingsKeysKey handles the "all settings" screen's key list for
// the currently selected group: up/down (or j/k) moves the cursor over
// mgr.setKeys, enter opens the value input for the key under the cursor
// (pre-filled with its CURRENT value from mgr.setCfg.Get, fetched when the
// screen was opened), esc goes back to the group list.
func (m model) updateSettingsKeysKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.mgr.setKeyCur > 0 {
			m.mgr.setKeyCur--
		}
	case "down", "j":
		if m.mgr.setKeyCur < len(m.mgr.setKeys)-1 {
			m.mgr.setKeyCur++
		}
	case "enter":
		if len(m.mgr.setKeys) == 0 {
			return m, nil
		}
		ki := m.mgr.setKeys[m.mgr.setKeyCur]
		m.mgr.setKey = ki.Name
		m.mgr.setRestart = ki.RestartRequired
		m.mgr.setValueIn = newManageValueInput(ki.Help)
		if m.mgr.setCfg != nil {
			if val, ok := m.mgr.setCfg.Get(ki.Name); ok {
				m.mgr.setValueIn.SetValue(val)
				m.mgr.setValueIn.CursorEnd()
			}
		}
		m.mgr.screen = manageSettingsValue
		return m, m.mgr.setValueIn.Focus()
	case "esc":
		m.mgr.screen = manageSettingsGroups
	}
	return m, nil
}

// updateSettingsValueKey feeds msg into the selected key's value input:
// enter commits it via applyConfigKeyCmd (config.Set's real validation, so
// a rejected value never reaches ApplyConfig), esc discards and returns to
// the key list.
func (m model) updateSettingsValueKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.mgr.applying = true
		return m, applyConfigKeyCmd(m.api, m.mgr.setKey, m.mgr.setValueIn.Value())
	case "esc":
		m.mgr.screen = manageSettingsKeys
		return m, nil
	}
	var cmd tea.Cmd
	m.mgr.setValueIn, cmd = m.mgr.setValueIn.Update(msg)
	return m, cmd
}

// --- View ---

// manageView renders whichever management screen is active.
func (m model) manageView() string {
	var b strings.Builder
	b.WriteString(breadcrumb("Manage") + "\n\n")
	switch m.mgr.screen {
	case manageMenuList:
		b.WriteString(faintStyle.Render("what would you like to manage?") + "\n\n")
		for i, item := range manageItems {
			label := item
			if ic := manageIcons[item]; ic != "" {
				label = ic + "  " + item
			}
			b.WriteString(menuRow(i == m.mgr.cursor, label) + "\n")
		}
		b.WriteString("\n" + hintStyle.Render("↑/↓ move   enter open   esc home") + "\n")
	case manageScheduleMode:
		if m.mgr.configLoading {
			b.WriteString(faintStyle.Render("loading current schedule...") + "\n")
		} else {
			b.WriteString(panelTitleStyle.Render("SCHEDULE") + "\n\n")
			for i, choice := range scheduleModeChoices {
				b.WriteString(menuRow(i == m.mgr.schedModeCursor, choice) + "\n")
			}
			if m.mgr.configErr != nil {
				b.WriteString("\n" + errStyle.Render(fmt.Sprintf("could not load the current schedule: %v", m.mgr.configErr)) + "\n")
			}
			b.WriteString("\n" + hintStyle.Render("↑/↓ choose   enter select   esc cancel") + "\n")
		}
	case manageScheduleValue:
		label := "daily time (HH:MM):"
		if m.mgr.schedAns.Mode == scheduleWeekly {
			label = "weekly time (dow@HH:MM):"
		}
		fmt.Fprintf(&b, "%s\n\n%s\n", label, m.mgr.valueIn.View())
		b.WriteString(manageApplyingOrHint(m.mgr.applying))
	case manageQuietValue:
		if m.mgr.configLoading {
			b.WriteString("loading current quiet hours...\n")
		} else {
			fmt.Fprintf(&b, "quiet hours (HH-HH, or \"off\"):\n\n%s\n", m.mgr.valueIn.View())
			if m.mgr.configErr != nil {
				b.WriteString("\n" + errStyle.Render(fmt.Sprintf("could not load the current value: %v", m.mgr.configErr)) + "\n")
			}
			b.WriteString(manageApplyingOrHint(m.mgr.applying))
		}
	case manageHealthValue:
		if m.mgr.configLoading {
			b.WriteString("loading current healthchecks setting...\n")
		} else {
			fmt.Fprintf(&b, "healthchecks ping URL (or \"off\"):\n\n%s\n", m.mgr.valueIn.View())
			if m.mgr.configErr != nil {
				b.WriteString("\n" + errStyle.Render(fmt.Sprintf("could not load the current value: %v", m.mgr.configErr)) + "\n")
			}
			b.WriteString(manageApplyingOrHint(m.mgr.applying))
		}
	case manageMonitorList:
		b.WriteString(m.monitorListView())
	case manageMonitorThreshold:
		row := monitorTargetRow{}
		if len(m.mgr.monRows) > 0 {
			row = m.mgr.monRows[m.mgr.monCursor]
		}
		fmt.Fprintf(&b, "threshold for %s (%s):\n\n%s\n", row.ID, row.Kind, m.mgr.monThreshIn.View())
		b.WriteString("\n" + hintStyle.Render("enter to save, esc to cancel") + "\n")
	case manageChannelsList:
		b.WriteString(m.channelsListView())
	case manageChannelsName:
		fmt.Fprintf(&b, "channel name:\n\n%s\n", m.mgr.chanNameIn.View())
		b.WriteString("\n" + hintStyle.Render("enter to continue, esc to cancel") + "\n")
	case manageChannelsType:
		b.WriteString(panelTitleStyle.Render("CHANNEL TYPE") + "\n\n")
		for i, choice := range channelTypeChoices {
			b.WriteString(menuRow(i == m.mgr.chanTypeCur, choice) + "\n")
		}
		b.WriteString("\n" + hintStyle.Render("↑/↓ choose   enter select   esc back") + "\n")
	case manageChannelsEnabled:
		state := faintStyle.Render("○ disabled")
		if m.mgr.chanAns.Enabled {
			state = okStyle.Render("● enabled")
		}
		fmt.Fprintf(&b, "channel state: %s\n", state)
		b.WriteString("\n" + hintStyle.Render("↑/↓/space toggle   enter continue   esc back") + "\n")
	case manageChannelsField:
		if m.mgr.chanSaving {
			b.WriteString("saving...\n")
		} else {
			fields := channelTypeFields[m.mgr.chanAns.Type]
			label := ""
			if m.mgr.chanFieldIdx < len(fields) {
				label = fields[m.mgr.chanFieldIdx].Label
			}
			fmt.Fprintf(&b, "%s:\n\n%s\n", label, m.mgr.chanFieldIn.View())
			b.WriteString("\n" + hintStyle.Render("enter to continue, esc to go back") + "\n")
		}
	case manageSettingsGroups:
		if m.mgr.setLoading {
			b.WriteString(faintStyle.Render("loading current settings...") + "\n")
		} else {
			b.WriteString(panelTitleStyle.Render("ALL SETTINGS") + faintStyle.Render("  by group") + "\n\n")
			for i, group := range m.mgr.setGroups {
				b.WriteString(menuRow(i == m.mgr.setGroupCur, group) + "\n")
			}
			if m.mgr.setErr != nil {
				b.WriteString("\n" + errStyle.Render(fmt.Sprintf("could not load the current config: %v", m.mgr.setErr)) + "\n")
			}
			b.WriteString("\n" + hintStyle.Render("↑/↓ choose   enter open   esc back") + "\n")
		}
	case manageSettingsKeys:
		if len(m.mgr.setKeys) == 0 {
			b.WriteString(faintStyle.Render("no keys in this group.") + "\n")
		} else {
			for i, ki := range m.mgr.setKeys {
				val := ""
				if m.mgr.setCfg != nil {
					val, _ = m.mgr.setCfg.Get(ki.Name)
				}
				restart := ""
				if ki.RestartRequired {
					restart = warnStyle.Render(" (restart required)")
				}
				// key name + current value, then the help/restart note dimmed.
				row := fmt.Sprintf("%-28s %s", ki.Name, signalStyle.Render(fmt.Sprintf("%-16s", val)))
				row += faintStyle.Render(ki.Help) + restart
				b.WriteString(menuRow(i == m.mgr.setKeyCur, row) + "\n")
			}
		}
		b.WriteString("\n" + hintStyle.Render("↑/↓ choose   enter edit   esc back") + "\n")
	case manageSettingsValue:
		fmt.Fprintf(&b, "%s (%s):\n\n%s\n", m.mgr.setKey, currentSettingsKeyHelp(m.mgr.setKeys, m.mgr.setKeyCur), m.mgr.setValueIn.View())
		if m.mgr.setRestart {
			b.WriteString("\n" + hintStyle.Render("this setting only takes effect after a daemon restart") + "\n")
		}
		b.WriteString(manageApplyingOrHint(m.mgr.applying))
	case manageResult:
		if m.mgr.applyErr != nil {
			b.WriteString(critStyle.Render("✗ apply failed: ") + fmt.Sprintf("%v", m.mgr.applyErr) + "\n")
		} else {
			b.WriteString(okStyle.Render("✓ applied") + "\n")
			if m.mgr.setKey != "" && m.mgr.setRestart {
				b.WriteString(warnStyle.Render("this setting only takes effect after a daemon restart") + "\n")
			}
		}
		b.WriteString("\n" + hintStyle.Render("press any key to return to the menu") + "\n")
	}
	return b.String()
}

// currentSettingsKeyHelp returns keys[cur].Help, or "" if cur is out of
// range (defensive: the value screen is only reachable via a valid
// selection, but View must never index out of bounds).
func currentSettingsKeyHelp(keys []config.KeyInfo, cur int) string {
	if cur < 0 || cur >= len(keys) {
		return ""
	}
	return keys[cur].Help
}

// manageApplyingOrHint is the value screens' trailing line: "applying..."
// while the ApplyConfig round trip is in flight, otherwise the usual
// enter/esc hint.
func manageApplyingOrHint(applying bool) string {
	if applying {
		return "\napplying...\n"
	}
	return "\n" + hintStyle.Render("enter to apply, esc to cancel") + "\n"
}

// monitorListView renders the Monitor thresholds screen's target table.
func (m model) monitorListView() string {
	var b strings.Builder
	if m.mgr.monLoading {
		b.WriteString("discovering targets...\n")
		return b.String()
	}
	if m.mgr.monErr != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("error: %v", m.mgr.monErr)) + "\n\n")
	}
	if len(m.mgr.monRows) == 0 {
		b.WriteString(faintStyle.Render("no monitorable targets discovered.") + "\n")
	} else {
		for i, row := range m.mgr.monRows {
			threshold := faintStyle.Render("thr -")
			if row.ThresholdSet {
				threshold = faintStyle.Render("thr ") + signalStyle.Render(strconv.FormatFloat(row.Threshold, 'f', -1, 64))
			}
			line := fmt.Sprintf("%-28s %s  %s  %s",
				row.ID, faintStyle.Render(fmt.Sprintf("%-8s", row.Kind)),
				stateBadge(row.Enabled, row.Available), threshold)
			b.WriteString(menuRow(i == m.mgr.monCursor, line) + "\n")
		}
	}
	b.WriteString("\n" + hintStyle.Render("↑/↓ move   enter/space toggle   t threshold   esc back") + "\n")
	return b.String()
}
