// Interactive management TUI for trinetra-ctl, built on Bubble Tea
// (github.com/charmbracelet/bubbletea).
package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// step is the TUI's top level screen.
type step int

const (
	stepHome step = iota
	stepSetupWeb
	stepManage
	stepOnboard
)

// refreshInterval is how often the home screen re-fetches Snapshot() while idle, so "live
// status" actually stays live without the user pressing a refresh key.
const refreshInterval = 2 * time.Second

// cpuHistCap bounds the rolling CPU history the home sparkline draws from: at
// refreshInterval each, this is the last ~80s of samples.
const cpuHistCap = 40

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	hintStyle  = lipgloss.NewStyle().Faint(true)
	errStyle   = lipgloss.NewStyle().Bold(true)
)

// model is the Bubble Tea model backing the whole TUI.
type model struct {
	api core.API

	step step

	// home screen
	snap    core.DashboardView
	snapErr error
	loading bool
	// alerts holds the latest ActiveAlerts() fetch, refreshed alongside the snapshot on Home
	// so the ALERTS panel stays live.
	alerts    []core.AlertRecord
	alertsErr error
	// cpuHist is the rolling CPU% history the home sparkline draws, capped at
	// cpuHistCap and appended on each snapshotMsg.
	cpuHist []float64
	// help toggles the global keymap overlay (opened with '?' from Home,
	// dismissed by any key).
	help bool
	// serverName is the host's display name (config server.name, or the hostname when unset).
	serverName string

	// web setup wizard
	wiz        webSetupStep
	modeCursor int
	ans        webSetupAnswers
	listenIn   textinput.Model
	domainIn   textinput.Model
	rpidIn     textinput.Model
	originIn   textinput.Model
	certIn     textinput.Model // manual mode only (webSetupCert)
	keyIn      textinput.Model // manual mode only (webSetupKey)
	applying   bool
	applyErr   error
	// wizFieldErr is the cert/key steps' own must-not-be-empty guard message
	// (validateManualPath): set when "enter" is pressed on a blank value.
	wizFieldErr error

	// management menu (schedule/quiet-hours/healthchecks/monitor thresholds/ channels)
	mgr manageModel

	// first-run onboarding (capture the Telegram bot token, then show the
	// enrollment pin and poll until enrolled -- see onboard_ui.go)
	onboard onboardModel

	quitting bool
}

// newModel builds the initial model: Home screen, nothing loaded yet, and the wizard's text
// inputs pre-built.
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
		certIn:   mk("/etc/trinetra/tls/cert.pem"),
		keyIn:    mk("/etc/trinetra/tls/key.pem"),
	}
}

// --- messages ---

// snapshotMsg carries the result of an api.Snapshot() call back into Update; err is non-nil
// when the control socket call failed.
type snapshotMsg struct {
	view core.DashboardView
	err  error
}

// tickMsg drives the home screen's periodic refresh.
type tickMsg time.Time

// alertsMsg carries the result of an api.ActiveAlerts() call back into Update; err is
// non-nil when the control socket call failed.
type alertsMsg struct {
	alerts []core.AlertRecord
	err    error
}

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

func fetchAlertsCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		a, err := api.ActiveAlerts()
		return alertsMsg{alerts: a, err: err}
	}
}

// applyWebSetupCmd fetches the CURRENT config fresh from the daemon.
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
	return tea.Batch(fetchSnapshotCmd(m.api), fetchAlertsCmd(m.api), tickCmd(), fetchOnboardCheckCmd(m.api))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			m.quitting = true
			return m, tea.Quit
		}
		// The help overlay swallows the next keypress to dismiss itself, so it never interferes
		// with the screen underneath. '?' opens it from Home only.
		if m.help {
			m.help = false
			return m, nil
		}
		if msg.String() == "?" && m.step == stepHome {
			m.help = true
			return m, nil
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
		if msg.err == nil {
			m.cpuHist = append(m.cpuHist, msg.view.CPU)
			if len(m.cpuHist) > cpuHistCap {
				m.cpuHist = m.cpuHist[len(m.cpuHist)-cpuHistCap:]
			}
		}
		return m, nil

	case alertsMsg:
		m.alerts = msg.alerts
		m.alertsErr = msg.err
		return m, nil

	case tickMsg:
		if m.step == stepHome {
			return m, tea.Batch(fetchSnapshotCmd(m.api), fetchAlertsCmd(m.api), tickCmd())
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

	case settingsConfigMsg:
		m.mgr.setLoading = false
		m.mgr.setErr = msg.err
		if msg.err == nil {
			m.mgr.setCfg = msg.cfg
			m.mgr.setGroups = settingsGroups()
		}
		return m, nil

	case settingsAppliedMsg:
		m.mgr.applying = false
		m.mgr.applyErr = msg.err
		m.mgr.screen = manageResult
		return m, nil

	case onboardCheckMsg:
		// The onboarding check's Config() fetch (Init) is also where we learn
		// the host's display name for the Home header (#101).
		if msg.cfg != nil {
			m.serverName = msg.cfg.ServerName()
		}
		// Only auto-enter onboarding if the user is still sitting on Home: by the time this lands
		// (it's fetched alongside the snapshot/tick in Init, so it can arrive after other keys).
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

// updateHomeKey handles a keypress on the Home screen: 's' launches the web setup wizard,
// 'm' opens the management menu (schedule/quiet-hours/ healthchecks/monitor thresholds).
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
		m.wizFieldErr = nil
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
	case webSetupListen, webSetupDomain, webSetupRPID, webSetupOrigin, webSetupCert, webSetupKey:
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

// updateModeKey handles the mode-selection screen: up/down (or j/k) moves the cursor over
// webModeChoices.
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

// updateTextKey feeds msg into whichever text step (listen/domain/rp_id/ origin) m.wiz
// names, unless it is enter (commit via that step's onXDone) or esc.
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
		case webSetupCert:
			return m.onCertDone()
		case webSetupKey:
			return m.onKeyDone()
		}
		return m, nil
	case "esc":
		m.wiz = webSetupMode
		m.wizFieldErr = nil
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
	case webSetupCert:
		m.certIn, cmd = m.certIn.Update(msg)
	case webSetupKey:
		m.keyIn, cmd = m.keyIn.Update(msg)
	}
	return m, cmd
}

// onListenDone commits the listen-address step and advances to the domain step.
func (m model) onListenDone() (tea.Model, tea.Cmd) {
	m.ans.Listen = m.listenIn.Value()
	m.wiz = webSetupDomain
	return m, m.domainIn.Focus()
}

// onDomainDone commits the domain step and seeds rp_id/origin's defaults from it
// (deriveRPIDOrigin) before moving on so the following two screens open pre-filled.
func (m model) onDomainDone() (tea.Model, tea.Cmd) {
	m.ans.Domain = strings.TrimSpace(m.domainIn.Value())
	if m.ans.Mode == "proxy" && m.ans.Domain == "" {
		m.ans.RPID, m.ans.Origin = "", ""
		m.wiz = webSetupConfirm
		return m, nil
	}
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

// onOriginDone commits the origin step. manual mode still needs its TLS cert/key file paths
// (internal/web's manual serving mode refuses to start without both).
func (m model) onOriginDone() (tea.Model, tea.Cmd) {
	m.ans.Origin = m.originIn.Value()
	if m.ans.Mode == "manual" {
		m.wiz = webSetupCert
		return m, m.certIn.Focus()
	}
	m.wiz = webSetupConfirm
	return m, nil
}

// onCertDone commits the cert-path step.
func (m model) onCertDone() (tea.Model, tea.Cmd) {
	val := m.certIn.Value()
	if err := validateManualPath("cert path", val); err != nil {
		m.wizFieldErr = err
		return m, nil
	}
	m.ans.TLSCert = val
	m.wizFieldErr = nil
	m.wiz = webSetupKey
	return m, m.keyIn.Focus()
}

// onKeyDone is onCertDone's counterpart for the key-path step, the last
// step before confirm in manual mode.
func (m model) onKeyDone() (tea.Model, tea.Cmd) {
	val := m.keyIn.Value()
	if err := validateManualPath("key path", val); err != nil {
		m.wizFieldErr = err
		return m, nil
	}
	m.ans.TLSKey = val
	m.wizFieldErr = nil
	m.wiz = webSetupConfirm
	return m, nil
}

// updateConfirmKey handles the review screen: enter/'y' applies (fetch, merge, ApplyConfig,
// all in applyWebSetupCmd so it runs off the UI goroutine).
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
	if m.help {
		return m.helpView()
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

// homeView renders the live dashboard: a status header, two side-by-side panels (SYSTEM
// meters + CPU sparkline, and the ALERTS list), an inventory line, and the key hints.
func (m model) homeView() string {
	var b strings.Builder
	b.WriteString(m.homeHeader() + "\n\n")
	if m.loading {
		b.WriteString(faintStyle.Render("loading live status...") + "\n")
		return b.String()
	}
	if m.snapErr != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("snapshot error: %v", m.snapErr)) + "\n")
		b.WriteString("\n" + m.homeHints() + "\n")
		return b.String()
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.systemPanel(), "  ", m.alertsPanel())
	b.WriteString(body + "\n")
	if d := m.disksPanel(); d != "" {
		b.WriteString(d + "\n")
	}
	if s := m.availabilityStrip(); s != "" {
		b.WriteString(s + "\n")
	}
	b.WriteString(m.inventoryLine() + "\n\n")
	b.WriteString(m.homeHints() + "\n")
	return b.String()
}

// homeHeader is the "trinetra  ● online   updated 3s ago" status line.
func (m model) homeHeader() string {
	glyph, text, style := onlineGlyph(m.snap.Online)
	head := titleStyle.Render("trinetra")
	if m.serverName != "" {
		head += faintStyle.Render(" · " + m.serverName)
	}
	head += "  " + style.Render(glyph+" "+text)
	if !m.loading && m.snap.TS != 0 {
		head += faintStyle.Render("   updated " + agoString(m.snap.TS))
	}
	return head
}

func (m model) homeHints() string {
	return faintStyle.Render("s setup   m manage   r refresh   ? help   q quit")
}

// systemPanel is the boxed SYSTEM panel: coloured CPU/MEM/SWAP meter bars, the
// live CPU sparkline, load averages, and (when present) temperature.
func (m model) systemPanel() string {
	const w = 12
	meter := func(pct float64) string {
		return meterStyle(pct).Render(bar(pct, w))
	}
	var b strings.Builder
	b.WriteString(panelTitleStyle.Render("SYSTEM") + "\n")
	fmt.Fprintf(&b, "%s %s %s  %s\n", labelStyle.Render("CPU "), meter(m.snap.CPU),
		pctText(m.snap.CPU), signalStyle.Render(spark(m.cpuHist, 16)))
	fmt.Fprintf(&b, "%s %s %s  %s\n", labelStyle.Render("MEM "), meter(m.snap.MemPct),
		pctText(m.snap.MemPct), faintStyle.Render(fmt.Sprintf("%d cores", m.snap.Cores)))
	fmt.Fprintf(&b, "%s %s %s\n", labelStyle.Render("SWAP"), meter(m.snap.SwapPct), pctText(m.snap.SwapPct))
	fmt.Fprintf(&b, "%s %.2f %.2f %.2f\n", labelStyle.Render("LOAD"),
		m.snap.Load1, m.snap.Load5, m.snap.Load15)
	if m.snap.TempC != 0 {
		fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("TEMP"),
			meterStyle(m.snap.TempC).Render(fmt.Sprintf("%.1f°C", m.snap.TempC)))
	}
	fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("NET "),
		faintStyle.Render("↓"+humanRate(m.snap.NetRxBps)+"  ↑"+humanRate(m.snap.NetTxBps)))
	return panelStyle.Render(strings.TrimRight(b.String(), "\n"))
}

// disksPanel is the boxed DISKS panel: the top mounts by usage, each with a coloured usage
// bar.
func (m model) disksPanel() string {
	if len(m.snap.Disks) == 0 {
		return ""
	}
	ds := append([]core.DiskView(nil), m.snap.Disks...)
	sort.Slice(ds, func(i, j int) bool { return ds[i].UsagePct > ds[j].UsagePct })
	var b strings.Builder
	b.WriteString(panelTitleStyle.Render("DISKS") + "\n")
	for i, d := range ds {
		if i >= 5 {
			fmt.Fprintf(&b, "%s\n", faintStyle.Render(fmt.Sprintf("+%d more mounts", len(ds)-5)))
			break
		}
		mount := d.Mount
		if mount == "" {
			mount = d.Device
		}
		fmt.Fprintf(&b, "%s %s %s\n", labelStyle.Render(fmt.Sprintf("%-16s", trunc(mount, 16))),
			meterStyle(d.UsagePct).Render(bar(d.UsagePct, 10)), pctText(d.UsagePct))
	}
	return panelStyle.Render(strings.TrimRight(b.String(), "\n"))
}

// availabilityStrip renders the 24h up/down blocks (green up, red down) plus the uptime %,
// total downtime, and incident count.
func (m model) availabilityStrip() string {
	av := m.snap.Availability
	if len(av.Blocks) == 0 {
		return ""
	}
	var blocks strings.Builder
	for _, blk := range av.Blocks {
		if blk.Down {
			blocks.WriteString(critStyle.Render("▇"))
		} else {
			blocks.WriteString(okStyle.Render("▇"))
		}
	}
	meta := faintStyle.Render(fmt.Sprintf("  uptime %.2f%%   %s down   %s",
		av.UptimePct, av.DowntimeStr, av.IncidentsLabel))
	return labelStyle.Render("24h ") + blocks.String() + meta
}

// alertsPanel is the boxed ALERTS panel: a firing count and the top few
// active alerts (severity dot + truncated key), or a reassuring empty state.
func (m model) alertsPanel() string {
	const maxRows = 6
	var b strings.Builder
	if m.alertsErr != nil {
		b.WriteString(panelTitleStyle.Render("ALERTS") + "\n")
		b.WriteString(critStyle.Render("fetch failed"))
		return panelStyle.Render(b.String())
	}
	if len(m.alerts) == 0 {
		b.WriteString(panelTitleStyle.Render("ALERTS") + "\n")
		b.WriteString(okStyle.Render("✓ ") + "no active alerts")
		return panelStyle.Render(b.String())
	}
	b.WriteString(panelTitleStyle.Render("ALERTS") + "  " +
		critStyle.Render(fmt.Sprintf("%d firing", len(m.alerts))) + "\n")
	for i, a := range m.alerts {
		if i >= maxRows {
			fmt.Fprintf(&b, "%s\n", faintStyle.Render(fmt.Sprintf("+%d more", len(m.alerts)-maxRows)))
			break
		}
		ack := ""
		if a.Acked {
			ack = faintStyle.Render(" (acked)")
		}
		fmt.Fprintf(&b, "%s %s%s\n", severityGlyph(a.Severity), trunc(a.Key, 26), ack)
	}
	return panelStyle.Render(strings.TrimRight(b.String(), "\n"))
}

// inventoryLine summarizes the host inventory in one glyphed row.
func (m model) inventoryLine() string {
	cont := fmt.Sprintf("🐳 %d/%d up", m.snap.ContainersRunning, m.snap.ContainersTotal)
	units := fmt.Sprintf("⚙ %d failed / %d units", m.snap.UnitsFailed, m.snap.UnitsTotal)
	disks := fmt.Sprintf("💾 %d mounts", len(m.snap.Disks))
	if m.snap.DisksCritical > 0 {
		disks += critStyle.Render(fmt.Sprintf(" (%d crit)", m.snap.DisksCritical))
	}
	procs := fmt.Sprintf("▤ %d procs", m.snap.Processes.Total)
	sep := dimStyle.Render("   ")
	return cont + sep + units + sep + disks + sep + procs
}

// helpView is the global keymap overlay ('?').
func (m model) helpView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("trinetra-ctl · keys") + "\n\n")
	section := func(title string, rows [][2]string) {
		b.WriteString(panelTitleStyle.Render(title) + "\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "  %s  %s\n", signalStyle.Render(fmt.Sprintf("%-8s", r[0])), r[1])
		}
		b.WriteString("\n")
	}
	section("Home", [][2]string{
		{"s", "set up the web UI (guided wizard)"},
		{"m", "manage config (schedule, channels, thresholds, all settings)"},
		{"r", "refresh live status now"},
		{"?", "toggle this help"},
		{"q", "quit"},
	})
	section("Menus", [][2]string{
		{"↑/↓ j/k", "move"},
		{"enter", "open / confirm"},
		{"esc", "back"},
	})
	section("Global", [][2]string{
		{"ctrl-c", "quit from anywhere"},
	})
	b.WriteString(faintStyle.Render("press any key to close") + "\n")
	return b.String()
}

// agoString renders how long ago a Unix-seconds timestamp was, compactly.
func agoString(ts int64) string {
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// pctText renders a percentage right-aligned to a stable width so the meter
// column and the value column both line up regardless of magnitude.
func pctText(pct float64) string {
	return fmt.Sprintf("%5.1f%%", pct)
}

func (m model) setupView() string {
	var b strings.Builder
	b.WriteString(breadcrumb("Web setup") + "\n\n")
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
		if m.ans.Mode == "proxy" {
			b.WriteString("\n" + hintStyle.Render("optional in proxy mode: leave blank to derive rp_id/origin from your reverse proxy's forwarded headers") + "\n")
		}
		b.WriteString("\n" + hintStyle.Render("enter to continue, esc to go back") + "\n")
	case webSetupRPID:
		fmt.Fprintf(&b, "webauthn rp_id (relying party id):\n\n%s\n", m.rpidIn.View())
		b.WriteString("\n" + hintStyle.Render("enter to continue, esc to go back") + "\n")
	case webSetupOrigin:
		fmt.Fprintf(&b, "public origin (scheme + host[:port]):\n\n%s\n", m.originIn.View())
		b.WriteString("\n" + hintStyle.Render("enter to continue, esc to go back") + "\n")
	case webSetupCert:
		fmt.Fprintf(&b, "TLS certificate file path (PEM):\n\n%s\n", m.certIn.View())
		if m.wizFieldErr != nil {
			b.WriteString("\n" + errStyle.Render(m.wizFieldErr.Error()) + "\n")
		}
		b.WriteString("\n" + hintStyle.Render("enter to continue, esc to go back") + "\n")
	case webSetupKey:
		fmt.Fprintf(&b, "TLS private key file path (PEM):\n\n%s\n", m.keyIn.View())
		if m.wizFieldErr != nil {
			b.WriteString("\n" + errStyle.Render(m.wizFieldErr.Error()) + "\n")
		}
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

// runInteractive launches the Bubble Tea program against api and blocks until the user
// quits.
func runInteractive(api core.API, errOut io.Writer) int {
	p := tea.NewProgram(newModel(api))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(errOut, "trinetra-ctl: tui: %v\n", err)
		return 1
	}
	return 0
}
