package trinetra

import (
	"context"

	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

// telegramNotifier delivers Alerts over Telegram via an existing telegram.Client.
type telegramNotifier struct {
	client *telegram.Client
	name   string
}

func (t *telegramNotifier) Name() string { return t.name }

// Send delivers a as a formatted Telegram message, honoring ctx: a cancelled ctx aborts the
// send.
func (t *telegramNotifier) Send(ctx context.Context, a Alert) error {
	if len(a.Buttons) > 0 {
		return t.client.SendMessageWithButtonsContext(ctx, formatAlert(a), a.Buttons)
	}
	return t.client.SendMessageContext(ctx, formatAlert(a))
}
