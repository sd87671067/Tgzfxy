package forwarder

import (
	"context"
	"encoding/json"
)

type savedReceipt struct {
	Key   receiptKey
	Value Receipt
}
type retrySnapshot struct {
	Queue    []*Delivery
	Receipts []savedReceipt
}

func (e *Engine) restoreRetry(ctx context.Context) error {
	var raw string
	if err := e.DB.QueryRowContext(ctx, "SELECT snapshot FROM retry_state WHERE id=1").Scan(&raw); err != nil {
		return err
	}
	var state retrySnapshot
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return err
	}
	for _, d := range state.Queue {
		e.Queue[d.Key] = d
	}
	for _, r := range state.Receipts {
		e.Receipts[r.Key] = r.Value
	}
	e.prune()
	return nil
}
func (e *Engine) saveRetry(ctx context.Context) error {
	state := retrySnapshot{}
	for _, d := range e.Queue {
		state.Queue = append(state.Queue, d)
	}
	for k, v := range e.Receipts {
		state.Receipts = append(state.Receipts, savedReceipt{k, v})
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = e.DB.ExecContext(ctx, "UPDATE retry_state SET snapshot=? WHERE id=1 AND snapshot<>?", string(raw), string(raw))
	return err
}
