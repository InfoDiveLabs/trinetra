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
// request) instead of blocking for the client's full timeout. When a
// carries Buttons (task 9: the master's alerting engine sets these on an
// incident's fire notification), they're attached as an inline keyboard;
// otherwise this is byte-for-byte the same plain send as before.
func (t *telegramNotifier) Send(ctx context.Context, a Alert) error {
	if len(a.Buttons) > 0 {
		return t.client.SendMessageWithButtonsContext(ctx, formatAlert(a), a.Buttons)
	}
	return t.client.SendMessageContext(ctx, formatAlert(a))
}
