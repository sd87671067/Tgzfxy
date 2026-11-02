package forwarder

import (
	"context"
	tg "local/bots/internal/telegram"
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

type blockedHistory struct {
	*fakeTransport
	started, stopped chan struct{}
}

func (f *blockedHistory) History(ctx context.Context, _ int64, _ int, _ bool) ([]Message, error) {
	f.started <- struct{}{}
	<-ctx.Done()
	f.stopped <- struct{}{}
	return nil, ctx.Err()
}
func TestForwarderDisableCancelsInFlightHistory(t *testing.T) {
	e, tr, _ := setup(t)
	f := &blockedHistory{tr, make(chan struct{}, 2), make(chan struct{}, 2)}
	e.Transport = f
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &fakeSession{started: make(chan struct{}, 2), stopped: make(chan struct{}, 2)}
	done := make(chan struct{})
	go func() { defer close(done); e.Serve(ctx, s, make(chan Message)) }()
	waitSignal(t, f.started)
	m := tg.Message{From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1}}
	handled, err := e.Handle(ctx, m, "fw:toggle:1", "", nil)
	if !handled || err != nil {
		t.Fatal(err)
	}
	waitSignal(t, f.stopped)
	waitSignal(t, s.stopped)
	select {
	case <-f.started:
		t.Fatal("disabled rule polled again")
	case <-time.After(60 * time.Millisecond):
	}
	cancel()
	waitSignal(t, done)
}
