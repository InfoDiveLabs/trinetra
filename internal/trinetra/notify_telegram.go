package trinetra

import (
	"context"

	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

// telegramNotifier delivers Alerts over Telegram via an existing
// telegram.Client. It's the Notifier implementation registered for the
// "telegram" ChannelConfig type in buildNotifier (channels.go).
type telegramNotifier struct {
	client *telegram.Client
	name   string
}

func (t *telegramNotifier) Name() string { return t.name }

// Send delivers a as a formatted Telegram message, honoring ctx: a
// cancelled ctx aborts the send (whether before or during the HTTP
// request) instead of blocking for the client's full timeout.
func (t *telegramNotifier) Send(ctx context.Context, a Alert) error {
	return t.client.SendMessageContext(ctx, formatAlert(a))
}
