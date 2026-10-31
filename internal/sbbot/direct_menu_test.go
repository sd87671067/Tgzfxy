package sbbot

import (
	"context"
	"encoding/json"
	"fmt"
	tg "local/bots/internal/telegram"
	"strings"
	"testing"
	"time"
)

func TestDirectNormalizePersistenceIsolation(t *testing.T) {
	b, _ := testBot(t)
	d := unifiedDoc(t)
	other, err := AddUnifiedUser(d, "sbbot-unified-main", "other", "", "direct")
	if err != nil {
		t.Fatal(err)
	}
	first := UnifiedUsers(d)[0]
	// Reproduce an old persisted direct-only selector.
	d["outbounds"] = append(list(d["outbounds"]), Doc{"tag": userOutbound(first.Key), "type": "selector", "outbounds": []string{"direct"}, "default": "direct"})
	for _, raw := range list(obj(d["route"])["rules"]) {
		if ruleForUser(obj(raw), first) {
			obj(raw)["outbound"] = userOutbound(first.Key)
		}
	}
	identities := configVersion(Doc{"inbounds": d["inbounds"]})
	installDoc(t, b, d)
	ctx := context.Background()
	b.Store.SetLimit(ctx, first.Key, 12345)
	b.Store.SetExpiry(ctx, first.Key, 30)
	expiry, _ := b.Store.Expiry(ctx, first.Key)
	if err = b.Config.Apply(ctx, d, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := Load(b.Config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if Kind(saved, userOutbound(first.Key)) != "" || Kind(saved, userOutbound(other.Key)) != "" {
		t.Fatal("direct selector regenerated")
	}
	if configVersion(Doc{"inbounds": saved["inbounds"]}) != identities {
		t.Fatal("identities changed")
	}
	if err = BindUser(saved, first.Key, "proxy"); err != nil {
		t.Fatal(err)
	}
	got, _ := findUser(saved, other.Key)
	if got.Outbound != "direct" {
		t.Fatal("shared binding changed")
	}
	if err = BindUser(saved, first.Key, "direct"); err != nil {
		t.Fatal(err)
	}
	if err = ValidateUnified(saved); err != nil {
		t.Fatal(err)
	}
	limit, _ := b.Store.Limit(ctx, first.Key)
	exp, _ := b.Store.Expiry(ctx, first.Key)
	if limit != 12345 || exp != expiry {
		t.Fatal("user policy changed")
	}
	payload := Doc{"connections": []any{}}
	for i, u := range []UnifiedUser{first, other} {
		payload["connections"] = append(list(payload["connections"]), Doc{"id": u.Key, "metadata": Doc{"type": "vless/" + u.Inbound}, "chains": []string{"direct"}, "rule": "inbound=" + u.Inbound + " auth_user=" + u.Name + " => route(direct)", "upload": (i + 1) * 100})
	}
	for i := 0; i < 2; i++ {
		if err = b.Store.Sample(ctx, payload, saved); err != nil {
			t.Fatal(err)
		}
	}
	for i, u := range []UnifiedUser{first, other} {
		n, err := b.Store.Usage(ctx, u.Key, time.Unix(0, 0))
		if err != nil || n != int64((i+1)*100) {
			t.Fatal("traffic isolation", n, err)
		}
	}
	for _, rule := range []string{"final", "inbound=" + first.Inbound + " auth_user=" + first.Name + "x => route(direct)", "inbound=wrong auth_user=" + first.Name + " => route(direct)"} {
		if ConnectionAccounting(Doc{"metadata": Doc{"type": "vless/" + first.Inbound}, "chains": []string{"direct"}, "rule": rule}, saved) != "" {
			t.Fatal("ambiguous traffic attributed")
		}
	}
}

func TestUserMenuPaginationFreshnessAndLengths(t *testing.T) {
	b, calls := testBot(t)
	d := unifiedDoc(t)
	for i := 0; i < 15; i++ {
		if _, err := AddUnifiedUser(d, "sbbot-unified-main", fmt.Sprintf("readable_%02d", i), "", "direct"); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	m := tg.Message{ID: 42, Chat: tg.Chat{ID: 1}, From: tg.User{ID: 1}}
	st := &pending{}
	if err := b.userMenu(ctx, m, d, st); err != nil {
		t.Fatal(err)
	}
	m.ID = st.MessageID
	check := func(want int, page string) {
		t.Helper()
		call := (*calls)[len(*calls)-1]
		raw, _ := json.Marshal(call["reply_markup"])
		var k tg.Keyboard
		json.Unmarshal(raw, &k)
		count := 0
		for _, row := range k.Rows {
			for _, bt := range row {
				if len([]byte(bt.Data)) > 64 {
					t.Fatal("oversize callback")
				}
				if strings.HasPrefix(bt.Data, "user:pick:") {
					count++
					if strings.Contains(bt.Text, "sbbot-") || strings.Contains(bt.Text, " · ") {
						t.Fatal("unreadable label", bt.Text)
					}
				}
			}
		}
		if count != want || !strings.Contains(str(call["text"]), page) {
			t.Fatal(count, call["text"])
		}
	}
	check(8, "1 / 3")
	old := st.Token
	cb := "user:page:" + old + ":1"
	if err := b.userCallback(ctx, m, d, cb, st); err != nil {
		t.Fatal(err)
	}
	check(8, "2 / 3")
	if err := b.userCallback(ctx, m, d, cb, st); err == nil {
		t.Fatal("replayed page accepted")
	}
	wrong := m
	wrong.ID++
	if err := b.userCallback(ctx, wrong, d, "user:page:"+st.Token+":2", st); err == nil {
		t.Fatal("wrong message accepted")
	}
	if err := b.userCallback(ctx, m, d, "user:page:"+st.Token+":2", st); err != nil {
		t.Fatal(err)
	}
	check(1, "3 / 3")
	installDoc(t, b, d)
	b.Config.AutoRestart = true
	if err := b.command(ctx, m, "adduser", []string{"sbbot-unified-main", "fresh_user", "direct"}, st); err != nil {
		t.Fatal(err)
	}
	d, err := Load(b.Config.Path)
	if err != nil {
		t.Fatal(err)
	}
	check(2, "3 / 3")
	if st.Page != 2 {
		t.Fatal("add lost current page")
	}
	stale := st.Token
	// Simulate deleting users and their routes; page refresh clamps to new count.
	in := obj(list(d["inbounds"])[3])
	in["users"] = list(in["users"])[:2]
	if err := b.userCallback(ctx, m, d, "user:pick:"+stale+":"+ref(UnifiedUsers(d)[0].Key), st); err == nil {
		t.Fatal("stale pick accepted")
	}
	if err := b.userCallback(ctx, m, d, "user:page:"+stale+":2", st); err != nil {
		t.Fatal(err)
	}
	check(2, "1 / 1")
	st.Until = time.Now().Add(-time.Second)
	if err := b.userCallback(ctx, m, d, "user:page:"+st.Token+":0", st); err == nil {
		t.Fatal("expired button accepted")
	}
}
