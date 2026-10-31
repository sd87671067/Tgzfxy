package forwarder

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeSession struct {
	mu               sync.Mutex
	starter          func(context.Context) error
	started, stopped chan struct{}
}

func (s *fakeSession) SetStarter(f func(context.Context) error) {
	s.mu.Lock()
	s.starter = f
	s.mu.Unlock()
}
func (s *fakeSession) Run(ctx context.Context) error {
	s.started <- struct{}{}
	<-ctx.Done()
	s.stopped <- struct{}{}
	return ctx.Err()
}
func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle timeout")
	}
}
func TestIdleForwarderStartsAndReleasesOnChanges(t *testing.T) {
	e, _, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := e.DB.Exec("UPDATE rules SET enabled=0"); err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{started: make(chan struct{}, 4), stopped: make(chan struct{}, 4)}
	done := make(chan struct{})
	go func() { defer close(done); e.Serve(ctx, session, make(chan Message)) }()
	select {
	case <-session.started:
		t.Fatal("idle started MTProto")
	case <-time.After(80 * time.Millisecond):
	}
	for i := 0; i < 2; i++ {
		if _, err := e.DB.Exec("UPDATE rules SET enabled=1"); err != nil {
			t.Fatal(err)
		}
		e.Changed()
		waitSignal(t, session.started)
		if _, err := e.DB.Exec("UPDATE rules SET enabled=0"); err != nil {
			t.Fatal(err)
		}
		e.Changed()
		waitSignal(t, session.stopped)
	}
	cancel()
	waitSignal(t, done)
}
func TestRetryQueueAndReceiptsSurviveRestart(t *testing.T) {
	e, tr, r := setup(t)
	ctx := context.Background()
	key := deliveryKey{r.ID, 7}
	e.track(r, 7, "https://example.org", []int{100})
	e.failure(ctx, Message{Chat: 222, Sender: 222, Text: "分享生成 STRM 文件失败\nhttps://example.org", Reply: 100}, []Rule{r})
	if err := e.saveRetry(ctx); err != nil {
		t.Fatal(err)
	}
	var path string
	e.DB.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path)
	reopened, err := Open(path, tr, e.Bot)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	d := reopened.Queue[key]
	if d == nil || d.State != "queued" || !d.Due.Equal(e.Queue[key].Due) {
		t.Fatal(d)
	}
	if reopened.Receipts[receiptKey{222, 100}].Key != key {
		t.Fatal("reply association lost")
	}
}
