package trinetra

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestChannelAddListSetRemoveTestViaCLI(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")

	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	// Staged disabled: this channel has a chat_id but no token, which would fail the #83
	// up-front validation gate if added enabled.
	if code := Main([]string{"channel", "add", "tg", "--type", "telegram", "--set", "chat_id=123", "--disabled"}); code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, errb.String())
	}
	if code := Main([]string{"channel", "set", "tg", "enabled", "true"}); code != 0 {
		t.Fatalf("set enabled exit=%d stderr=%s", code, errb.String())
	}

	out.Reset()
	if code := Main([]string{"channel", "list"}); code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "tg") || !strings.Contains(out.String(), "telegram") {
		t.Fatalf("list output missing channel: %q", out.String())
	}

	if code := Main([]string{"channel", "set", "tg", "min_severity", "critical"}); code != 0 {
		t.Fatalf("set exit=%d stderr=%s", code, errb.String())
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.GetChannel("tg")
	if !ok || got.MinSeverity != "critical" {
		t.Fatalf("channel after set = %+v ok=%v", got, ok)
	}
	if got.Settings["chat_id"] != "123" {
		t.Fatalf("settings.chat_id = %q, want 123", got.Settings["chat_id"])
	}

	// test: telegram is implemented now, but this channel has a chat_id and no token (neither
	// in Settings nor in config.Telegram).
	errb.Reset()
	if code := Main([]string{"channel", "test", "tg"}); code == 0 {
		t.Fatalf("expected non-zero exit for missing token, stderr=%s", errb.String())
	}
	if !strings.Contains(errb.String(), "token") {
		t.Errorf("expected 'token' in stderr, got %q", errb.String())
	}

	if code := Main([]string{"channel", "remove", "tg"}); code != 0 {
		t.Fatalf("remove exit=%d stderr=%s", code, errb.String())
	}
	c2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.GetChannel("tg"); ok {
		t.Fatal("expected channel removed")
	}

	if code := Main([]string{"channel", "remove", "tg"}); code == 0 {
		t.Fatal("expected removing an already-removed channel to fail")
	}
}

func TestChannelAddRequiresType(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	if code := Main([]string{"channel", "add", "tg"}); code == 0 {
		t.Fatal("expected channel add without --type to fail")
	}
}

// TestChannelAddRejectsIncompleteEnabledChannel is the #83 "cheap standalone win".
func TestChannelAddRejectsIncompleteEnabledChannel(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	// slack requires a webhook url; none given, so buildNotifier must fail
	// and the channel must never reach AddChannel/saveCfg.
	if code := Main([]string{"channel", "add", "mychan", "--type", "slack"}); code == 0 {
		t.Fatalf("expected non-zero exit for undeliverable enabled channel, stderr=%s", errb.String())
	}

	c, err := config.Load(cfgPath)
	if err != nil {
		// No config file was ever written -- also an acceptable way for
		// "not persisted" to manifest, since saveCfg is never reached.
		return
	}
	if _, ok := c.GetChannel("mychan"); ok {
		t.Fatalf("channel %q was persisted despite failing validation", "mychan")
	}
}

// TestChannelAddAcceptsCompleteChannel is the positive counterpart: an enabled channel
// whose settings DO build a working notifier is accepted and persisted.
func TestChannelAddAcceptsCompleteChannel(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	if code := Main([]string{"channel", "add", "mychan", "--type", "slack", "--set", "url=https://hooks.slack.example/T000/B000/xxx"}); code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, errb.String())
	}

	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.GetChannel("mychan"); !ok {
		t.Fatal("expected deliverable channel to be persisted")
	}
}

// TestChannelAddErrorNamesMissingSetting pins that the rejection error is actionable --
// built from buildNotifier's own real failure message (which names the missing setting).
func TestChannelAddErrorNamesMissingSetting(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	if code := Main([]string{"channel", "add", "mychan", "--type", "slack"}); code == 0 {
		t.Fatalf("expected non-zero exit, stderr=%s", errb.String())
	}
	if !strings.Contains(errb.String(), "url") {
		t.Fatalf("expected error to name the missing 'url' setting, got %q", errb.String())
	}
}

// TestChannelAddAcceptsDisabledIncompleteChannel is the staging case: a DISABLED channel
// can't misdeliver.
func TestChannelAddAcceptsDisabledIncompleteChannel(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	if code := Main([]string{"channel", "add", "mychan", "--type", "slack", "--disabled"}); code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, errb.String())
	}

	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.GetChannel("mychan")
	if !ok {
		t.Fatal("expected disabled incomplete channel to be persisted (staging)")
	}
	if got.Enabled {
		t.Fatal("expected channel to be saved disabled")
	}
}

func TestChannelListShowsMigratedTelegram(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	// Seed a legacy telegram token directly (as if from an old install).
	c := config.Default()
	c.Telegram.Token = "legacy-tok"
	c.Telegram.ChatID = "legacy-chat"
	if err := c.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	if code := Main([]string{"channel", "list"}); code != 0 {
		t.Fatalf("list exit=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "telegram") {
		t.Fatalf("expected migrated telegram channel in list output: %q", out.String())
	}

	// Migration should have persisted so it only happens once.
	c2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, cc := range c2.Channels {
		if cc.Type == "telegram" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 persisted telegram channel, got %d", count)
	}
}
