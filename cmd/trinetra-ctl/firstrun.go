package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// needsFirstRun is true only for a server nobody has set up yet. Upgraded
// servers that already have Telegram enrolled, an enabled channel, or a web
// user never see the flow, even without setup.completed.
func needsFirstRun(cfg *config.Config, webUsers int) bool {
	if cfg == nil || cfg.Setup.Completed || cfg.Telegram.ChatID != "" || webUsers > 0 {
		return false
	}
	return !anyChannelEnabled(cfg)
}

func anyChannelEnabled(cfg *config.Config) bool {
	for _, ch := range cfg.Channels {
		if ch.Enabled {
			return true
		}
	}
	return false
}

// Seams so tests don't exec the web plugin.
var (
	webUsersCountFn = webUsersCount
	inviteAdminFn   = inviteAdmin
)

// webBinary prefers the trinetra-web installed next to this binary. The
// child inherits this process's environment, including the control socket
// the front door handed us.
func webBinary() string {
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), "trinetra-web")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "trinetra-web"
}

func webUsersCount() (int, error) {
	out, err := exec.Command(webBinary(), "users", "list", "--json").Output()
	if err != nil {
		return 0, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

func inviteAdmin() (string, error) {
	out, err := exec.Command(webBinary(), "users", "invite", "--role", "admin", "--json").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", errors.New(string(ee.Stderr))
		}
		return "", err
	}
	var res struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &res); err != nil || res.URL == "" {
		return "", errors.New("trinetra-web returned no enroll link")
	}
	return res.URL, nil
}

type setupCompletedMsg struct{ err error }

func applySetupCompletedCmd(api core.API) tea.Cmd {
	return func() tea.Msg {
		cfg, err := api.Config()
		if err == nil {
			err = cfg.Set("setup.completed", "true")
		}
		if err == nil {
			err = api.ApplyConfig(cfg)
		}
		return setupCompletedMsg{err: err}
	}
}
