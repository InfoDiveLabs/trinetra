package trinetra

import (
	"context"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// TestHostPrefixNotifierPrefixesTitle pins #101's multi-host disambiguation: the decorator
// prepends "[server.name]" to the title the inner notifier sees.
func TestHostPrefixNotifierPrefixesTitle(t *testing.T) {
	inner := &fakeNotifier{name: "inner"}
	orig := Alert{Title: "disk:/ = 91.0"}

	h := hostPrefixNotifier{inner: inner, serverName: "attic-pi"}
	if err := h.Send(context.Background(), orig); err != nil {
		t.Fatal(err)
	}
	got := inner.received()
	if len(got) != 1 || got[0].Title != "[attic-pi] disk:/ = 91.0" {
		t.Fatalf("inner saw %+v, want title prefixed with [attic-pi]", got)
	}
	if orig.Title != "disk:/ = 91.0" {
		t.Errorf("caller's Alert.Title mutated to %q; must stay host-neutral", orig.Title)
	}
	if h.Name() != "inner" {
		t.Errorf("Name() = %q, want the inner notifier's name", h.Name())
	}

	// Empty server name: no prefix.
	bare := &fakeNotifier{name: "b"}
	_ = hostPrefixNotifier{inner: bare, serverName: ""}.Send(context.Background(), Alert{Title: "x"})
	if g := bare.received(); len(g) != 1 || g[0].Title != "x" {
		t.Errorf("empty name should not prefix; got %+v", g)
	}
}

// TestSendTestNotificationUnknownChannel pins sendTestNotification's "unknown channel"
// error path.
func TestSendTestNotificationUnknownChannel(t *testing.T) {
	c := config.Default()
	if err := sendTestNotification(c, "does-not-exist", "web"); err == nil {
		t.Fatal("expected an error for an unknown channel, got nil")
	}
}

// TestSendTestNotificationSurfacesBuildNotifierError pins that an incomplete channel
// config.
func TestSendTestNotificationSurfacesBuildNotifierError(t *testing.T) {
	c := config.Default()
	c.AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram"}) // no token, no chat_id
	err := sendTestNotification(c, "tg", "web")
	if err == nil {
		t.Fatal("expected an error for an incomplete telegram channel, got nil")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q, want it to mention the missing token", err.Error())
	}
}

func TestBuildNotifierNotImplementedForEveryType(t *testing.T) {
	c := config.Default()
	for _, typ := range []string{"whatever"} {
		if _, err := buildNotifier(config.ChannelConfig{Name: "x", Type: typ}, c); err == nil {
			t.Errorf("buildNotifier(type=%q) expected not-implemented error, got nil", typ)
		}
	}
}

func TestBuildNotifierTelegram(t *testing.T) {
	// Settings override the config-level secret when both are present.
	c := config.Default()
	c.Telegram.Token = "cfg-token"
	c.Telegram.ChatID = "cfg-chat"
	cc := config.ChannelConfig{Name: "tg", Type: "telegram", Settings: map[string]string{
		"token": "override-token", "chat_id": "override-chat",
	}}
	n, err := buildNotifier(cc, c)
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	if n.Name() != "tg" {
		t.Errorf("Name() = %q, want tg", n.Name())
	}

	// Falls back to config-level token/chat_id when Settings has none.
	cc2 := config.ChannelConfig{Name: "tg2", Type: "telegram"}
	if _, err := buildNotifier(cc2, c); err != nil {
		t.Errorf("buildNotifier with config-level secrets: %v", err)
	}

	// Missing token entirely (neither Settings nor config) errors.
	if _, err := buildNotifier(config.ChannelConfig{Name: "tg3", Type: "telegram"}, config.Default()); err == nil {
		t.Error("expected error when token is unavailable")
	}

	// Missing chat_id entirely errors too.
	noChat := config.Default()
	noChat.Telegram.Token = "tok"
	if _, err := buildNotifier(config.ChannelConfig{Name: "tg4", Type: "telegram"}, noChat); err == nil {
		t.Error("expected error when chat_id is unavailable")
	}
}

func TestRouteFromChannelConfig(t *testing.T) {
	cc := config.ChannelConfig{
		MinSeverity:            "warning",
		IncludeKinds:           []string{"disk"},
		ExcludeKinds:           []string{"smart"},
		CriticalOverridesQuiet: true,
	}
	r := routeFromChannelConfig(cc)
	if r.MinSeverity != SevWarning {
		t.Errorf("MinSeverity = %v, want SevWarning", r.MinSeverity)
	}
	if len(r.IncludeKinds) != 1 || r.IncludeKinds[0] != "disk" {
		t.Errorf("IncludeKinds = %v", r.IncludeKinds)
	}
	if len(r.ExcludeKinds) != 1 || r.ExcludeKinds[0] != "smart" {
		t.Errorf("ExcludeKinds = %v", r.ExcludeKinds)
	}
	if !r.CriticalOverridesQuiet {
		t.Error("CriticalOverridesQuiet should be true")
	}

	// Empty MinSeverity defaults permissively to info.
	def := routeFromChannelConfig(config.ChannelConfig{})
	if def.MinSeverity != SevInfo {
		t.Errorf("default MinSeverity = %v, want SevInfo", def.MinSeverity)
	}
}

func TestChannelsFromConfigSkipsUnavailableNotifiers(t *testing.T) {
	c := config.Default()
	c.AddChannel(config.ChannelConfig{
		Name: "tg", Type: "telegram", Enabled: true,
		MinSeverity: "warning", IncludeKinds: []string{"disk"},
	})
	// The telegram channel has no token/chat_id available (neither in Settings nor in
	// config.Telegram).
	got := channelsFromConfig(c)
	if len(got) != 0 {
		t.Fatalf("expected 0 channels (telegram channel missing secrets), got %d", len(got))
	}
}

func TestMigrateTelegramChannelAddsWhenTokenSetAndNoneExists(t *testing.T) {
	c := config.Default()
	c.Telegram.Token = "tok"
	c.Telegram.ChatID = "chat1"
	if !migrateTelegramChannel(c) {
		t.Fatal("expected migration to report a change")
	}
	got, ok := c.GetChannel("telegram")
	if !ok {
		t.Fatal("expected telegram channel to be added")
	}
	if got.Type != "telegram" || !got.Enabled || got.MinSeverity != "info" {
		t.Errorf("migrated channel = %+v", got)
	}
	if got.Settings["chat_id"] != "chat1" {
		t.Errorf("chat_id = %q, want chat1", got.Settings["chat_id"])
	}
	// config.Default() sets the legacy global CriticalOverridesQuiet to true.
	if !got.CriticalOverridesQuiet {
		t.Error("migrated channel should carry over CriticalOverridesQuiet=true from the legacy global default")
	}
}

func TestMigrateTelegramChannelNoopWithoutToken(t *testing.T) {
	c := config.Default()
	if migrateTelegramChannel(c) {
		t.Fatal("expected no migration without a token")
	}
	if len(c.Channels) != 0 {
		t.Fatal("expected no channels added")
	}
}

func TestMigrateTelegramChannelNoopWhenNameTakenByOtherType(t *testing.T) {
	c := config.Default()
	c.Telegram.Token = "tok"
	// A channel already NAMED "telegram" but of a different type (e.g. a hand-edited/restored
	// webhook).
	c.AddChannel(config.ChannelConfig{Name: "telegram", Type: "webhook", Enabled: true})
	before := len(c.Channels)

	if migrateTelegramChannel(c) {
		t.Fatal("expected no migration when a channel named telegram already exists")
	}
	if len(c.Channels) != before {
		t.Fatalf("expected no channel appended, len went %d -> %d", before, len(c.Channels))
	}
	got, _ := c.GetChannel("telegram")
	if got.Type != "webhook" {
		t.Fatalf("existing channel must be untouched, type = %q", got.Type)
	}
}

func TestMigrateTelegramChannelIdempotent(t *testing.T) {
	c := config.Default()
	c.Telegram.Token = "tok"
	if !migrateTelegramChannel(c) {
		t.Fatal("expected first call to migrate")
	}
	if migrateTelegramChannel(c) {
		t.Fatal("expected second call to be a no-op")
	}
	count := 0
	for _, cc := range c.Channels {
		if cc.Type == "telegram" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 telegram channel, got %d", count)
	}
}
