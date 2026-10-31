package telegram

import "context"

// PollQueued keeps polling and callback acknowledgements independent of slow
// operations. A single bounded worker preserves the order of all admin changes.
// Backpressure never drops an accepted update. Shutdown joins the worker.
func (c *Client) PollQueued(ctx context.Context, prepare func(context.Context, *Update) error, handle func(context.Context, Update) error, report func(error)) error {
	jobs := make(chan Update, 128)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for u := range jobs {
			if ctx.Err() != nil {
				continue
			}
			if err := handle(ctx, u); err != nil {
				report(err)
			}
		}
	}()
	err := c.Poll(ctx, func(ctx context.Context, u Update) error {
		if err := prepare(ctx, &u); err != nil {
			report(err)
		}
		select {
		case jobs <- u:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, report)
	close(jobs)
	<-done
	return err
}
