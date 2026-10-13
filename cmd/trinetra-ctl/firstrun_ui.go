package main

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type firstRunScreen int

const (
	frWelcome firstRunScreen = iota
	frWeb
	frAdmin
	frAlerts
	frDone
)

type firstRunModel struct {
	active bool
	screen firstRunScreen
	// resume is where the flow continues after a detour into the web
	// wizard, the channel editor or Telegram setup.
	resume firstRunScreen

	inviting    bool
	inviteURL   string
	inviteErr   error
	adminExists bool

	webVisited    bool
	alertsVisited bool
}

type firstRunInviteMsg struct {
	url    string
	err    error
	exists bool
}

func firstRunInviteCmd() tea.Cmd {
	return func() tea.Msg {
		if n, err := webUsersCountFn(); err == nil && n > 0 {
			return firstRunInviteMsg{exists: true}
		}
		url, err := inviteAdminFn()
		return firstRunInviteMsg{url: url, err: err}
	}
}

func (m model) startFirstRun() model {
	m.step = stepFirstRun
	m.firstRun = firstRunModel{active: true, screen: frWelcome}
	return m
}

// home leaves a detour: back into the first run where it left off, or Home.
func (m model) home() model {
	if m.firstRun.active {
		m.step = stepFirstRun
		m.firstRun.screen = m.firstRun.resume
		if m.firstRun.screen == frAdmin && m.firstRun.inviteURL == "" && !m.firstRun.adminExists {
			m.firstRun.inviting = true
		}
		return m
	}
	m.step = stepHome
	return m
}

// goHome is home() plus the follow-up a resumed admin step needs.
func (m model) goHome() (tea.Model, tea.Cmd) {
	m = m.home()
	if m.step == stepFirstRun && m.firstRun.screen == frAdmin && m.firstRun.inviting {
		return m, firstRunInviteCmd()
	}
	return m, nil
}

func (m model) enterFirstRunAdmin() (model, tea.Cmd) {
	m.firstRun.screen = frAdmin
	m.firstRun.inviting = true
	m.firstRun.inviteErr = nil
	return m, firstRunInviteCmd()
}

func (m model) finishFirstRun() (tea.Model, tea.Cmd) {
	m.firstRun.active = false
	m.firstRunDone = true
	m.step = stepHome
	return m, applySetupCompletedCmd(m.api)
}

func (m model) updateFirstRunKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch m.firstRun.screen {
	case frWelcome:
		switch k {
		case "enter":
			m.firstRun.screen = frWeb
		case "esc":
			return m.finishFirstRun()
		}
	case frWeb:
		switch k {
		case "enter":
			m.firstRun.webVisited = true
			m.firstRun.resume = frAdmin
			m.step = stepSetupWeb
			m.wiz = webSetupMode
			m.modeCursor = 0
			m.ans = webSetupAnswers{}
			m.applyErr = nil
			m.wizFieldErr = nil
		case "esc":
			return m.enterFirstRunAdmin()
		}
	case frAdmin:
		if m.firstRun.inviting {
			if k == "esc" {
				m.firstRun.inviting = false
				m.firstRun.screen = frAlerts
			}
			return m, nil
		}
		if k == "enter" || k == "esc" {
			m.firstRun.screen = frAlerts
		}
	case frAlerts:
		switch k {
		case "t":
			m.firstRun.alertsVisited = true
			m.firstRun.resume = frDone
			return m.enterTelegramSetup()
		case "c":
			m.firstRun.alertsVisited = true
			m.firstRun.resume = frDone
			return m.enterChannels()
		case "esc", "enter":
			m.firstRun.screen = frDone
		}
	case frDone:
		if k == "enter" || k == "esc" {
			return m.finishFirstRun()
		}
	}
	return m, nil
}

func (m model) enterChannels() (tea.Model, tea.Cmd) {
	m.step = stepManage
	m.mgr = manageModel{screen: manageChannelsList, configLoading: true}
	return m, fetchChannelsConfigCmd(m.api)
}

func (m model) enterTelegramSetup() (tea.Model, tea.Cmd) {
	m.step = stepOnboard
	m.onboard = onboardModel{tokenIn: newManageValueInput("bot token from @BotFather")}
	return m, m.onboard.tokenIn.Focus()
}

func (m model) firstRunView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Set up trinetra") + "\n\n")
	switch m.firstRun.screen {
	case frWelcome:
		b.WriteString("trinetra is installed and already monitoring this server.\n")
		b.WriteString("This short setup gets the web UI running, creates your admin account,\nand picks where alerts go. Every step can be skipped.\n\n")
		b.WriteString(hintStyle.Render("enter start   esc skip setup"))
	case frWeb:
		b.WriteString(titleStyle.Render("1. Web UI") + "\n")
		b.WriteString("Choose how the web UI is reached: behind your own proxy, with automatic\nHTTPS, or with your own certificate.\n\n")
		b.WriteString(hintStyle.Render("enter set it up now   esc skip"))
	case frAdmin:
		b.WriteString(titleStyle.Render("2. Your admin account") + "\n")
		switch {
		case m.firstRun.inviting:
			b.WriteString("Creating your enroll link...\n\n")
			b.WriteString(hintStyle.Render("esc skip"))
		case m.firstRun.inviteErr != nil:
			b.WriteString(errStyle.Render("Couldn't create the link: "+strings.TrimSpace(m.firstRun.inviteErr.Error())) + "\n")
			b.WriteString("You can do this later with: sudo trinetra users invite --role admin\n\n")
			b.WriteString(hintStyle.Render("enter continue"))
		default:
			b.WriteString("Open this link on the device you'll sign in from to create the admin account:\n\n")
			b.WriteString("  " + m.firstRun.inviteURL + "\n\n")
			b.WriteString("Single use, expires in 24 hours. Get another any time with:\n  sudo trinetra users invite --role admin\n\n")
			b.WriteString(hintStyle.Render("enter continue"))
		}
	case frAlerts:
		b.WriteString(titleStyle.Render("3. Where should alerts go?") + "\n")
		b.WriteString("Pick it in the web UI under Notifications, or set one up here.\n\n")
		b.WriteString(hintStyle.Render("t Telegram   c Slack, Discord, email, ntfy, Gotify or webhook   esc later"))
	case frDone:
		b.WriteString(titleStyle.Render("All set") + "\n")
		b.WriteString("Admin account: " + firstRunAdminSummary(m.firstRun) + "\n")
		alerts := "not yet; you'll see a reminder until you add a channel"
		if m.firstRun.alertsVisited {
			alerts = "set up here"
		}
		b.WriteString("Alerts: " + alerts + "\n\n")
		b.WriteString("Later: sudo trinetra cli (this console), sudo trinetra users invite --role ...,\n       trinetra channel add\n\n")
		b.WriteString(hintStyle.Render("enter finish"))
	}
	return b.String()
}

func firstRunAdminSummary(f firstRunModel) string {
	switch {
	case f.adminExists:
		return "already exists"
	case f.inviteURL != "":
		return "enroll link shown"
	default:
		return "skipped; run sudo trinetra users invite --role admin"
	}
}
