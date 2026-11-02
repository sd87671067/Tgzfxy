package sbbot

import (
	"bytes"
	"context"
	"encoding/json"
	tg "local/bots/internal/telegram"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBotBridgeMenuSaveSnapshotRefreshAndFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	socket := filepath.Join(dir, "control.sock")
	raw := []byte(`{"inbounds":[],"outbounds":[{"tag":"direct","type":"direct"}],"extension":{"number":9007199254740993}}`)
	os.WriteFile(path, raw, 0600)
	ln, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	failed := false
	received := false
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Base      []byte `json:"base_bytes"`
			Candidate json.RawMessage
			Hash      string
		}
		if e := json.NewDecoder(r.Body).Decode(&v); e != nil {
			t.Error(e)
		}
		received = true
		if !bytes.Equal(v.Base, raw) {
			t.Error("base snapshot changed")
		}
		if failed {
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"conflict-refresh"}`))
			return
		}
		var d Doc
		json.Unmarshal(v.Candidate, &d)
		d["peer-field"] = "fresh"
		now, _ := json.Marshal(d)
		os.WriteFile(path, now, 0600)
		w.Write([]byte(`{"ok":true}`))
	})}
	defer server.Close()
	go server.Serve(ln)
	c := &ConfigManager{Path: path, ControlSocket: socket}
	d, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	d["outbounds"] = append(list(d["outbounds"]), Doc{"tag": "new", "type": "direct"})
	if e = c.Apply(context.Background(), d, nil); e != nil {
		t.Fatal(e)
	}
	if !received || str(d["peer-field"]) != "fresh" {
		t.Fatal("save not read back")
	}
	bot := &Bot{Features: map[string]bool{"singbox": true}}
	found := false
	for _, row := range bot.panel().Rows {
		for _, button := range row {
			if button.Data == "proto:menu" {
				found = true
			}
		}
	}
	if !found || !bot.CommandAvailable("addvless") {
		t.Fatal("protocol management hidden")
	}
	os.WriteFile(path, raw, 0600)
	d, _ = Load(path)
	d["extension"] = Doc{"number": "modified"}
	failed = true
	if e = c.Apply(context.Background(), d, nil); e == nil {
		t.Fatal("rejected write reported successful")
	}
	now, _ := os.ReadFile(path)
	if !bytes.Equal(now, raw) {
		t.Fatal("failure changed configuration")
	}
}
func TestBotSavePreservesForeignRoutesAndNormalizationOrder(t *testing.T) {
	d := testDoc()
	route := nested(d, "route")
	foreign := Doc{"inbound": []string{"legacy"}, "auth_user": []string{"other"}, "action": "reject", "method": "default", "extension": "keep"}
	route["rules"] = append(list(route["rules"]), foreign)
	before := configVersion(d)
	if e := NormalizeDirectUsers(d); e != nil {
		t.Fatal(e)
	}
	if configVersion(d) != before {
		t.Fatal("unrelated save normalized all users")
	}
	SetBlockedDoc(d, map[string]bool{})
	found := false
	for _, v := range list(obj(d["route"])["rules"]) {
		if str(obj(v)["extension"]) == "keep" {
			found = true
		}
	}
	if !found {
		t.Fatal("foreign reject deleted")
	}
}

func TestBotConfigWatcherRefreshesReadsPreservesEditors(t *testing.T) {
	b, _ := testBot(t)
	d := unifiedDoc(t)
	installDoc(t, b, d)
	m := tg.Message{ID: 17, From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1}}
	b.Admins[1] = true
	st := &pending{}
	b.States[1] = st
	if e := b.userPage(context.Background(), m, d, st, 0, true); e != nil {
		t.Fatal(e)
	}
	original := st.Version
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.WatchConfig(ctx) }()
	time.Sleep(30 * time.Millisecond)
	d["external-field"] = "changed"
	raw, _ := json.Marshal(d)
	tmp := b.Config.Path + ".tmp"
	os.WriteFile(tmp, raw, 0600)
	os.Rename(tmp, b.Config.Path)
	refreshed := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		b.Mu.Lock()
		refreshed = st.Version != original
		b.Mu.Unlock()
		if refreshed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !refreshed {
		t.Fatal("config file event did not refresh Telegram list")
	}
	b.Mu.Lock()
	st.Mode = "user-confirm"
	st.Version = original
	b.Mu.Unlock()
	b.refreshConfigDisplays(context.Background())
	if st.Version != original {
		t.Fatal("unsaved edit silently rebased")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop")
	}
}

func TestBotSharedPauseOwnershipAndQuotaIsolation(t *testing.T) {
	d := unifiedDoc(t)
	u := UnifiedUsers(d)[0]
	setManualPause(d, u.Key, true)
	if !Blocked(d)[u.Key] {
		t.Fatal("manual pause unreadable")
	}
	setQuotaBlocks(d, map[string]bool{u.Key: true})
	if !Blocked(d)[u.Key] {
		t.Fatal("quota pause unreadable")
	}
	setManualPause(d, u.Key, false)
	if !Blocked(d)[u.Key] {
		t.Fatal("manual enable bypassed quota")
	}
	setQuotaBlocks(d, map[string]bool{})
	if Blocked(d)[u.Key] {
		t.Fatal("expired quota rule was restored")
	}
	// A live Web pause is authoritative; stale manual database state is not replayed.
	setManualPause(d, u.Key, true)
	before := configVersion(d)
	setQuotaBlocks(d, map[string]bool{})
	if configVersion(d) != before {
		t.Fatal("quota reconciliation removed Web pause")
	}
}
