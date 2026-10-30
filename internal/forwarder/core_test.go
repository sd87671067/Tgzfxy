package forwarder

import (
	"context"
	_ "embed"
	"errors"
	"io"
	tg "local/bots/internal/telegram"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/schema.sql
var testSchema string

type fakeTransport struct {
	sent, forwarded int
	fail            bool
	history         []Message
	logged          bool
}

func (f *fakeTransport) Ready(context.Context) (bool, error)                  { return true, nil }
func (f *fakeTransport) Resolve(context.Context, string) (int64, bool, error) { return 222, false, nil }
func (f *fakeTransport) History(context.Context, int64, int, bool) ([]Message, error) {
	return f.history, nil
}
func (f *fakeTransport) Message(context.Context, int64, int) (*Message, error) {
	return &Message{ID: 7, Text: "https://example.org"}, nil
}
func (f *fakeTransport) Forward(context.Context, int64, Message) ([]int, error) {
	f.forwarded++
	return []int{100}, nil
}
func (f *fakeTransport) Send(context.Context, int64, string) ([]int, error) {
	f.sent++
	if f.fail {
		return nil, errors.New("send failed")
	}
	return []int{100 + f.sent}, nil
}
func (f *fakeTransport) Download(context.Context, Message) (io.ReadCloser, string, error) {
	return io.NopCloser(strings.NewReader("media")), "photo.jpg", nil
}
func (f *fakeTransport) Click(context.Context, Message, Button) (string, string, error) {
	return "expanded", "https://example.org", nil
}
func (f *fakeTransport) SendCode(context.Context, string) (string, error) { return "codehash", nil }
func (f *fakeTransport) SignIn(context.Context, string, string, string) error {
	f.logged = true
	return nil
}
func (f *fakeTransport) Password(context.Context, string) error { f.logged = true; return nil }
func setup(t *testing.T) (*Engine, *fakeTransport, Rule) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(server.Close)
	transport := &fakeTransport{}
	e, err := Open(filepath.Join(t.TempDir(), "forwarder.db"), transport, &tg.Client{BaseURL: server.URL + "/", HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.DB.Close() })
	if _, err = e.DB.Exec(strings.ReplaceAll(testSchema, "CREATE TABLE ", "CREATE TABLE IF NOT EXISTS ")); err != nil {
		t.Fatal(err)
	}
	if _, err = e.DB.Exec("INSERT INTO rules(owner,source,target,source_label,target_label) VALUES(1,-100123,222,'src','dst')"); err != nil {
		t.Fatal(err)
	}
	r, err := e.Rule(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	return e, transport, r
}
func TestRetryCooldownDuplicateSixAttempts(t *testing.T) {
	e, tr, r := setup(t)
	ctx := context.Background()
	key := deliveryKey{r.ID, 7}
	e.track(r, 7, "https://example.org", []int{100})
	fail := Message{Chat: 222, Sender: 222, Text: "分享生成 STRM 文件失败\nhttps://example.org", Reply: 100}
	e.failure(ctx, fail, []Rule{r})
	due := e.Queue[key].Due
	e.failure(ctx, fail, []Rule{r})
	if !e.Queue[key].Due.Equal(due) {
		t.Fatal("duplicate reset cooldown")
	}
	e.retry(ctx, []Rule{r})
	if tr.sent != 0 {
		t.Fatal("early retry")
	}
	for i := 1; i <= 6; i++ {
		e.Queue[key].Due = time.Time{}
		e.retry(ctx, []Rule{r})
		if e.Queue[key].Attempts != i {
			t.Fatal("attempt mismatch")
		}
		fail.Reply = 100 + i
		e.failure(ctx, fail, []Rule{r})
	}
	if len(e.Queue) != 0 || len(e.Receipts) != 0 || tr.sent != 6 {
		t.Fatalf("queue=%d receipts=%d sends=%d", len(e.Queue), len(e.Receipts), tr.sent)
	}
}
func TestRetryPauseErrorAndAmbiguousReply(t *testing.T) {
	e, tr, r := setup(t)
	ctx := context.Background()
	e.track(r, 7, "https://example.org", []int{100})
	e.track(r, 8, "https://example.org", []int{200})
	m := Message{Chat: 222, Sender: 222, Text: "分享生成 STRM 文件失败 https://example.org"}
	e.failure(ctx, m, []Rule{r})
	if e.Queue[deliveryKey{1, 7}].State != "waiting" {
		t.Fatal("ambiguous reply retried")
	}
	m.Reply = 100
	e.failure(ctx, m, []Rule{r})
	e.Queue[deliveryKey{1, 7}].Due = time.Time{}
	r.Enabled = false
	e.retry(ctx, []Rule{r})
	if tr.sent != 0 {
		t.Fatal("paused rule sent")
	}
	r.Enabled = true
	r.RetryMax = 1
	tr.fail = true
	e.retry(ctx, []Rule{r})
	if e.Queue[deliveryKey{1, 7}] != nil {
		t.Fatal("exhausted record retained")
	}
}
func TestDeliveryDedupAndCursor(t *testing.T) {
	e, tr, r := setup(t)
	ctx := context.Background()
	m := Message{Chat: r.Source, ID: 7, Text: "https://example.org"}
	if err := e.deliver(ctx, r, m); err != nil {
		t.Fatal(err)
	}
	if err := e.deliver(ctx, r, m); err != nil {
		t.Fatal(err)
	}
	if tr.forwarded != 1 {
		t.Fatal("duplicate send")
	}
	tr.history = []Message{m, {Chat: r.Source, ID: 8, Text: "next"}}
	if err := e.drain(ctx, r); err != nil {
		t.Fatal(err)
	}
	saved, _ := e.Rule(ctx, 1, 1)
	if saved.Cursor != 8 || tr.forwarded != 2 {
		t.Fatal(saved.Cursor, tr.forwarded)
	}
}
func TestKeywordsAndBoundedCache(t *testing.T) {
	if !Accepted("STRASSE", "white", []string{"straße"}) || Accepted("BLOCK", "black", []string{"block"}) || Accepted("anything", "white", nil) {
		t.Fatal("keyword semantics")
	}
	e, _, r := setup(t)
	for i := 0; i < 550; i++ {
		e.track(r, i, "https://example.org", []int{i})
	}
	e.prune()
	if len(e.Queue) > 500 {
		t.Fatal("cache not bounded")
	}
	for _, d := range e.Queue {
		d.SentAt = time.Now().Add(-25 * time.Hour)
	}
	e.prune()
	if len(e.Queue) != 0 || len(e.Receipts) != 0 {
		t.Fatal("cache TTL")
	}
}
func TestRuleUIOwnerAndRetrySettings(t *testing.T) {
	e, _, _ := setup(t)
	ctx := context.Background()
	m := tg.Message{From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1, Type: "private"}}
	for _, cb := range []string{"fw:rule:1", "fw:retry:1:0", "fw:retrycfg:1", "fw:retryclear:1", "fw:logs:1"} {
		handled, err := e.Handle(ctx, m, cb, "", nil)
		if err != nil || !handled {
			t.Fatalf("%s: %v", cb, err)
		}
	}
	if _, err := e.Handle(ctx, m, "", "cs", []string{"2", "3"}); err != nil {
		t.Fatal(err)
	}
	r, _ := e.Rule(ctx, 1, 1)
	if r.RetryMinutes != 2 || r.RetryMax != 3 {
		t.Fatal(r)
	}
	m.From.ID = 2
	if _, err := e.Handle(ctx, m, "fw:rule:1", "", nil); err == nil {
		t.Fatal("owner bypass")
	}
}
