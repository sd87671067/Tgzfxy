package sbbot

import (
	"context"
	"encoding/json"
	"errors"
	tg "local/bots/internal/telegram"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testDoc() Doc {
	return Doc{"inbounds": []any{Doc{"type": "anytls", "tag": "alice", "listen_port": 443, "users": []any{Doc{"password": "p@ss:word"}}, "tls": Doc{"server_name": "legacy.example.com"}}, Doc{"type": "hysteria2", "tag": "bob", "listen_port": 444, "users": []any{Doc{"password": "p"}}, "tls": Doc{"server_name": "example.com"}}}, "outbounds": []any{Doc{"tag": "direct", "type": "direct"}, Doc{"tag": "proxy", "type": "socks", "server": "example.org", "server_port": 1080}}, "route": Doc{"rules": []any{Doc{"inbound": []any{"alice", "bob"}, "outbound": "proxy"}}}}
}
func TestProtocolLinks(t *testing.T) {
	cases := []struct {
		link               string
		kind, pass, server string
	}{
		{"anytls://p%40ss%3Aword@[::1]:443/?sni=example.com&insecure=0#test", "anytls", "p@ss:word", "::1"},
		{"hy2://user:pass@example.com?obfs=salamander&obfs-password=x%26y&alpn=h3", "hysteria2", "user:pass", "example.com"},
		{"socks5://u:p@example.com:1080", "socks", "p", "example.com"},
		{"server: example.com\nport: 1080\nuser: u\npass: p", "socks", "p", "example.com"},
		{"https://u:p@example.com", "http", "p", "example.com"},
	}
	for _, c := range cases {
		d, err := ParseOutbound(c.link)
		if err != nil {
			t.Fatal(err)
		}
		if str(d["type"]) != c.kind || str(d["password"]) != c.pass || str(d["server"]) != c.server {
			t.Fatalf("parsed mismatch: %#v", d)
		}
	}
	for _, link := range []string{"anytls://x@example.com:0", "anytls://x@example.com:65536", "anytls://example.com:443", "vless://bad@example.com?security=reality", "hy2://x@example.com?obfs=salamander", "hy2://x@example.com?mport=600-500", "anytls://x@example.com?insecure=maybe"} {
		if _, err := ParseOutbound(link); err == nil {
			t.Errorf("accepted invalid link %s", link)
		}
	}
	links, err := ShareLinks(testDoc(), []string{"alice"}, "::1", "new.example.com")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(links[0])
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "[::1]:443" || u.Query().Get("sni") != "legacy.example.com" || u.Query().Get("insecure") != "0" {
		t.Fatal(links)
	}
	out, err := ParseOutbound(links[0])
	if err != nil || str(out["password"]) != "p@ss:word" {
		t.Fatal(out, err)
	}
}
func TestBindingDeletionPreservesBlocks(t *testing.T) {
	d := testDoc()
	SetBlockedDoc(d, map[string]bool{"alice": true})
	if err := Bind(d, "direct", "alice"); err != nil {
		t.Fatal(err)
	}
	rules := list(obj(d["route"])["rules"])
	if str(obj(rules[0])["outbound"]) != ManagedBlock || str(obj(rules[1])["outbound"]) != "direct" {
		t.Fatal(rules)
	}
	if err := Bind(d, "alice", "bob"); err == nil {
		t.Fatal("accepted two inbounds")
	}
	if _, err := Delete(d, "outbound", "direct"); err == nil {
		t.Fatal("deleted direct")
	}
	if _, err := Delete(d, "outbound", "proxy"); err != nil {
		t.Fatal(err)
	}
	for _, v := range list(obj(d["route"])["rules"]) {
		if str(obj(v)["outbound"]) == "proxy" {
			t.Fatal("dangling route")
		}
	}
}
func TestApplyRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := []byte("{\"inbounds\":[]}\n")
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	runs := 0
	c := ConfigManager{Path: path, AutoRestart: true, Run: func(context.Context, string, ...string) (string, error) {
		runs++
		if runs == 1 {
			return "", errors.New("invalid config")
		}
		return "", nil
	}}
	if err := c.Apply(context.Background(), testDoc(), nil); err == nil {
		t.Fatal("accepted failed validation")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != string(old) || runs != 3 {
		t.Fatalf("rollback failed: %s runs=%d", raw, runs)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe permissions")
	}
}
func testBot(t *testing.T) (*Bot, *[]map[string]any) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "sbbot.db"), 500<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	path := filepath.Join(dir, "config.json")
	raw, _ := json.Marshal(testDoc())
	os.WriteFile(path, raw, 0600)
	calls := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		p["method"] = filepath.Base(r.URL.Path)
		calls = append(calls, p)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(server.Close)
	b := &Bot{API: &tg.Client{BaseURL: server.URL + "/", HTTP: server.Client()}, Store: store, Config: &ConfigManager{Path: path, Run: func(context.Context, string, ...string) (string, error) { return "Status: active", nil }}, Admins: map[int64]bool{1: true}, Features: map[string]bool{"singbox": true, "ufw": true, "bwh": true}, TZ: time.UTC, HTTP: server.Client(), Host: "example.org"}
	b.InitStates()
	return b, &calls
}
func TestTrafficDeltasAndRestartCompatibility(t *testing.T) {
	b, _ := testBot(t)
	ctx := context.Background()
	payload := func(up int64) Doc {
		return Doc{"connections": []any{Doc{"id": "x", "metadata": Doc{"inbound": "alice"}, "uploadTotal": up, "downloadTotal": int64(10)}}}
	}
	for _, n := range []int64{100, 140, 5, 10} {
		if err := b.Store.Sample(ctx, payload(n), testDoc()); err != nil {
			t.Fatal(err)
		}
	}
	used, err := b.Store.Usage(ctx, "alice", time.Unix(0, 0))
	if err != nil || used != 45 {
		t.Fatalf("usage=%d err=%v", used, err)
	}
	if err = b.Store.Sample(ctx, Doc{}, testDoc()); err != nil {
		t.Fatal(err)
	}
	var n int
	b.Store.DB.QueryRow("SELECT COUNT(*) FROM connections").Scan(&n)
	if n != 0 {
		t.Fatal("stale connections")
	}
	if err = b.Store.SetManual(ctx, map[string]bool{"alice": true}); err != nil {
		t.Fatal(err)
	}
	manual, err := b.Store.Manual(ctx)
	if err != nil || !manual["alice"] {
		t.Fatal(manual, err)
	}
}
func TestEnforceManualQuotaExpiry(t *testing.T) {
	b, _ := testBot(t)
	ctx := context.Background()
	b.Store.SetManual(ctx, map[string]bool{"alice": true})
	b.Store.DB.Exec("INSERT INTO proto_expiry VALUES('bob',?)", time.Now().Unix()-1)
	d := testDoc()
	if err := b.enforce(ctx, d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(Blocked(d), map[string]bool{"alice": true, "bob": true}) {
		t.Fatal(Blocked(d))
	}
	b.Store.SetManual(ctx, map[string]bool{})
	b.Store.SetExpiry(ctx, "bob", 0)
	if err := b.enforce(ctx, d); err != nil {
		t.Fatal(err)
	}
	if len(Blocked(d)) != 0 {
		t.Fatal("failed to unblock")
	}
}

func TestEnforceEmptyDoesNotRestart(t *testing.T) {
	b, _ := testBot(t)
	d := testDoc()
	b.Config.Run = func(context.Context, string, ...string) (string, error) {
		t.Fatal("unchanged quota state must not restart sing-box")
		return "", nil
	}
	if err := b.enforce(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if Kind(d, ManagedBlock) != "" {
		t.Fatal("unnecessary block outbound added")
	}
}
func TestAdminCommandsAndButtons(t *testing.T) {
	b, calls := testBot(t)
	ctx := context.Background()
	m := tg.Message{From: tg.User{ID: 2}, Chat: tg.Chat{ID: 2, Type: "private"}, Text: "/limit alice 0"}
	if err := b.Handle(ctx, tg.Update{Message: &m}); err != nil {
		t.Fatal(err)
	}
	lim, _ := b.Store.Limit(ctx, "alice")
	if lim == 0 {
		t.Fatal("unauthorized mutation")
	}
	m.From.ID = 1
	m.Chat.ID = 1
	for _, cmd := range []string{"/panel", "/help", "/today", "/month", "/status", "/usage 443", "/limit alice 0", "/setexpiry bob 7d", "/share alice", "/protocols", "/ufw list", "/addvless", "/addhy2", "/addanytls", "/add", "/cancel"} {
		m.Text = cmd
		if err := b.Handle(ctx, tg.Update{Message: &m}); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
	for _, cb := range []string{"proto:menu", "proto:list:0", "proto:exp", "use:month", "blocks", "logs", "bind:start", "bind:pick:alice", "bind:pick:direct", "bind:save", "share", "sharepick:alice", "sharedone", "delete:start", "delete:pick:outbound:proxy", "delete:save", "ufw:list", "panel"} {
		u := tg.Update{Callback: &tg.Callback{ID: "x", From: m.From, Message: &m, Data: selectedCallback(b.States[m.From.ID], cb)}}
		if err := b.Handle(ctx, u); err != nil {
			t.Fatalf("%s: %v", cb, err)
		}
		last := (*calls)[len(*calls)-1]
		if strings.HasPrefix(str(last["text"]), "操作失败") {
			t.Fatalf("%s: %v", cb, last)
		}
	}
	lim, _ = b.Store.Limit(ctx, "alice")
	if lim != b.Store.DefaultLimit {
		t.Fatal("removed limit command changed persisted monthly quota")
	}
	d, _ := Load(b.Config.Path)
	if Kind(d, "proxy") != "" {
		t.Fatal("delete failed")
	}
}
func TestSnapshotConfirmOwnershipAndReplay(t *testing.T) {
	b, _ := testBot(t)
	requests := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		w.Header().Set("Content-Type", "text/plain")
		if r.URL.Path == "/snapshot/list" {
			w.Write([]byte(`{"error":0,"snapshots":[{"fileName":"snapshot-long-name","description":"sample"}]}`))
		} else {
			w.Write([]byte(`{"error":0}`))
		}
	}))
	defer srv.Close()
	b.BWHURL = srv.URL
	b.HTTP = srv.Client()
	m := tg.Message{From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1, Type: "private"}}
	st := &pending{}
	ctx := context.Background()
	if err := b.bwhCallback(ctx, m, "snaprestore:"+snapID("snapshot-long-name"), st); err != nil {
		t.Fatal(err)
	}
	token := st.Token
	if err := b.bwhCallback(ctx, m, "snapconfirm:wrong", st); err == nil {
		t.Fatal("accepted wrong confirmation")
	}
	if err := b.bwhCallback(ctx, m, "snapconfirm:"+token, st); err != nil {
		t.Fatal(err)
	}
	if err := b.bwhCallback(ctx, m, "snapconfirm:"+token, st); err == nil {
		t.Fatal("replayed confirmation")
	}
	n := 0
	for _, p := range requests {
		if p == "/snapshot/restore" {
			n++
		}
	}
	if n != 1 {
		t.Fatal(requests)
	}
}
func TestYesterdayReportExcludesToday(t *testing.T) {
	b, calls := testBot(t)
	ctx := context.Background()
	now := time.Now().UTC()
	b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?),(?,?,?)", Day(now).AddDate(0, 0, -1).Unix()+1, "alice", 100, Day(now).Unix()+1, "alice", 900)
	if err := b.Daily(ctx, now.Hour()); err != nil {
		t.Fatal(err)
	}
	last := (*calls)[len(*calls)-1]
	if !strings.Contains(str(last["text"]), "100.00 B") || strings.Contains(str(last["text"]), "900.00 B") {
		t.Fatal(last)
	}
	n := len(*calls)
	if err := b.Daily(ctx, now.Hour()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != n {
		t.Fatal("duplicate report")
	}
}
