package serverwatch

import (
	"testing"
	"time"

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
	off, _ := processUpdates(ups, 0, cfg, &enrollState{pin: "424242"}, time.Now, getCfg, setChatID, reply)

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
	processUpdates(ups, 0, cfg, &enrollState{pin: "424242"}, time.Now, getCfg, setChatID, reply)

	if len(replied) != 1 || replied[0].ChatID != "111" {
		t.Fatalf("authorized sender must get exactly one reply; got %+v", replied)
	}
}

// TestProcessUpdatesEnrollsOnCorrectPINThenReplies is the #78 Scenario A fix:
// with no chat id set, ownership is claimed ONLY by a correct "/start <pin>",
// and that now-authorized sender is answered.
func TestProcessUpdatesEnrollsOnCorrectPINThenReplies(t *testing.T) {
	cur := newTgConfig("tok", "") // chat id not yet known
	getCfg := func() *config.Config { return cur }
	setChatID := func(id string) {
		nc := *cur
		nc.Telegram.ChatID = id
		cur = &nc
	}
	var replied []telegram.Update
	reply := func(c *config.Config, u telegram.Update) { replied = append(replied, u) }

	ups := []telegram.Update{{UpdateID: 1, Text: "/start 424242", ChatID: "555"}}
	_, c := processUpdates(ups, 0, cur, &enrollState{pin: "424242"}, time.Now, getCfg, setChatID, reply)

	if c.Telegram.ChatID != "555" {
		t.Fatalf("a correct /start <pin> must enroll the sender; got chat id %q", c.Telegram.ChatID)
	}
	if len(replied) != 1 {
		t.Fatalf("the newly-enrolled owner must be answered; got %d replies", len(replied))
	}
}

// TestProcessUpdatesRejectsWrongPIN: an attacker who messages the unclaimed
// bot without the PIN cannot become the owner (closes the capture race).
func TestProcessUpdatesRejectsWrongPIN(t *testing.T) {
	cur := newTgConfig("tok", "")
	getCfg := func() *config.Config { return cur }
	var captured []string
	setChatID := func(id string) { captured = append(captured, id) }
	var replied []telegram.Update
	reply := func(c *config.Config, u telegram.Update) { replied = append(replied, u) }

	ups := []telegram.Update{{UpdateID: 1, Text: "/start 000000", ChatID: "555"}}
	processUpdates(ups, 0, cur, &enrollState{pin: "424242"}, time.Now, getCfg, setChatID, reply)

	if len(captured) != 0 {
		t.Fatalf("a wrong PIN must not enroll anyone; captured %v", captured)
	}
	if len(replied) != 0 {
		t.Fatalf("a wrong PIN must not be answered; got %d replies", len(replied))
	}
}

// TestProcessUpdatesIgnoresNonEnrollWhileUnclaimed: any non-enroll message to
// an unclaimed bot is ignored (no capture, no reply).
func TestProcessUpdatesIgnoresNonEnrollWhileUnclaimed(t *testing.T) {
	cur := newTgConfig("tok", "")
	getCfg := func() *config.Config { return cur }
	var captured []string
	setChatID := func(id string) { captured = append(captured, id) }
	var replied []telegram.Update
	reply := func(c *config.Config, u telegram.Update) { replied = append(replied, u) }

	ups := []telegram.Update{{UpdateID: 2, Text: "/stats", ChatID: "555"}}
	processUpdates(ups, 0, cur, &enrollState{pin: "424242"}, time.Now, getCfg, setChatID, reply)

	if len(captured) != 0 || len(replied) != 0 {
		t.Fatalf("a non-enroll message to an unclaimed bot must be ignored; captured %v, replies %d", captured, len(replied))
	}
}

func TestEnrollMatch(t *testing.T) {
	cases := []struct {
		text, pin string
		want      bool
	}{
		{"/start 424242", "424242", true},
		{"/start 000000", "424242", false},
		{"/start", "424242", false},
		{"/stats", "424242", false},
		{"start 424242", "424242", false},
		{"/start 424242 extra", "424242", false},
		{"/start 424242", "", false}, // an empty pin must never match
	}
	for _, tc := range cases {
		if got := enrollMatch(tc.text, tc.pin); got != tc.want {
			t.Errorf("enrollMatch(%q, %q) = %v, want %v", tc.text, tc.pin, got, tc.want)
		}
	}
}

func TestNewEnrollPINIsSixDigits(t *testing.T) {
	p := newEnrollPIN()
	if len(p) != 6 {
		t.Fatalf("enroll PIN must be 6 characters, got %q", p)
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			t.Fatalf("enroll PIN must be digits only, got %q", p)
		}
	}
}
