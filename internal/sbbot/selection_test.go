package sbbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tg "local/bots/internal/telegram"
)

func TestSelectionPanel(t *testing.T) {
	d := Doc{"inbounds": []any{Doc{"tag": "in1"}, Doc{"tag": "in2"}, Doc{"tag": "in3"}}, "outbounds": []any{Doc{"tag": "out1"}, Doc{"tag": "out2"}, Doc{"tag": "direct"}}}
	calls := 0
	var body struct {
		MessageID int64       `json:"message_id"`
		Keyboard  tg.Keyboard `json:"reply_markup"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/editMessageText" {
			t.Errorf("unexpected API: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	b := &Bot{API: &tg.Client{BaseURL: server.URL + "/", HTTP: server.Client()}}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(d)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	b.Config = &ConfigManager{Path: path}
	m := tg.Message{ID: 42, Chat: tg.Chat{ID: 7}}
	st := &pending{Mode: "bind"}
	for _, cb := range []string{"bind:pick:in1", "bind:pick:in2", "bind:pick:out1", "bind:pick:out2"} {
		if err := b.callback(context.Background(), m, selectedCallback(st, cb), st); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(st.Tags, ",") != "in1,in2,out2" {
		t.Fatalf("selection: %v", st.Tags)
	}
	if calls != 4 || body.MessageID != 42 {
		t.Fatalf("calls=%d message=%d", calls, body.MessageID)
	}
	if len(body.Keyboard.Rows[0]) != 2 || len(body.Keyboard.Rows[1]) != 1 || len(body.Keyboard.Rows[2]) != 2 {
		t.Fatalf("layout: %+v", body.Keyboard)
	}
	if err := b.callback(context.Background(), m, selectedCallback(st, "bind:pick:in1"), st); err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.Tags, ",") != "in2,out2" {
		t.Fatalf("toggle: %v", st.Tags)
	}
	st = &pending{Mode: "delete"}
	if err := b.callback(context.Background(), m, "delete:pick:inbound:in2", st); err != nil {
		t.Fatal(err)
	}
	rows := body.Keyboard.Rows
	if rows[len(rows)-1][0].Text != "确认删除" {
		t.Fatalf("delete label: %+v", rows)
	}
	if len(rows[0]) != 2 {
		t.Fatal("delete layout")
	}
}
