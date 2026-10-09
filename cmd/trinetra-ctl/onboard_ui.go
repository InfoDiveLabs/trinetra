// onboard_ui.go is the first-run onboarding flow's Bubble Tea glue: whether
// to show it (needsOnboarding, checked once against a fresh Config() fetch
// right after Init) and the two screens it walks -- capture the Telegram
// bot token, then show the enrollment pin (api.EnrollmentPIN, Task 1's
// #90 work) with the "/start <pin>" instruction, polling until enrolled.
// The pure decision (needsOnboarding) and config mutation (applyOnboardToken)
// this drives live in onboarding.go, unit-tested there against a fake
// core.API with no terminal involved; this file is deliberately thin,
// mirroring tui.go's own split for the web-setup wizard.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// onboardScreen is the onboarding flow's own sub-step, mirroring
// webSetupStep's role for the web-setup wizard.
type onboardScreen int

const (
	onboardTokenStep onboardScreen = iota
	onboardPINStep
)

// onboardPollInterval is how often the pin screen re-fetches api.EnrollmentPIN while
// waiting for the user to message the bot.
const onboardPollInterval = 2 * time.Second

// onboardModel holds the onboarding flow's state across both screens.
type onboardModel struct {
	screen onboardScreen

	// token step
	tokenIn  textinput.Model
	applying bool
	applyErr error

	// pin step
	pinLoading bool
	pin        string
	enrolled   bool
	pinErr     error
}

// --- messages ---

// onboardCheckMsg carries the result of the ONE Config() fetch Init issues
// (fetchOnboardCheckCmd) to decide whether to auto-enter onboarding at all.
type onboardCheckMsg struct {
	cfg *config.Config
	err error
}

// onboardTokenAppliedMsg carries the result of applying the captured token
// (fetch Config, set telegram.token, ApplyConfig) back into Update.
type onboardTokenAppliedMsg struct {
	err error
}

// onboardPINMsg carries the result of an api.EnrollmentPIN call back into Update, for both
// the first fetch right after the token is saved and every later poll.
type onboardPINMsg struct {
	pin      string
	enrolled bool
	err      error
}

// onboardPollTickMsg drives the pin screen's periodic re-fetch.
type onboardPollTickMsg time.Time

// --- commands ---

// fetchOnboardCheckCmd fetches Config once, right after Init, so Update can decide whether
// to auto-enter onboarding (needsOnboarding, onboarding.go).
func fetchOnboardCheckCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		return onboardCheckMsg{cfg: cfg, err: err}
	}
}

// applyOnboardTokenCmd fetches Config fresh, sets telegram.token via applyOnboardToken
// (onboarding.go), and posts it with ApplyConfig.
func applyOnboardTokenCmd(api core.API, token string) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err != nil {
			return onboardTokenAppliedMsg{err: fmt.Errorf("fetching current config: %w", err)}
		}
		if err := applyOnboardToken(cfg, token); err != nil {
			return onboardTokenAppliedMsg{err: err}
		}
		if err := api.ApplyConfig(cfg); err != nil {
			return onboardTokenAppliedMsg{err: err}
		}
		return onboardTokenAppliedMsg{}
	}
}

// fetchOnboardPINCmd calls api.EnrollmentPIN (Task 1, #90) -- the exact pin the daemon's
// poll loop accepts via "/start <pin>".
func fetchOnboardPINCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		pin, enrolled, err := api.EnrollmentPIN(context.Background())
		return onboardPINMsg{pin: pin, enrolled: enrolled, err: err}
	}
}

// onboardPollTickCmd schedules the next pin re-fetch onboardPollInterval from now.
func onboardPollTickCmd() tea.Cmd {
	return tea.Tick(onboardPollInterval, func(t time.Time) tea.Msg { return onboardPollTickMsg(t) })
}

// --- Update ---

// updateOnboardKey routes a keypress to whichever onboarding screen is
// active, mirroring updateSetupKey's role for the web-setup wizard.
func (m model) updateOnboardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.onboard.screen {
	case onboardTokenStep:
		return m.updateOnboardTokenKey(msg)
	case onboardPINStep:
		return m.updateOnboardPINKey(msg)
	}
	return m, nil
}

// updateOnboardTokenKey feeds msg into the token input: enter applies it
// (fetch, set telegram.token, ApplyConfig, all off the UI goroutine via
// applyOnboardTokenCmd) once non-empty, esc skips onboarding for now and
// returns to Home (the user can always set the token later via the
// Channels screen or `trinetra telegram set-token`).
func (m model) updateOnboardTokenKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		token := m.onboard.tokenIn.Value()
		if token == "" {
			return m, nil
		}
		m.onboard.applying = true
		m.onboard.applyErr = nil
		return m, applyOnboardTokenCmd(m.api, token)
	case "esc":
		m.step = stepHome
		return m, nil
	}
	var cmd tea.Cmd
	m.onboard.tokenIn, cmd = m.onboard.tokenIn.Update(msg)
	return m, cmd
}

// updateOnboardPINKey handles the pin screen: esc leaves onboarding at any point (the token
// is already saved by the time this screen shows, so leaving here never loses that).
func (m model) updateOnboardPINKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.step = stepHome
		return m, nil
	}
	if m.onboard.enrolled {
		m.step = stepHome
	}
	return m, nil
}

// --- View ---

// onboardView renders whichever onboarding screen is active.
func (m model) onboardView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("welcome to trinetra") + "\n\n")
	switch m.onboard.screen {
	case onboardTokenStep:
		b.WriteString("let's connect Telegram so trinetra can alert you.\n\n")
		fmt.Fprintf(&b, "bot token (from @BotFather):\n\n%s\n", m.onboard.tokenIn.View())
		if m.onboard.applyErr != nil {
			b.WriteString("\n" + errStyle.Render(fmt.Sprintf("could not save the token: %v", m.onboard.applyErr)) + "\n")
		}
		if m.onboard.applying {
			b.WriteString("\nsaving...\n")
		} else {
			b.WriteString("\n" + hintStyle.Render("enter to continue, esc to skip for now") + "\n")
		}
	case onboardPINStep:
		switch {
		case m.onboard.pinLoading:
			b.WriteString("fetching your enrollment pin...\n")
		case m.onboard.pinErr != nil:
			b.WriteString(errStyle.Render(fmt.Sprintf("could not fetch the enrollment pin: %v", m.onboard.pinErr)) + "\n")
			b.WriteString("\n" + hintStyle.Render("the daemon will log it on start: journalctl -u trinetra | grep /start") + "\n")
		case m.onboard.enrolled:
			b.WriteString("enrolled! trinetra can now message you on Telegram.\n")
			b.WriteString("\n" + hintStyle.Render("press any key to continue") + "\n")
		default:
			b.WriteString("token saved. to finish enrollment, from your Telegram account message the bot:\n\n")
			fmt.Fprintf(&b, "  /start %s\n", m.onboard.pin)
			b.WriteString("\n" + hintStyle.Render("waiting for enrollment... esc to continue later") + "\n")
		}
	}
	return b.String()
}
