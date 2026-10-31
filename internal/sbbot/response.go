package sbbot

import (
	"context"
	tg "local/bots/internal/telegram"
	"log"
	"time"
)

func (b *Bot) acknowledge(ctx context.Context, cb *tg.Callback) error {
	if cb.Acknowledged {
		return nil
	}
	ackCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	err := b.API.Answer(ackCtx, cb.ID, "")
	log.Printf("callback_ack duration_ms=%d ok=%t", time.Since(start).Milliseconds(), err == nil)
	cb.Acknowledged = err == nil
	return err
}

func (b *Bot) Prepare(ctx context.Context, u *tg.Update) error {
	if u.Callback == nil || !b.Admins[u.Callback.From.ID] || u.Callback.Message == nil {
		return nil
	}
	return b.acknowledge(ctx, u.Callback)
}
