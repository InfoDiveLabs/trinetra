// manage_channels.go is the Channels screen's Bubble Tea glue: list, add,
// edit, remove, and test. It is deliberately thin -- the config mutations
// and the #79-safe validate-before-save gate it drives live in channels.go
// (unit-tested there against a fake core.API, no terminal involved),
// mirroring manage_ui.go's own split for the schedule/quiet-hours/
// healthchecks/monitor-thresholds screens.
package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// updateChannelsListKey handles the Channels screen's list: up/down (or
// j/k) moves the cursor, 'a' starts the add flow, 'e' edits the channel
// under the cursor, 'd'/'x' removes it (applied immediately, one
// ApplyConfig per removal -- mirroring the Monitor thresholds list's
// immediate-toggle pattern), 't' sends a live test notification via
// api.TestChannel, esc/q returns to the menu. Ignores every key but esc
// while the screen's pre-fill fetch is still in flight (mirrors
// updateScheduleModeKey/updateManageValueKey's configLoading guard -- see
// manageModel.configLoading's doc for why this matters).
func (m model) updateChannelsListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mgr.configLoading {
		if msg.String() == "esc" {
			m.mgr.screen = manageMenuList
		}
		return m, nil
	}
	switch msg.String() {
	case "up", "k":
		if m.mgr.chanCursor > 0 {
			m.mgr.chanCursor--
		}
	case "down", "j":
		if m.mgr.chanCursor < len(m.mgr.chanList)-1 {
			m.mgr.chanCursor++
		}
	case "a":
		m.mgr.chanIsEdit = false
		m.mgr.chanEditName = ""
		m.mgr.chanAns = channelAnswers{Enabled: true}
		m.mgr.chanNameIn = newManageValueInput("channel name")
		m.mgr.screen = manageChannelsName
		return m, m.mgr.chanNameIn.Focus()
	case "e":
		if len(m.mgr.chanList) == 0 {
			return m, nil
		}
		cc := m.mgr.chanList[m.mgr.chanCursor]
		m.mgr.chanIsEdit = true
		m.mgr.chanEditName = cc.Name
		m.mgr.chanAns = channelAnswers{Name: cc.Name, Type: cc.Type, Enabled: cc.Enabled, Settings: copyChannelSettings(cc.Settings)}
		m.mgr.chanTypeCur = indexOfChannelType(cc.Type)
		m.mgr.screen = manageChannelsType
		return m, nil
	case "d", "x":
		if len(m.mgr.chanList) == 0 {
			return m, nil
		}
		cc := m.mgr.chanList[m.mgr.chanCursor]
		m.mgr.chanErr = nil
		m.mgr.chanTestMsg = ""
		return m, removeChannelCmd(m.api, cc.Name)
	case "t":
		if len(m.mgr.chanList) == 0 {
			return m, nil
		}
		cc := m.mgr.chanList[m.mgr.chanCursor]
		m.mgr.chanErr = nil
		m.mgr.chanTestMsg = ""
		return m, testChannelCmd(m.api, cc.Name)
	case "esc", "q":
		m.mgr.screen = manageMenuList
	}
	return m, nil
}

// updateChannelsNameKey feeds msg into the add flow's name input (edit
// skips this step -- the name is fixed once a channel exists). enter
// commits and moves on to the type-selection screen; esc discards back to
// the list.
func (m model) updateChannelsNameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.mgr.chanAns.Name = m.mgr.chanNameIn.Value()
		m.mgr.screen = manageChannelsType
		return m, nil
	case "esc":
		m.mgr.screen = manageChannelsList
		return m, nil
	}
	var cmd tea.Cmd
	m.mgr.chanNameIn, cmd = m.mgr.chanNameIn.Update(msg)
	return m, cmd
}

// updateChannelsTypeKey handles the type-selection row (channelTypeChoices):
// up/down moves the cursor, enter commits and moves on to the
// enabled/disabled toggle, esc backs out (to the name step for add, or
// straight to the list for edit, which has no name step to return to).
func (m model) updateChannelsTypeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.mgr.chanTypeCur > 0 {
			m.mgr.chanTypeCur--
		}
	case "down", "j":
		if m.mgr.chanTypeCur < len(channelTypeChoices)-1 {
			m.mgr.chanTypeCur++
		}
	case "enter":
		m.mgr.chanAns.Type = channelTypeChoices[m.mgr.chanTypeCur]
		m.mgr.screen = manageChannelsEnabled
		return m, nil
	case "esc":
		if m.mgr.chanIsEdit {
			m.mgr.screen = manageChannelsList
			return m, nil
		}
		m.mgr.screen = manageChannelsName
		return m, m.mgr.chanNameIn.Focus()
	}
	return m, nil
}

// updateChannelsEnabledKey handles the enabled/disabled toggle: any of
// up/down/j/k/space flips it (there are only two states, so direction
// doesn't matter, matching a simple on/off switch rather than a two-row
// list), enter commits and starts walking the type's per-field Settings
// inputs (enterChannelField), esc backs out to the type step.
func (m model) updateChannelsEnabledKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "down", "k", "j", " ":
		m.mgr.chanAns.Enabled = !m.mgr.chanAns.Enabled
	case "enter":
		m.mgr.chanFieldIdx = 0
		return m.enterChannelField()
	case "esc":
		m.mgr.screen = manageChannelsType
	}
	return m, nil
}

// enterChannelField sets up the next per-type Settings field input
// (channelTypeFields[ans.Type][chanFieldIdx]), pre-filled from any existing
// value (an edit's current setting, or a value already typed earlier this
// same add/edit pass -- e.g. backing up with esc and returning). Once every
// field for this type has been walked (chanFieldIdx reaches the end), it
// saves the channel via saveChannelCmd instead of opening another field --
// shared by the enabled-toggle step's "enter" (index 0) and the field
// step's own "enter" (index+1), so both paths fall through to the same save
// once the walk completes.
func (m model) enterChannelField() (tea.Model, tea.Cmd) {
	fields := channelTypeFields[m.mgr.chanAns.Type]
	if m.mgr.chanFieldIdx >= len(fields) {
		m.mgr.chanSaving = true
		m.mgr.screen = manageChannelsField
		return m, saveChannelCmd(m.api, m.mgr.chanEditName, m.mgr.chanAns, m.mgr.chanIsEdit)
	}
	f := fields[m.mgr.chanFieldIdx]
	m.mgr.chanFieldIn = newManageValueInput(f.Label)
	if v, ok := m.mgr.chanAns.Settings[f.Key]; ok {
		m.mgr.chanFieldIn.SetValue(v)
		m.mgr.chanFieldIn.CursorEnd()
	}
	m.mgr.screen = manageChannelsField
	return m, m.mgr.chanFieldIn.Focus()
}

// updateChannelsFieldKey feeds msg into the current per-type Settings field
// input: enter commits it into chanAns.Settings and advances to the next
// field (or saves, via enterChannelField), esc backs out to the
// enabled/disabled toggle (discarding this field's typed-but-uncommitted
// value, matching every other text step's esc behavior in this package).
func (m model) updateChannelsFieldKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		fields := channelTypeFields[m.mgr.chanAns.Type]
		if m.mgr.chanFieldIdx < len(fields) {
			f := fields[m.mgr.chanFieldIdx]
			if m.mgr.chanAns.Settings == nil {
				m.mgr.chanAns.Settings = map[string]string{}
			}
			m.mgr.chanAns.Settings[f.Key] = m.mgr.chanFieldIn.Value()
		}
		m.mgr.chanFieldIdx++
		return m.enterChannelField()
	case "esc":
		m.mgr.screen = manageChannelsEnabled
		return m, nil
	}
	var cmd tea.Cmd
	m.mgr.chanFieldIn, cmd = m.mgr.chanFieldIn.Update(msg)
	return m, cmd
}

// --- View ---

// channelsListView renders the Channels screen's list.
func (m model) channelsListView() string {
	var b strings.Builder
	if m.mgr.configLoading {
		b.WriteString("loading channels...\n")
		return b.String()
	}
	if m.mgr.configErr != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("could not load channels: %v", m.mgr.configErr)) + "\n\n")
	}
	if m.mgr.chanErr != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("error: %v", m.mgr.chanErr)) + "\n\n")
	}
	if m.mgr.chanTestMsg != "" {
		b.WriteString(m.mgr.chanTestMsg + "\n\n")
	}
	if len(m.mgr.chanList) == 0 {
		b.WriteString("no channels configured.\n")
	} else {
		for i, cc := range m.mgr.chanList {
			cursor := "  "
			if i == m.mgr.chanCursor {
				cursor = "> "
			}
			state := "off"
			if cc.Enabled {
				state = "on"
			}
			fmt.Fprintf(&b, "%s%-16s %-10s %s\n", cursor, cc.Name, cc.Type, state)
		}
	}
	b.WriteString("\n" + hintStyle.Render("a: add   e: edit   d: remove   t: test   esc: back") + "\n")
	return b.String()
}
