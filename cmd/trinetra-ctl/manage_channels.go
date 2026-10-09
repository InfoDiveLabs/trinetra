// manage_channels.go is the Channels screen's Bubble Tea glue: list, add, edit, remove, and
// test.
package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// updateChannelsListKey handles the Channels screen's list: up/down (or j/k) moves the
// cursor, 'a' starts the add flow, 'e' edits the channel under the cursor.
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

// updateChannelsNameKey feeds msg into the add flow's name input.
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

// updateChannelsTypeKey handles the type-selection row (channelTypeChoices): up/down moves
// the cursor, enter commits and moves on to the enabled/disabled toggle, esc backs out.
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

// updateChannelsEnabledKey handles the enabled/disabled toggle: any of up/down/j/k/space
// flips it.
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
// (channelTypeFields[ans.Type][chanFieldIdx]), pre-filled from any existing value.
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

// updateChannelsFieldKey feeds msg into the current per-type Settings field input: enter
// commits it into chanAns.Settings and advances to the next field.
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
		b.WriteString(okStyle.Render("✓ ") + m.mgr.chanTestMsg + "\n\n")
	}
	if len(m.mgr.chanList) == 0 {
		b.WriteString(faintStyle.Render("no channels configured.") + "\n")
	} else {
		for i, cc := range m.mgr.chanList {
			line := fmt.Sprintf("%-16s %s  %s",
				cc.Name, faintStyle.Render(fmt.Sprintf("%-10s", cc.Type)),
				stateBadge(cc.Enabled, true))
			b.WriteString(menuRow(i == m.mgr.chanCursor, line) + "\n")
		}
	}
	b.WriteString("\n" + hintStyle.Render("a add   e edit   d remove   t test   esc back") + "\n")
	return b.String()
}
