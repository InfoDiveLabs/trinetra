package main

import (
	"errors"
	"testing"

	"serverwatch/internal/config"
)

func TestBuildChannelConfigDropsEmptySettings(t *testing.T) {
	ans := channelAnswers{
		Name:    "tg",
		Type:    "telegram",
		Enabled: true,
		Settings: map[string]string{
			"token":   "abc",
			"chat_id": "", // blank: must fall back to the global telegram.chat_id, not shadow it with ""
		},
	}
	cc := buildChannelConfig(ans)
	if cc.Name != "tg" || cc.Type != "telegram" || !cc.Enabled {
		t.Fatalf("buildChannelConfig() = %+v, want Name/Type/Enabled to round-trip", cc)
	}
	if cc.MinSeverity != "info" {
		t.Errorf("MinSeverity = %q, want info (the permissive default)", cc.MinSeverity)
	}
	if _, ok := cc.Settings["chat_id"]; ok {
		t.Errorf("Settings[chat_id] present = %q, want dropped (blank optional field)", cc.Settings["chat_id"])
	}
	if cc.Settings["token"] != "abc" {
		t.Errorf("Settings[token] = %q, want abc", cc.Settings["token"])
	}
}

func TestApplyChannelAddNewChannel(t *testing.T) {
	cfg := &config.Config{}
	ans := channelAnswers{Name: "tg", Type: "telegram", Enabled: true, Settings: map[string]string{"token": "abc", "chat_id": "123"}}
	if err := applyChannelAdd(cfg, ans); err != nil {
		t.Fatalf("applyChannelAdd() error = %v, want nil", err)
	}
	cc, ok := cfg.GetChannel("tg")
	if !ok {
		t.Fatal("channel tg not found after applyChannelAdd")
	}
	if cc.Type != "telegram" || !cc.Enabled || cc.Settings["token"] != "abc" || cc.Settings["chat_id"] != "123" {
		t.Errorf("added channel = %+v, want telegram/enabled/token=abc/chat_id=123", cc)
	}
}

func TestApplyChannelAddDuplicateNameRejected(t *testing.T) {
	cfg := &config.Config{}
	cfg.AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram"})
	err := applyChannelAdd(cfg, channelAnswers{Name: "tg", Type: "webhook"})
	if err == nil {
		t.Fatal("applyChannelAdd() error = nil, want a duplicate-name error")
	}
	if len(cfg.Channels) != 1 {
		t.Fatalf("len(cfg.Channels) = %d, want 1 (nothing added on rejection)", len(cfg.Channels))
	}
}

func TestApplyChannelAddEmptyNameRejected(t *testing.T) {
	cfg := &config.Config{}
	err := applyChannelAdd(cfg, channelAnswers{Name: "", Type: "telegram"})
	if err == nil {
		t.Fatal("applyChannelAdd() error = nil, want an empty-name error")
	}
	if len(cfg.Channels) != 0 {
		t.Fatalf("len(cfg.Channels) = %d, want 0", len(cfg.Channels))
	}
}

func TestApplyChannelEditPreservesRouting(t *testing.T) {
	cfg := &config.Config{}
	cfg.AddChannel(config.ChannelConfig{
		Name: "tg", Type: "telegram", Enabled: false,
		MinSeverity: "critical", IncludeKinds: []string{"fire"}, CriticalOverridesQuiet: true,
	})
	ans := channelAnswers{Type: "telegram", Enabled: true, Settings: map[string]string{"token": "xyz"}}
	if err := applyChannelEdit(cfg, "tg", ans); err != nil {
		t.Fatalf("applyChannelEdit() error = %v, want nil", err)
	}
	cc, _ := cfg.GetChannel("tg")
	if !cc.Enabled {
		t.Error("Enabled = false, want true (edited)")
	}
	if cc.Settings["token"] != "xyz" {
		t.Errorf("Settings[token] = %q, want xyz", cc.Settings["token"])
	}
	if cc.MinSeverity != "critical" || len(cc.IncludeKinds) != 1 || cc.IncludeKinds[0] != "fire" || !cc.CriticalOverridesQuiet {
		t.Errorf("routing fields changed by edit: MinSeverity=%q IncludeKinds=%v CriticalOverridesQuiet=%v, want untouched", cc.MinSeverity, cc.IncludeKinds, cc.CriticalOverridesQuiet)
	}
}

func TestApplyChannelEditUnknownChannelRejected(t *testing.T) {
	cfg := &config.Config{}
	err := applyChannelEdit(cfg, "nope", channelAnswers{Type: "telegram"})
	if err == nil {
		t.Fatal("applyChannelEdit() error = nil, want unknown-channel error")
	}
}

func TestApplyChannelRemove(t *testing.T) {
	cfg := &config.Config{}
	cfg.AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram"})
	if err := applyChannelRemove(cfg, "tg"); err != nil {
		t.Fatalf("applyChannelRemove() error = %v, want nil", err)
	}
	if len(cfg.Channels) != 0 {
		t.Fatalf("len(cfg.Channels) = %d, want 0", len(cfg.Channels))
	}
}

func TestApplyChannelRemoveUnknownRejected(t *testing.T) {
	cfg := &config.Config{}
	if err := applyChannelRemove(cfg, "nope"); err == nil {
		t.Fatal("applyChannelRemove() error = nil, want unknown-channel error")
	}
}

func TestChannelNeedsValidation(t *testing.T) {
	if !channelNeedsValidation(config.ChannelConfig{Enabled: true}) {
		t.Error("channelNeedsValidation(enabled) = false, want true")
	}
	if channelNeedsValidation(config.ChannelConfig{Enabled: false}) {
		t.Error("channelNeedsValidation(disabled) = true, want false")
	}
}

// TestSaveChannelGateBlocksInvalidEnabledChannel is the #79-safe regression
// test: an enabled channel that fails api.ValidateChannel must NOT be
// persisted -- cfg comes back untouched, and the caller learns why.
func TestSaveChannelGateBlocksInvalidEnabledChannel(t *testing.T) {
	wantErr := errors.New("token not configured")
	api := &fakeAPI{validateErr: wantErr}
	cfg := &config.Config{}
	ans := channelAnswers{Name: "tg", Type: "telegram", Enabled: true}

	err := saveChannel(api, cfg, "", ans, false)
	if err == nil {
		t.Fatal("saveChannel() error = nil, want the validation error surfaced")
	}
	if len(cfg.Channels) != 0 {
		t.Fatalf("len(cfg.Channels) = %d, want 0 (nothing persisted when the gate rejects)", len(cfg.Channels))
	}
	if len(api.validateCalls) != 1 {
		t.Fatalf("ValidateChannel calls = %d, want 1", len(api.validateCalls))
	}
}

// TestSaveChannelGateSkippedForDisabledChannel asserts a disabled channel
// skips the ValidateChannel gate entirely (it can't misdeliver) and saves
// even though the fake would have rejected it.
func TestSaveChannelGateSkippedForDisabledChannel(t *testing.T) {
	api := &fakeAPI{validateErr: errors.New("would fail if checked")}
	cfg := &config.Config{}
	ans := channelAnswers{Name: "tg", Type: "telegram", Enabled: false}

	if err := saveChannel(api, cfg, "", ans, false); err != nil {
		t.Fatalf("saveChannel() error = %v, want nil (disabled channels skip the gate)", err)
	}
	if len(api.validateCalls) != 0 {
		t.Errorf("ValidateChannel calls = %d, want 0", len(api.validateCalls))
	}
	if _, ok := cfg.GetChannel("tg"); !ok {
		t.Error("channel tg not saved")
	}
}

func TestSaveChannelAddSuccess(t *testing.T) {
	api := &fakeAPI{}
	cfg := &config.Config{}
	ans := channelAnswers{Name: "tg", Type: "telegram", Enabled: true, Settings: map[string]string{"token": "abc", "chat_id": "1"}}

	if err := saveChannel(api, cfg, "", ans, false); err != nil {
		t.Fatalf("saveChannel() error = %v, want nil", err)
	}
	cc, ok := cfg.GetChannel("tg")
	if !ok || cc.Settings["token"] != "abc" {
		t.Errorf("channel not saved as expected: %+v, ok=%v", cc, ok)
	}
	if len(api.validateCalls) != 1 || api.validateCalls[0].Name != "tg" {
		t.Errorf("validateCalls = %+v, want one call for cc.Name=tg", api.validateCalls)
	}
}

func TestSaveChannelEditSuccess(t *testing.T) {
	api := &fakeAPI{}
	cfg := &config.Config{}
	cfg.AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram", Enabled: false})
	ans := channelAnswers{Type: "telegram", Enabled: true, Settings: map[string]string{"token": "abc", "chat_id": "1"}}

	if err := saveChannel(api, cfg, "tg", ans, true); err != nil {
		t.Fatalf("saveChannel() error = %v, want nil", err)
	}
	cc, _ := cfg.GetChannel("tg")
	if !cc.Enabled || cc.Settings["token"] != "abc" {
		t.Errorf("channel not edited as expected: %+v", cc)
	}
	if len(api.validateCalls) != 1 || api.validateCalls[0].Name != "tg" {
		t.Errorf("validateCalls = %+v, want one call naming the edited channel tg", api.validateCalls)
	}
}

func TestSortedChannelsOrder(t *testing.T) {
	cfg := &config.Config{}
	cfg.AddChannel(config.ChannelConfig{Name: "zzz", Type: "webhook"})
	cfg.AddChannel(config.ChannelConfig{Name: "aaa", Type: "telegram"})
	got := sortedChannels(cfg)
	if len(got) != 2 || got[0].Name != "aaa" || got[1].Name != "zzz" {
		t.Fatalf("sortedChannels() = %+v, want [aaa zzz]", got)
	}
}
