package trinetra

import (
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestWithChatIDAddsTelegramChannel(t *testing.T) {
	old := config.Default()
	old.Telegram.Token = "123:abc"
	old.Channels = []config.ChannelConfig{{Name: "ops", Type: "slack"}}
	nc := withChatID(old, "555")
	if nc.Telegram.ChatID != "555" || old.Telegram.ChatID != "" {
		t.Fatal("chat id not set on a copy")
	}
	ch, ok := nc.GetChannel("telegram")
	if !ok || !ch.Enabled || ch.Settings["chat_id"] != "555" {
		t.Fatalf("telegram channel = %+v %v, want enabled with chat_id 555", ch, ok)
	}
	if len(old.Channels) != 1 {
		t.Fatal("the old config's channel list was modified")
	}
	if len(channelsFromConfig(nc)) == 0 {
		t.Fatal("no notifier built for the new chat")
	}
}
