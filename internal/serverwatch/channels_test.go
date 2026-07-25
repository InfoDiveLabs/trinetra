package serverwatch

import (
	"testing"

	"serverwatch/internal/config"
)

func TestBuildNotifierNotImplementedForEveryType(t *testing.T) {
	for _, typ := range []string{"telegram", "email", "webhook", "whatever"} {
		if _, err := buildNotifier(config.ChannelConfig{Name: "x", Type: typ}); err == nil {
			t.Errorf("buildNotifier(type=%q) expected not-implemented error, got nil", typ)
		}
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
	// Every type is currently unimplemented, so channelsFromConfig must
	// build zero Channels (and not panic on the nil Notifier).
	got := channelsFromConfig(c)
	if len(got) != 0 {
		t.Fatalf("expected 0 channels (no types implemented yet), got %d", len(got))
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
	// A channel already NAMED "telegram" but of a different type (e.g. a
	// hand-edited/restored webhook). Name is the unique key everywhere, so
	// migration must not append a second {Name:"telegram"}.
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
