package serverwatch

import (
	"context"

	"serverwatch/internal/telegram"
)

// telegramNotifier delivers Alerts over Telegram via an existing
// telegram.Client. It's the Notifier implementation registered for the
// "telegram" ChannelConfig type in buildNotifier (channels.go).
type telegramNotifier struct {
	client *telegram.Client
	name   string
}

func (t *telegramNotifier) Name() string { return t.name }

// Send delivers a as a formatted Telegram message. It honors ctx
// best-effort: if ctx is already cancelled it returns immediately without
// hitting the network. telegram.Client.SendMessage itself doesn't take a
// ctx (it uses a client-level HTTP timeout), so cancellation mid-flight
// isn't observed here; the Dispatcher's own per-send timeout is the
// backstop for that case.
func (t *telegramNotifier) Send(ctx context.Context, a Alert) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.client.SendMessage(formatAlert(a))
}
