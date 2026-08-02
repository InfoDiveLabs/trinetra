package main

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"serverwatch/internal/config"
)

// TestManageMenuChannelsOpensListAndFetches drives the menu -> "channels"
// path (menu cursor 4, the last item) and asserts it issues
// fetchChannelsConfigCmd and lands on the list with the fetched channels.
func TestManageMenuChannelsOpensListAndFetches(t *testing.T) {
	cfg := &config.Config{}
	cfg.AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram", Enabled: true})
	api := &fakeAPI{cfg: cfg}
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(keyRunes('m'))
	for i := 0; i < 4; i++ {
		mm, _ = mm.Update(keyType(tea.KeyDown))
	}
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	got := mm.(model)
	if got.mgr.screen != manageChannelsList {
		t.Fatalf("mgr.screen = %v, want manageChannelsList", got.mgr.screen)
	}
	if !got.mgr.configLoading {
		t.Fatal("configLoading = false, want true right after opening the screen")
	}
	if cmd == nil {
		t.Fatal("expected fetchChannelsConfigCmd, got nil")
	}
	msg := runCmd(t, cmd)
	mm, _ = mm.Update(msg)
	got = mm.(model)
	if got.mgr.configLoading {
		t.Fatal("configLoading should be false once channelsConfigMsg lands")
	}
	if len(got.mgr.chanList) != 1 || got.mgr.chanList[0].Name != "tg" {
		t.Fatalf("chanList = %+v, want [tg]", got.mgr.chanList)
	}
}

// openChannelsList is TestManageMenuChannelsOpensListAndFetches's shared
// setup: drives 'm' -> down x4 -> enter -> feeds the fetch's message back
// in, returning a model sitting on the populated Channels list.
func openChannelsList(t *testing.T, api *fakeAPI) tea.Model {
	t.Helper()
	var mm tea.Model = newModel(api)
	mm, _ = mm.Update(keyRunes('m'))
	for i := 0; i < 4; i++ {
		mm, _ = mm.Update(keyType(tea.KeyDown))
	}
	mm, cmd := mm.Update(keyType(tea.KeyEnter))
	msg := runCmd(t, cmd)
	mm, _ = mm.Update(msg)
	return mm
}

// TestChannelsAddFlowTelegram walks the full add flow -- name, type,
// enabled, and telegram's two Settings fields -- and asserts the saved
// config carries everything, with the #79 gate consulted along the way
// since the channel is enabled.
func TestChannelsAddFlowTelegram(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}}
	mm := openChannelsList(t, api)

	mm, _ = mm.Update(keyRunes('a'))
	if mm.(model).mgr.screen != manageChannelsName {
		t.Fatalf("mgr.screen = %v, want manageChannelsName", mm.(model).mgr.screen)
	}
	mm = typeString(t, mm, "tg")
	mm, _ = mm.Update(keyType(tea.KeyEnter))
	if mm.(model).mgr.screen != manageChannelsType {
		t.Fatalf("mgr.screen = %v, want manageChannelsType", mm.(model).mgr.screen)
	}
	// telegram is channelTypeChoices[0]: accept it immediately.
	mm, _ = mm.Update(keyType(tea.KeyEnter))
	if mm.(model).mgr.screen != manageChannelsEnabled {
		t.Fatalf("mgr.screen = %v, want manageChannelsEnabled", mm.(model).mgr.screen)
	}
	if !mm.(model).mgr.chanAns.Enabled {
		t.Fatal("chanAns.Enabled = false, want true (the add flow's default)")
	}
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept enabled=true
	if mm.(model).mgr.screen != manageChannelsField {
		t.Fatalf("mgr.screen = %v, want manageChannelsField", mm.(model).mgr.screen)
	}

	mm = typeString(t, mm, "mytoken")
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // token -> chat_id field
	mm = typeString(t, mm, "12345")
	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // chat_id -> save
	if cmd == nil {
		t.Fatal("expected saveChannelCmd after the last field, got nil")
	}
	msg := runCmd(t, cmd)
	saved, ok := msg.(channelSavedMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want channelSavedMsg", msg)
	}
	if saved.err != nil {
		t.Fatalf("save err = %v, want nil", saved.err)
	}
	if api.applied == nil {
		t.Fatal("ApplyConfig was never called")
	}
	cc, ok := api.applied.GetChannel("tg")
	if !ok {
		t.Fatal("channel tg not present in the applied config")
	}
	if cc.Type != "telegram" || !cc.Enabled || cc.Settings["token"] != "mytoken" || cc.Settings["chat_id"] != "12345" {
		t.Errorf("saved channel = %+v, want telegram/enabled/token=mytoken/chat_id=12345", cc)
	}
	if len(api.validateCalls) != 1 {
		t.Errorf("ValidateChannel calls = %d, want 1 (enabled channel gate)", len(api.validateCalls))
	}

	mm, _ = mm.Update(saved)
	if mm.(model).mgr.screen != manageResult {
		t.Fatalf("mgr.screen after channelSavedMsg = %v, want manageResult", mm.(model).mgr.screen)
	}
}

// TestChannelsAddFlowGateBlocksInvalidEnabledChannel asserts a failing
// api.ValidateChannel surfaces on manageResult and leaves ApplyConfig
// untouched -- the #79-safe path exercised end to end through the UI, not
// just the pure saveChannel function (channels_test.go already covers
// that directly).
func TestChannelsAddFlowGateBlocksInvalidEnabledChannel(t *testing.T) {
	wantErr := errors.New("token not configured")
	api := &fakeAPI{cfg: &config.Config{}, validateErr: wantErr}
	mm := openChannelsList(t, api)

	mm, _ = mm.Update(keyRunes('a'))
	mm = typeString(t, mm, "tg")
	mm, _ = mm.Update(keyType(tea.KeyEnter))    // name -> type
	mm, _ = mm.Update(keyType(tea.KeyEnter))    // telegram -> enabled
	mm, _ = mm.Update(keyType(tea.KeyEnter))    // enabled=true -> fields
	mm, _ = mm.Update(keyType(tea.KeyEnter))    // blank token -> chat_id
	mm, cmd := mm.Update(keyType(tea.KeyEnter)) // blank chat_id -> save

	msg := runCmd(t, cmd)
	saved := msg.(channelSavedMsg)
	if saved.err == nil {
		t.Fatal("save err = nil, want the validation error surfaced")
	}
	if api.applyN != 0 {
		t.Errorf("ApplyConfig calls = %d, want 0 (gate rejected before any apply)", api.applyN)
	}

	mm, _ = mm.Update(saved)
	got := mm.(model)
	if got.mgr.screen != manageResult {
		t.Fatalf("mgr.screen = %v, want manageResult", got.mgr.screen)
	}
	if got.mgr.applyErr == nil {
		t.Error("mgr.applyErr = nil, want the gate's error surfaced")
	}
}

// TestChannelsEditFlowPrefillsExistingValues asserts 'e' on an existing
// channel opens the type/enabled/field steps pre-filled from its current
// config, not blank -- the same "never blind-wipe" concern the schedule/
// quiet-hours/healthchecks screens' pre-fill fixes for their own fields.
func TestChannelsEditFlowPrefillsExistingValues(t *testing.T) {
	cfg := &config.Config{}
	cfg.AddChannel(config.ChannelConfig{
		Name: "tg", Type: "telegram", Enabled: false,
		Settings: map[string]string{"token": "oldtoken"},
	})
	api := &fakeAPI{cfg: cfg}
	mm := openChannelsList(t, api)

	mm, _ = mm.Update(keyRunes('e'))
	got := mm.(model)
	if got.mgr.screen != manageChannelsType {
		t.Fatalf("mgr.screen = %v, want manageChannelsType", got.mgr.screen)
	}
	if channelTypeChoices[got.mgr.chanTypeCur] != "telegram" {
		t.Errorf("chanTypeCur points at %q, want telegram", channelTypeChoices[got.mgr.chanTypeCur])
	}

	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept telegram
	if mm.(model).mgr.chanAns.Enabled {
		t.Fatal("chanAns.Enabled = true, want false (the existing channel's own state)")
	}
	mm, _ = mm.Update(keyType(tea.KeyEnter)) // accept enabled=false
	if mm.(model).mgr.screen != manageChannelsField {
		t.Fatalf("mgr.screen = %v, want manageChannelsField", mm.(model).mgr.screen)
	}
	if mm.(model).mgr.chanFieldIn.Value() != "oldtoken" {
		t.Errorf("chanFieldIn = %q, want pre-filled with oldtoken", mm.(model).mgr.chanFieldIn.Value())
	}
}

// TestChannelsRemove asserts 'd' removes the channel under the cursor via
// one atomic ApplyConfig, and the list reflects it once the msg lands.
func TestChannelsRemove(t *testing.T) {
	cfg := &config.Config{}
	cfg.AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram"})
	api := &fakeAPI{cfg: cfg}
	mm := openChannelsList(t, api)

	mm, cmd := mm.Update(keyRunes('d'))
	if cmd == nil {
		t.Fatal("expected removeChannelCmd, got nil")
	}
	msg := runCmd(t, cmd)
	action, ok := msg.(channelActionMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want channelActionMsg", msg)
	}
	if action.err != nil {
		t.Fatalf("remove err = %v, want nil", action.err)
	}
	if _, ok := api.applied.GetChannel("tg"); ok {
		t.Error("channel tg still present in the applied config after remove")
	}

	mm, _ = mm.Update(action)
	if len(mm.(model).mgr.chanList) != 0 {
		t.Errorf("chanList = %+v, want empty after remove", mm.(model).mgr.chanList)
	}
}

// TestChannelsTest asserts 't' calls api.TestChannel for the channel under
// the cursor and surfaces a status line, without touching config at all.
func TestChannelsTest(t *testing.T) {
	cfg := &config.Config{}
	cfg.AddChannel(config.ChannelConfig{Name: "tg", Type: "telegram"})
	api := &fakeAPI{cfg: cfg}
	mm := openChannelsList(t, api)

	mm, cmd := mm.Update(keyRunes('t'))
	if cmd == nil {
		t.Fatal("expected testChannelCmd, got nil")
	}
	msg := runCmd(t, cmd)
	action := msg.(channelActionMsg)
	if !action.isTest || action.err != nil {
		t.Fatalf("channelActionMsg = %+v, want isTest=true err=nil", action)
	}
	if len(api.testChannelCalls) != 1 || api.testChannelCalls[0] != "tg" {
		t.Errorf("testChannelCalls = %v, want [tg]", api.testChannelCalls)
	}
	if api.applyN != 0 {
		t.Errorf("ApplyConfig calls = %d, want 0 (test never touches config)", api.applyN)
	}

	mm, _ = mm.Update(action)
	if mm.(model).mgr.chanTestMsg == "" {
		t.Error("chanTestMsg is empty, want a status line after a successful test")
	}
}

// TestChannelsListEscReturnsToMenu asserts esc on the list goes back to the
// management menu, matching every other screen's esc convention.
func TestChannelsListEscReturnsToMenu(t *testing.T) {
	mm := openChannelsList(t, &fakeAPI{cfg: &config.Config{}})
	mm, _ = mm.Update(keyType(tea.KeyEsc))
	if mm.(model).mgr.screen != manageMenuList {
		t.Fatalf("mgr.screen = %v, want manageMenuList", mm.(model).mgr.screen)
	}
}
