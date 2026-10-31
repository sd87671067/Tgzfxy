package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The same 250ms blocking operation precedes a callback in each run. This
// measures time from receiving the batch to callback acknowledgement.
func TestQueuedCallbackLatencyAndOperationOrder(t *testing.T) {
	for _, queued := range []bool{false, true} {
		label := "serial-baseline"
		if queued {
			label = "queued"
		}
		t.Run(label, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var fetched atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !fetched.Swap(true) {
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []Update{{ID: 1, Message: &Message{}}, {ID: 2, Callback: &Callback{ID: "cb"}}}})
					return
				}
				<-ctx.Done()
			}))
			defer func() { cancel(); server.Close() }()
			c := &Client{BaseURL: server.URL + "/", HTTP: server.Client()}
			ack := make(chan time.Duration, 1)
			order := make(chan int, 2)
			start := time.Now()
			prepare := func(ctx context.Context, u *Update) error {
				if u.Callback != nil {
					ack <- time.Since(start)
				}
				return nil
			}
			handle := func(ctx context.Context, u Update) error {
				if u.ID == 1 {
					select {
					case <-time.After(250 * time.Millisecond):
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				order <- int(u.ID)
				if u.ID == 2 {
					cancel()
				}
				return nil
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if queued {
					c.PollQueued(ctx, prepare, handle, func(error) {})
				} else {
					c.Poll(ctx, func(ctx context.Context, u Update) error { prepare(ctx, &u); return handle(ctx, u) }, func(error) {})
				}
			}()
			select {
			case elapsed := <-ack:
				t.Logf("acknowledgement latency %s", elapsed)
				if queued && elapsed > 100*time.Millisecond {
					t.Fatal("ack blocked behind work", elapsed)
				}
				if !queued && elapsed < 250*time.Millisecond {
					t.Fatal("invalid baseline", elapsed)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("missing ack")
			}
			<-done
			if <-order != 1 || <-order != 2 {
				t.Fatal("operations reordered")
			}
		})
	}
}
