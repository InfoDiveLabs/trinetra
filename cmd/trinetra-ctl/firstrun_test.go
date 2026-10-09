package main

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestNeedsFirstRun(t *testing.T) {
	if !needsFirstRun(config.Default(), 0) {
		t.Fatal("fresh install should get the first run")
	}
	if needsFirstRun(nil, 0) {
		t.Fatal("unknown config (daemon unreachable) should not start the first run")
	}
	for name, mut := range map[string]func(*config.Config){
		"completed": func(c *config.Config) { c.Setup.Completed = true },
		"telegram":  func(c *config.Config) { c.Telegram.Token, c.Telegram.ChatID = "t", "1" },
		"channel":   func(c *config.Config) { c.Channels = []config.ChannelConfig{{Name: "x", Type: "slack", Enabled: true}} },
	} {
		c := config.Default()
		mut(c)
		if needsFirstRun(c, 0) {
			t.Errorf("%s: first run shown on an already set-up server", name)
		}
	}
	if needsFirstRun(config.Default(), 1) {
		t.Error("web users exist: first run shown")
	}
}

func stubWeb(t *testing.T, users int, url string, err error) {
	t.Helper()
	pc, pi := webUsersCountFn, inviteAdminFn
	webUsersCountFn = func() (int, error) { return users, nil }
	inviteAdminFn = func() (string, error) { return url, err }
	t.Cleanup(func() { webUsersCountFn, inviteAdminFn = pc, pi })
}

func press(t *testing.T, m tea.Model, k tea.KeyMsg) (tea.Model, tea.Cmd) {
	t.Helper()
	return m.Update(k)
}

func TestFirstRunSkipEverything(t *testing.T) {
	stubWeb(t, 0, "https://ops.example.com/enroll?token=abc", nil)
	api := &fakeAPI{cfg: config.Default()}
	var mm tea.Model = newModel(api).startFirstRun()
	mm, _ = press(t, mm, keyType(tea.KeyEnter)) // welcome -> web
	if mm.(model).firstRun.screen != frWeb {
		t.Fatalf("screen %v, want frWeb", mm.(model).firstRun.screen)
	}
	mm, cmd := press(t, mm, keyType(tea.KeyEsc)) // skip web -> admin, invite starts
	if mm.(model).firstRun.screen != frAdmin || cmd == nil {
		t.Fatal("skipping the web UI should go to the admin step and create a link")
	}
	mm, _ = mm.Update(runCmd(t, cmd))
	if !strings.Contains(mm.View(), "https://ops.example.com/enroll?token=abc") {
		t.Fatalf("admin step does not show the link:\n%s", mm.View())
	}
	mm, _ = press(t, mm, keyType(tea.KeyEnter)) // admin -> alerts
	mm, _ = press(t, mm, keyType(tea.KeyEsc))   // later -> done
	if mm.(model).firstRun.screen != frDone {
		t.Fatalf("screen %v, want frDone", mm.(model).firstRun.screen)
	}
	mm, cmd = press(t, mm, keyType(tea.KeyEnter))
	if mm.(model).step != stepHome || cmd == nil {
		t.Fatal("finishing must return Home and save setup.completed")
	}
	if msg := runCmd(t, cmd); msg.(setupCompletedMsg).err != nil || api.applied == nil || !api.applied.Setup.Completed {
		t.Fatalf("setup.completed not applied: %+v", msg)
	}
}

func TestFirstRunEscOnWelcomeSkipsSetup(t *testing.T) {
	api := &fakeAPI{cfg: config.Default()}
	var mm tea.Model = newModel(api).startFirstRun()
	mm, cmd := press(t, mm, keyType(tea.KeyEsc))
	if mm.(model).step != stepHome || cmd == nil {
		t.Fatal("esc on welcome should skip to Home")
	}
	runCmd(t, cmd)
	if api.applied == nil || !api.applied.Setup.Completed {
		t.Fatal("skipping must still mark setup completed so it isn't shown again")
	}
}

func TestFirstRunAdminSkippedWhenUsersExist(t *testing.T) {
	stubWeb(t, 2, "", nil)
	var mm tea.Model = newModel(&fakeAPI{cfg: config.Default()}).startFirstRun()
	mm, _ = press(t, mm, keyType(tea.KeyEnter))
	mm, cmd := press(t, mm, keyType(tea.KeyEsc))
	mm, _ = mm.Update(runCmd(t, cmd))
	if got := mm.(model); got.firstRun.screen != frAlerts || !got.firstRun.adminExists {
		t.Fatalf("screen %v exists %v, want alerts with admin already present", got.firstRun.screen, got.firstRun.adminExists)
	}
}

func TestFirstRunInviteErrorIsShown(t *testing.T) {
	stubWeb(t, 0, "", errors.New("trinetra-web not installed"))
	var mm tea.Model = newModel(&fakeAPI{cfg: config.Default()}).startFirstRun()
	mm, _ = press(t, mm, keyType(tea.KeyEnter))
	mm, cmd := press(t, mm, keyType(tea.KeyEsc))
	mm, _ = mm.Update(runCmd(t, cmd))
	if v := mm.View(); !strings.Contains(v, "trinetra-web not installed") || !strings.Contains(v, "sudo trinetra users invite --role admin") {
		t.Fatalf("invite error not explained:\n%s", v)
	}
}

func TestFirstRunResumesAfterWebWizard(t *testing.T) {
	stubWeb(t, 0, "https://x/enroll?token=1", nil)
	var mm tea.Model = newModel(&fakeAPI{cfg: config.Default()}).startFirstRun()
	mm, _ = press(t, mm, keyType(tea.KeyEnter)) // -> web
	mm, _ = press(t, mm, keyType(tea.KeyEnter)) // open wizard
	if mm.(model).step != stepSetupWeb {
		t.Fatalf("step %v, want the web wizard", mm.(model).step)
	}
	mm, cmd := press(t, mm, keyType(tea.KeyEsc)) // leave the wizard
	got := mm.(model)
	if got.step != stepFirstRun || got.firstRun.screen != frAdmin || cmd == nil {
		t.Fatalf("leaving the wizard should resume at the admin step with an invite: step=%v screen=%v", got.step, got.firstRun.screen)
	}
}

func TestHomeShowsNoChannelReminder(t *testing.T) {
	cfg := config.Default()
	cfg.Setup.Completed = true
	var mm tea.Model = newModel(&fakeAPI{cfg: cfg})
	mm, _ = mm.Update(onboardCheckMsg{cfg: cfg})
	mm, _ = mm.Update(snapshotMsg{})
	if !strings.Contains(mm.View(), "No alert channel yet") {
		t.Fatal("Home lacks the no-channel reminder")
	}
	mm, cmd := press(t, mm, keyRunes('n'))
	if mm.(model).step != stepManage || mm.(model).mgr.screen != manageChannelsList || cmd == nil {
		t.Fatal("n should open the channel list")
	}
}
