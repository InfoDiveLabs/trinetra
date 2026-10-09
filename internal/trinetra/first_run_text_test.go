package trinetra

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestInstallNextSteps(t *testing.T) {
	got := installNextSteps()
	for _, want := range []string{"sudo trinetra cli", "sudo trinetra users invite --role admin"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "Telegram") {
		t.Error("next steps still push Telegram")
	}
}

func TestNoChannelReminder(t *testing.T) {
	c := config.Default()
	if noChannelReminder(c) == "" {
		t.Fatal("no reminder with no channels")
	}
	c.Channels = []config.ChannelConfig{{Name: "ops", Type: "slack"}}
	if noChannelReminder(c) == "" {
		t.Fatal("a disabled channel should still get the reminder")
	}
	c.Channels[0].Enabled = true
	if noChannelReminder(c) != "" {
		t.Fatal("reminder with an enabled channel")
	}
}

func TestScheduleOffClearsBoth(t *testing.T) {
	prev := cfgPath
	t.Cleanup(func() { cfgPath = prev })
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	var out, errb bytes.Buffer
	stdout, stderr = &out, &errb
	for _, args := range [][]string{{"schedule", "daily", "09:00"}, {"schedule", "weekly", "mon@09:00"}, {"schedule", "off"}} {
		if code := Main(args); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errb.String())
		}
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := c.Get("schedule.daily")
	w, _ := c.Get("schedule.weekly")
	if d != "" || w != "" {
		t.Fatalf("daily=%q weekly=%q, want both cleared", d, w)
	}
}
