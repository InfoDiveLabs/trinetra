package serverwatch

import (
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/telegram"
)

// newTgConfig builds a config with the given telegram token and chat id.
func newTgConfig(token, chatID string) *config.Config {
	c := &config.Config{}
	c.Telegram.Token = token
	c.Telegram.ChatID = chatID
	return c
}

// TestProcessUpdatesIgnoresUnauthorizedSender is the core #78 guard: once a
// chat id is known, an update from any OTHER chat must be dropped entirely,
// so an unknown sender can neither trigger host collection nor cause a reply.
func TestProcessUpdatesIgnoresUnauthorizedSender(t *testing.T) {
	cfg := newTgConfig("tok", "111")
	getCfg := func() *config.Config { return cfg }
	var captured []string
	setChatID := func(id string) { captured = append(captured, id) }
	var replied []telegram.Update
	reply := func(c *config.Config, u telegram.Update) { replied = append(replied, u) }

	ups := []telegram.Update{{UpdateID: 7, Text: "/stats", ChatID: "999"}}
	off, _ := processUpdates(ups, 0, cfg, getCfg, setChatID, reply)

	if len(replied) != 0 {
		t.Fatalf("unauthorized sender 999 must not be answered; got %d replies", len(replied))
	}
	if len(captured) != 0 {
		t.Fatalf("no capture expected when a chat id is already set; got %v", captured)
	}
	if off != 8 {
		t.Fatalf("offset must advance past the ignored update: got %d want 8", off)
	}
}

// TestProcessUpdatesRepliesToAuthorizedSender confirms the owner chat still
// gets a reply after the guard is in place.
func TestProcessUpdatesRepliesToAuthorizedSender(t *testing.T) {
	cfg := newTgConfig("tok", "111")
	getCfg := func() *config.Config { return cfg }
	setChatID := func(id string) { t.Fatalf("unexpected capture %q", id) }
	var replied []telegram.Update
	reply := func(c *config.Config, u telegram.Update) { replied = append(replied, u) }

	ups := []telegram.Update{{UpdateID: 3, Text: "/stats", ChatID: "111"}}
	processUpdates(ups, 0, cfg, getCfg, setChatID, reply)

	if len(replied) != 1 || replied[0].ChatID != "111" {
		t.Fatalf("authorized sender must get exactly one reply; got %+v", replied)
	}
}

// TestProcessUpdatesCapturesFirstChatIDThenReplies preserves the zero-config
// behavior: with no chat id set, the first inbound message is captured AND
// that same (now-authorized) sender is answered.
func TestProcessUpdatesCapturesFirstChatIDThenReplies(t *testing.T) {
	cur := newTgConfig("tok", "") // chat id not yet known
	getCfg := func() *config.Config { return cur }
	setChatID := func(id string) {
		nc := *cur
		nc.Telegram.ChatID = id
		cur = &nc
	}
	var replied []telegram.Update
	reply := func(c *config.Config, u telegram.Update) { replied = append(replied, u) }

	ups := []telegram.Update{{UpdateID: 1, Text: "/stats", ChatID: "555"}}
	_, c := processUpdates(ups, 0, cur, getCfg, setChatID, reply)

	if c.Telegram.ChatID != "555" {
		t.Fatalf("first inbound chat id must be captured; got %q", c.Telegram.ChatID)
	}
	if len(replied) != 1 {
		t.Fatalf("the first (now-authorized) sender must be answered; got %d replies", len(replied))
	}
}

// TestProcessUpdatesIgnoresUnidentifiableSenderBeforeCapture guards the
// pre-capture edge: an update carrying no chat id must not be processed.
func TestProcessUpdatesIgnoresUnidentifiableSenderBeforeCapture(t *testing.T) {
	cfg := newTgConfig("tok", "")
	getCfg := func() *config.Config { return cfg }
	setChatID := func(id string) { t.Fatalf("unexpected capture %q", id) }
	var replied []telegram.Update
	reply := func(c *config.Config, u telegram.Update) { replied = append(replied, u) }

	ups := []telegram.Update{{UpdateID: 2, Text: "/stats", ChatID: ""}}
	processUpdates(ups, 0, cfg, getCfg, setChatID, reply)

	if len(replied) != 0 {
		t.Fatalf("an update with no chat id must not be answered; got %d replies", len(replied))
	}
}
