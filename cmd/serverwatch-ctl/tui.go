// Interactive management TUI for serverwatch-ctl, built on Bubble Tea
// (github.com/charmbracelet/bubbletea). This is the only place in the
// module that third-party terminal UI packages (bubbletea, bubbles,
// lipgloss) are imported: cmd/serverwatch-ctl is a separate binary from
// the serverwatch daemon (cmd/serverwatch), which stays stdlib-only (see
// internal/serverwatch/buildtag_test.go's TestDefaultBuildIsStdlibOnly,
// scoped to cmd/serverwatch's own dependency graph for exactly this
// reason).
//
// The model is deliberately split from the terminal plumbing: Init/Update/
// View below hold ALL of the state transition logic as plain, pure(ish)
// functions over a `model` value, so setup_web_test.go and tui_test.go can
// drive a wizard end to end (mode -> listen -> domain -> rp_id -> origin ->
// confirm -> applied) by constructing tea.KeyMsg values and calling
// Update directly, with no real terminal, no tea.Program, and a fake
// core.API standing in for the control socket.
package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"serverwatch/internal/core"
)

// step is the TUI's top level screen.
type step int

const (
	stepHome step = iota
	stepSetupWeb
	stepManage
	stepOnboard
)

// refreshInterval is how often the home screen re-fetches Snapshot() while
// idle, so "live status" actually stays live without the user pressing a
// refresh key.
const refreshInterval = 2 * time.Second

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	hintStyle  = lipgloss.NewStyle().Faint(true)
	errStyle   = lipgloss.NewStyle().Bold(true)
)

// model is the Bubble Tea model backing the whole TUI. It holds the home
// screen's latest snapshot plus the guided web-setup wizard's state; only
// one of the two is ever shown at a time (step), but both are kept on the
// same model so returning from the wizard to Home needs no re-fetch.
type model struct {
	api core.API

	step step

	// home screen
	snap    core.DashboardView
	snapErr error
	loading bool

	// web setup wizard
	wiz        webSetupStep
	modeCursor int
	ans        webSetupAnswers
	listenIn   textinput.Model
	domainIn   textinput.Model
	rpidIn     textinput.Model
	originIn   textinput.Model
	applying   bool
	applyErr   error

	// management menu (schedule/quiet-hours/healthchecks/monitor thresholds/
	// channels)
	mgr manageModel

	// first-run onboarding (capture the Telegram bot token, then show the
	// enrollment pin and poll until enrolled -- see onboard_ui.go)
	onboard onboardModel

	quitting bool
}

// newModel builds the initial model: Home screen, nothing loaded yet, and
// the wizard's text inputs pre-built (but unfocused) so switching into the
// wizard never has to construct them mid-flow.
func newModel(api core.API) model {
	mk := func(placeholder string) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.CharLimit = 256
		ti.Width = 40
		return ti
	}
	return model{
		api:      api,
		step:     stepHome,
		loading:  true,
		listenIn: mk("127.0.0.1:8088"),
		domainIn: mk("example.com"),
		rpidIn:   mk("example.com"),
		originIn: mk("https://example.com"),
	}
}

// --- messages ---

// snapshotMsg carries the result of an api.Snapshot() call back into
// Update; err is non-nil when the control socket call failed (surfaced on
// the home screen rather than crashing the TUI).
type snapshotMsg struct {
	view core.DashboardView
	err  error
}

// tickMsg drives the home screen's periodic refresh.
type tickMsg time.Time

// webSetupAppliedMsg carries the result of applying the wizard's answers
// (fetch Config, set web.* fields, ApplyConfig) back into Update.
type webSetupAppliedMsg struct {
	err error
}

// --- commands ---

func fetchSnapshotCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		v, err := api.Snapshot()
		return snapshotMsg{view: v, err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// applyWebSetupCmd fetches the CURRENT config fresh from the daemon (so the
// wizard's changes layer onto whatever else is configured, never a stale
// snapshot from when the TUI started), applies ans onto it via
// applyWebSetup (setup_web.go), and posts the result with api.ApplyConfig.
// Any error, whether a local validation error from applyWebSetup or one the
// daemon returned from ApplyConfig, is surfaced identically to the caller
// as webSetupAppliedMsg.err -- the confirm screen doesn't need to know
// which step failed, only that nothing was applied.
func applyWebSetupCmd(api core.API, ans webSetupAnswers) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return webSetupAppliedMsg{err: fmt.Errorf("fetching current config: %w", err)}
		}
		if err := applyWebSetup(cfg, ans); err != nil {
			return webSetupAppliedMsg{err: err}
		}
		if err := api.ApplyConfig(cfg); err != nil {
			return webSetupAppliedMsg{err: err}
		}
		return webSetupAppliedMsg{}
	}
}

// --- tea.Model ---

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchSnapshotCmd(m.api), tickCmd(), fetchOnboardCheckCmd(m.api))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			m.quitting = true
			return m, tea.Quit
		}
		switch m.step {
		case stepHome:
			return m.updateHomeKey(msg)
		case stepSetupWeb:
			return m.updateSetupKey(msg)
		case stepManage:
			return m.updateManageKey(msg)
		case stepOnboard:
			return m.updateOnboardKey(msg)
		}
		return m, nil

	case snapshotMsg:
		m.loading = false
		m.snap = msg.view
		m.snapErr = msg.err
		return m, nil

	case tickMsg:
		if m.step == stepHome {
			return m, tea.Batch(fetchSnapshotCmd(m.api), tickCmd())
		}
		return m, tickCmd()

	case webSetupAppliedMsg:
		m.applying = false
		m.applyErr = msg.err
		m.wiz = webSetupResult
		return m, nil

	case manageAppliedMsg:
		m.mgr.applying = false
		m.mgr.applyErr = msg.err
		m.mgr.screen = manageResult
		return m, nil

	case scheduleConfigMsg:
		m.mgr.configLoading = false
		m.mgr.configErr = msg.err
		if msg.err == nil && msg.cfg != nil {
			m.mgr.schedAns.Daily = msg.cfg.Schedule.Daily
			m.mgr.schedAns.Weekly = msg.cfg.Schedule.Weekly
			switch {
			case msg.cfg.Schedule.Daily != "":
				m.mgr.schedModeCursor = 1 // "daily" in scheduleModeChoices
			case msg.cfg.Schedule.Weekly != "":
				m.mgr.schedModeCursor = 2 // "weekly" in scheduleModeChoices
			default:
				m.mgr.schedModeCursor = 0 // "off"
			}
		}
		return m, nil

	case quietHoursConfigMsg:
		m.mgr.configLoading = false
		m.mgr.configErr = msg.err
		m.mgr.valueIn = newManageValueInput("22-6 or off")
		if msg.err == nil {
			val := "off"
			if msg.cfg != nil && msg.cfg.QuietHours != "" {
				val = msg.cfg.QuietHours
			}
			m.mgr.valueIn.SetValue(val)
			m.mgr.valueIn.CursorEnd()
		}
		return m, m.mgr.valueIn.Focus()

	case healthchecksConfigMsg:
		m.mgr.configLoading = false
		m.mgr.configErr = msg.err
		m.mgr.valueIn = newManageValueInput("https://hc-ping.com/... or off")
		if msg.err == nil {
			val := "off"
			if msg.cfg != nil && msg.cfg.Healthchecks.URL != "" {
				val = msg.cfg.Healthchecks.URL
			}
			m.mgr.valueIn.SetValue(val)
			m.mgr.valueIn.CursorEnd()
		}
		return m, m.mgr.valueIn.Focus()

	case monitorTargetsMsg:
		m.mgr.monLoading = false
		m.mgr.monErr = msg.err
		if msg.err == nil {
			m.mgr.monTargets = msg.targets
			m.mgr.monRows = buildMonitorRows(msg.targets, msg.cfg)
			if m.mgr.monCursor >= len(m.mgr.monRows) {
				m.mgr.monCursor = 0
			}
		}
		return m, nil

	case monitorAppliedMsg:
		m.mgr.monErr = msg.err
		if msg.err == nil && msg.cfg != nil {
			m.mgr.monRows = buildMonitorRows(m.mgr.monTargets, msg.cfg)
		}
		return m, nil

	case channelsConfigMsg:
		m.mgr.configLoading = false
		m.mgr.configErr = msg.err
		if msg.err == nil && msg.cfg != nil {
			m.mgr.chanList = sortedChannels(msg.cfg)
			if m.mgr.chanCursor >= len(m.mgr.chanList) {
				m.mgr.chanCursor = 0
			}
		}
		return m, nil

	case channelActionMsg:
		m.mgr.chanErr = msg.err
		if msg.isTest {
			m.mgr.chanTestMsg = ""
			if msg.err == nil {
				m.mgr.chanTestMsg = fmt.Sprintf("test notification sent via %q", msg.name)
			}
			return m, nil
		}
		// remove
		if msg.err == nil && msg.cfg != nil {
			m.mgr.chanList = sortedChannels(msg.cfg)
			if m.mgr.chanCursor >= len(m.mgr.chanList) {
				m.mgr.chanCursor = len(m.mgr.chanList) - 1
			}
			if m.mgr.chanCursor < 0 {
				m.mgr.chanCursor = 0
			}
		}
		return m, nil

	case channelSavedMsg:
		m.mgr.chanSaving = false
		m.mgr.applyErr = msg.err
		m.mgr.screen = manageResult
		return m, nil

	case onboardCheckMsg:
		// Only auto-enter onboarding if the user is still sitting on Home:
		// by the time this lands (it's fetched alongside the snapshot/tick
		// in Init, so it can arrive after other keys), they may already have
		// navigated into the web wizard or the management menu, and forcing
		// them out into onboarding would be more annoying than helpful.
		if m.step == stepHome && needsOnboarding(msg.cfg) {
			m.step = stepOnboard
			m.onboard = onboardModel{tokenIn: newManageValueInput("bot token from @BotFather")}
			return m, m.onboard.tokenIn.Focus()
		}
		return m, nil

	case onboardTokenAppliedMsg:
		m.onboard.applying = false
		m.onboard.applyErr = msg.err
		if msg.err == nil {
			m.onboard.screen = onboardPINStep
			m.onboard.pinLoading = true
			return m, fetchOnboardPINCmd(m.api)
		}
		return m, nil

	case onboardPINMsg:
		m.onboard.pinLoading = false
		m.onboard.pinErr = msg.err
		if msg.err == nil {
			m.onboard.pin = msg.pin
			m.onboard.enrolled = msg.enrolled
		}
		if m.onboard.enrolled || msg.err != nil {
			return m, nil
		}
		return m, onboardPollTickCmd()

	case onboardPollTickMsg:
		if m.step == stepOnboard && m.onboard.screen == onboardPINStep && !m.onboard.enrolled {
			return m, fetchOnboardPINCmd(m.api)
		}
		return m, nil
	}
	return m, nil
}

// updateHomeKey handles a keypress on the Home screen: 's' launches the web
// setup wizard, 'm' opens the management menu (schedule/quiet-hours/
// healthchecks/monitor thresholds), 'r' forces an immediate Snapshot
// refresh, 'q' quits.
func (m model) updateHomeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		m.quitting = true
		return m, tea.Quit
	case "s":
		m.step = stepSetupWeb
		m.wiz = webSetupMode
		m.modeCursor = 0
		m.ans = webSetupAnswers{}
		m.applyErr = nil
		return m, nil
	case "m":
		m.step = stepManage
		m.mgr = manageModel{}
		return m, nil
	case "r":
		m.loading = true
		return m, fetchSnapshotCmd(m.api)
	}
	return m, nil
}

// updateSetupKey routes a keypress to whichever wizard screen is active.
func (m model) updateSetupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.wiz {
	case webSetupMode:
		return m.updateModeKey(msg)
	case webSetupListen, webSetupDomain, webSetupRPID, webSetupOrigin:
		return m.updateTextKey(msg)
	case webSetupConfirm:
		return m.updateConfirmKey(msg)
	case webSetupResult:
		// Any key returns Home; the wizard state resets on next entry.
		m.step = stepHome
		return m, nil
	}
	return m, nil
}

// updateModeKey handles the mode-selection screen: up/down (or j/k) moves
// the cursor over webModeChoices, enter commits the choice and derives a
// sensible default listen address for it (deriveWebDefaults), esc backs out
// to Home.
func (m model) updateModeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.modeCursor > 0 {
			m.modeCursor--
		}
	case "down", "j":
		if m.modeCursor < len(webModeChoices)-1 {
			m.modeCursor++
		}
	case "enter":
		m.ans.Mode = webModeChoices[m.modeCursor]
		m.ans.Listen = deriveWebDefaults(m.ans.Mode)
		m.listenIn.SetValue(m.ans.Listen)
		m.listenIn.CursorEnd()
		m.wiz = webSetupListen
		return m, m.listenIn.Focus()
	case "esc":
		m.step = stepHome
	}
	return m, nil
}

// updateTextKey feeds msg into whichever text step (listen/domain/rp_id/
// origin) m.wiz names, unless it is enter (commit via that step's onXDone)
// or esc (back to mode selection). It always mutates and returns THIS
// receiver copy's own field (m.listenIn, not some other copy's) and tail-
// calls the onXDone methods on the same copy for "enter", so the textinput's
// cursor/typed value and the wizard's step transitions never diverge across
// the value-receiver copies Go makes on each method call -- an earlier draft
// passed a *textinput.Model pointer into a *different* copy than the one
// ultimately returned, silently dropping every keystroke; this shape avoids
// that by keeping mutation and return on one copy throughout.
func (m model) updateTextKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		switch m.wiz {
		case webSetupListen:
			return m.onListenDone()
		case webSetupDomain:
			return m.onDomainDone()
		case webSetupRPID:
			return m.onRPIDDone()
		case webSetupOrigin:
			return m.onOriginDone()
		}
		return m, nil
	case "esc":
		m.wiz = webSetupMode
		return m, nil
	}
	var cmd tea.Cmd
	switch m.wiz {
	case webSetupListen:
		m.listenIn, cmd = m.listenIn.Update(msg)
	case webSetupDomain:
		m.domainIn, cmd = m.domainIn.Update(msg)
	case webSetupRPID:
		m.rpidIn, cmd = m.rpidIn.Update(msg)
	case webSetupOrigin:
		m.originIn, cmd = m.originIn.Update(msg)
	}
	return m, cmd
}

// onListenDone commits the listen-address step. proxy mode needs no public
// hostname (see needsDomain), so it skips straight to the confirm screen;
// autocert/manual continue on to the domain step.
func (m model) onListenDone() (tea.Model, tea.Cmd) {
	m.ans.Listen = m.listenIn.Value()
	if needsDomain(m.ans.Mode) {
		m.wiz = webSetupDomain
		return m, m.domainIn.Focus()
	}
	m.wiz = webSetupConfirm
	return m, nil
}

// onDomainDone commits the domain step and seeds rp_id/origin's defaults
// from it (deriveRPIDOrigin) before moving on so the following two screens
// open pre-filled rather than blank.
func (m model) onDomainDone() (tea.Model, tea.Cmd) {
	m.ans.Domain = m.domainIn.Value()
	rpid, origin := deriveRPIDOrigin(m.ans.Domain)
	m.ans.RPID, m.ans.Origin = rpid, origin
	m.rpidIn.SetValue(rpid)
	m.rpidIn.CursorEnd()
	m.originIn.SetValue(origin)
	m.wiz = webSetupRPID
	return m, m.rpidIn.Focus()
}

func (m model) onRPIDDone() (tea.Model, tea.Cmd) {
	m.ans.RPID = m.rpidIn.Value()
	m.wiz = webSetupOrigin
	return m, m.originIn.Focus()
}

func (m model) onOriginDone() (tea.Model, tea.Cmd) {
	m.ans.Origin = m.originIn.Value()
	m.wiz = webSetupConfirm
	return m, nil
}

// updateConfirmKey handles the review screen: enter/'y' applies (fetch,
// merge, ApplyConfig, all in applyWebSetupCmd so it runs off the UI
// goroutine), 'n'/esc discards the wizard and returns to Home.
func (m model) updateConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", "y":
		m.applying = true
		return m, applyWebSetupCmd(m.api, m.ans)
	case "n", "esc":
		m.step = stepHome
	}
	return m, nil
}

// --- View ---

func (m model) View() string {
	if m.quitting {
		return ""
	}
	switch m.step {
	case stepSetupWeb:
		return m.setupView()
	case stepManage:
		return m.manageView()
	case stepOnboard:
		return m.onboardView()
	default:
		return m.homeView()
	}
}

func (m model) homeView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("serverwatch-ctl") + "\n\n")
	if m.loading {
		b.WriteString("loading...\n")
	} else if m.snapErr != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("snapshot error: %v", m.snapErr)) + "\n")
	} else {
		online := "offline"
		if m.snap.Online {
			online = "online"
		}
		fmt.Fprintf(&b, "as of:    %s\n", formatTime(m.snap.TS))
		fmt.Fprintf(&b, "internet: %s\n", online)
		fmt.Fprintf(&b, "cpu:      %.1f%% (%d cores)\n", m.snap.CPU, m.snap.Cores)
		fmt.Fprintf(&b, "memory:   %.1f%%\n", m.snap.MemPct)
		fmt.Fprintf(&b, "load:     %.2f %.2f %.2f\n", m.snap.Load1, m.snap.Load5, m.snap.Load15)
		fmt.Fprintf(&b, "units:    %d failed / %d total\n", m.snap.UnitsFailed, m.snap.UnitsTotal)
	}
	b.WriteString("\n" + hintStyle.Render("s: set up the web UI   m: manage   r: refresh   q: quit") + "\n")
	return b.String()
}

func (m model) setupView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("set up the web UI") + "\n\n")
	switch m.wiz {
	case webSetupMode:
		b.WriteString("serving mode:\n\n")
		for i, choice := range webModeChoices {
			cursor := "  "
			if i == m.modeCursor {
				cursor = "> "
			}
			fmt.Fprintf(&b, "%s%s\n", cursor, choice)
		}
		b.WriteString("\n" + hintStyle.Render("up/down to choose, enter to select, esc to cancel") + "\n")
	case webSetupListen:
		fmt.Fprintf(&b, "listen address (host:port):\n\n%s\n", m.listenIn.View())
		b.WriteString("\n" + hintStyle.Render("enter to continue, esc to go back") + "\n")
	case webSetupDomain:
		fmt.Fprintf(&b, "public domain (used to derive rp_id/origin):\n\n%s\n", m.domainIn.View())
		b.WriteString("\n" + hintStyle.Render("enter to continue, esc to go back") + "\n")
	case webSetupRPID:
		fmt.Fprintf(&b, "webauthn rp_id (relying party id):\n\n%s\n", m.rpidIn.View())
		b.WriteString("\n" + hintStyle.Render("enter to continue, esc to go back") + "\n")
	case webSetupOrigin:
		fmt.Fprintf(&b, "public origin (scheme + host[:port]):\n\n%s\n", m.originIn.View())
		b.WriteString("\n" + hintStyle.Render("enter to continue, esc to go back") + "\n")
	case webSetupConfirm:
		b.WriteString("review:\n\n")
		b.WriteString(webSetupSummary(m.ans))
		b.WriteString("\nnote: enabling the web listener needs a daemon restart today; ")
		b.WriteString("applying saves config.json and hot-reloads everything else now.\n")
		if m.applying {
			b.WriteString("\napplying...\n")
		} else {
			b.WriteString("\n" + hintStyle.Render("enter/y to apply, n/esc to cancel") + "\n")
		}
	case webSetupResult:
		if m.applyErr != nil {
			b.WriteString(errStyle.Render(fmt.Sprintf("apply failed: %v", m.applyErr)) + "\n")
		} else {
			b.WriteString("applied. remember to restart the daemon to bind the web listener.\n")
		}
		b.WriteString("\n" + hintStyle.Render("press any key to return home") + "\n")
	}
	return b.String()
}

// runInteractive launches the Bubble Tea program against api and blocks
// until the user quits. It writes a one-line error to errOut and returns a
// non-zero exit code if the terminal program itself fails to run (e.g. not
// attached to a tty); a normal quit returns 0.
func runInteractive(api core.API, errOut io.Writer) int {
	p := tea.NewProgram(newModel(api))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(errOut, "serverwatch-ctl: tui: %v\n", err)
		return 1
	}
	return 0
}
