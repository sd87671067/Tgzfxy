package sbbot

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	tg "local/bots/internal/telegram"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unifiedDoc(t *testing.T) Doc {
	t.Helper()
	d := testDoc()
	key, e := ecdh.X25519().GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	d["inbounds"] = append(list(d["inbounds"]), Doc{"type": "vless", "tag": "legacy", "listen": "127.0.0.1", "listen_port": 19333, "users": []any{Doc{"uuid": "11111111-1111-4111-8111-111111111111"}}, "tls": Doc{"enabled": true, "server_name": "www.samsung.com", "reality": Doc{"enabled": true, "private_key": base64.RawURLEncoding.EncodeToString(key.Bytes()), "short_id": []any{"aabbccdd"}, "handshake": Doc{"server": "www.samsung.com", "server_port": 443}}}})
	d, e = StageUnified(d, "legacy", "sbbot-unified-main", "127.0.0.1", 19444)
	if e != nil {
		t.Fatal(e)
	}
	for _, x := range []struct{ n, o string }{{"one", "direct"}, {"two", "proxy"}} {
		if _, e = AddUnifiedUser(d, "sbbot-unified-main", x.n, "", x.o); e != nil {
			t.Fatal(e)
		}
	}
	return d
}
func installDoc(t *testing.T, b *Bot, d Doc) {
	t.Helper()
	raw, _ := json.Marshal(d)
	if e := os.WriteFile(b.Config.Path, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestUnifiedBindingsLinksAndLegacyPreservation(t *testing.T) {
	d := unifiedDoc(t)
	before := clone(d)
	users := UnifiedUsers(d)
	if len(users) != 2 {
		t.Fatal(users)
	}
	if e := ValidateUnified(d); e != nil {
		t.Fatal(e)
	}
	if e := Bind(d, "sbbot-unified-main", "direct"); e == nil {
		t.Fatal("aggregate binding accepted")
	}
	if _, e := AddUnifiedUser(d, "sbbot-unified-main", "another", users[0].UUID, "direct"); e == nil {
		t.Fatal("duplicate accepted")
	}
	if e := BindUser(d, users[0].Key, "proxy"); e != nil {
		t.Fatal(e)
	}
	if configVersion(Doc{"inbounds": before["inbounds"]}) != configVersion(Doc{"inbounds": d["inbounds"]}) {
		t.Fatal("legacy/user identities changed")
	}
	if _, e := Delete(d, "outbound", "proxy"); e == nil {
		t.Fatal("referenced exit deleted")
	}
	links, e := ShareLinks(d, []string{"sbbot-unified-main"}, "example.org", "")
	if e != nil || len(links) != 2 {
		t.Fatal(links, e)
	}
	for i, link := range links {
		out, e := ParseOutbound(link)
		if e != nil || str(out["uuid"]) != users[i].UUID || str(out["flow"]) != "xtls-rprx-vision" {
			t.Fatal(out, e)
		}
	}
	SetBlockedDoc(d, map[string]bool{users[0].Key: true})
	if !Blocked(d)[users[0].Key] || Blocked(d)[users[1].Key] {
		t.Fatal(Blocked(d))
	}
	if e := BindUser(d, users[0].Key, "direct"); e != nil {
		t.Fatal(e)
	}
	if !Blocked(d)[users[0].Key] {
		t.Fatal("bind bypassed pause")
	}
	if e := ValidateUnified(d); e != nil {
		t.Fatal(e)
	}
	SetBlockedDoc(d, map[string]bool{})
	if len(Blocked(d)) != 0 {
		t.Fatal("resume failed")
	}
}
func TestUnifiedTrafficUsesRealClashChains(t *testing.T) {
	b, _ := testBot(t)
	d := unifiedDoc(t)
	us := UnifiedUsers(d)
	ctx := context.Background()
	payload := func(a, b int) Doc {
		return Doc{"connections": []any{Doc{"id": "a", "metadata": Doc{"type": "vless/sbbot-unified-main"}, "chains": []string{"direct", userOutbound(us[0].Key)}, "upload": a, "download": 0}, Doc{"id": "b", "metadata": Doc{"type": "vless/sbbot-unified-main"}, "chains": []string{"proxy", userOutbound(us[1].Key)}, "upload": b, "download": 0}, Doc{"id": "unknown", "metadata": Doc{"type": "vless/sbbot-unified-main"}, "chains": []string{"direct"}, "upload": 9000}}}
	}
	for _, v := range [][2]int{{100, 200}, {150, 280}, {150, 280}, {5, 8}} {
		if e := b.Store.Sample(ctx, payload(v[0], v[1]), d); e != nil {
			t.Fatal(e)
		}
	}
	for i, want := range []int64{155, 288} {
		n, e := b.Store.Usage(ctx, us[i].Key, time.Unix(0, 0))
		if e != nil || n != want {
			t.Fatalf("user %d: %d want %d: %v", i, n, want, e)
		}
	}
	if _, e := b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?)", time.Now().Unix(), "legacy", 99); e != nil {
		t.Fatal(e)
	}
	installDoc(t, b, d)
	b.Config.AutoRestart = true
	if e := b.Store.SetLimit(ctx, us[0].Key, 100); e != nil {
		t.Fatal(e)
	}
	if e := b.enforce(ctx, d); e != nil {
		t.Fatal(e)
	}
	if !Blocked(d)[us[0].Key] || Blocked(d)[us[1].Key] {
		t.Fatal(Blocked(d))
	}
	if e := b.Store.SetExpiry(ctx, us[1].Key, -1); e != nil {
		t.Fatal(e)
	}
	if e := b.enforce(ctx, d); e != nil {
		t.Fatal(e)
	}
	if !Blocked(d)[us[1].Key] {
		t.Fatal("expiry not enforced")
	}
	if n, _ := b.Store.Usage(ctx, "legacy", time.Unix(0, 0)); n != 99 {
		t.Fatal("legacy usage changed")
	}
}
func TestUnifiedApplyPrecheckAndRollback(t *testing.T) {
	d := unifiedDoc(t)
	path := filepath.Join(t.TempDir(), "config.json")
	old := []byte("{\"inbounds\":[]}\n")
	os.WriteFile(path, old, 0600)
	c := ConfigManager{Path: path}
	if e := c.ApplyUnified(context.Background(), d); e == nil {
		t.Fatal("disabled restart accepted")
	}
	c.AutoRestart = true
	runs := 0
	c.Run = func(context.Context, string, ...string) (string, error) {
		runs++
		if runs == 1 {
			return "", errors.New("candidate invalid")
		}
		return "", nil
	}
	if e := c.ApplyUnified(context.Background(), d); e == nil {
		t.Fatal("invalid candidate accepted")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != string(old) || runs != 1 {
		t.Fatal("precheck wrote active file")
	}
	runs = 0
	c.Run = func(_ context.Context, program string, args ...string) (string, error) {
		runs++
		if program == "/usr/bin/systemctl" && args[0] == "is-active" {
			return "", errors.New("dead")
		}
		return "", nil
	}
	if e := c.ApplyUnified(context.Background(), d); e == nil {
		t.Fatal("dead service accepted")
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != string(old) || runs != 6 {
		t.Fatalf("rollback raw=%s runs=%d", raw, runs)
	}
}
func TestUnifiedTelegramConfirmationOwnershipStalenessAndReplay(t *testing.T) {
	b, calls := testBot(t)
	d := unifiedDoc(t)
	installDoc(t, b, d)
	b.Config.AutoRestart = true
	ctx := context.Background()
	m := tg.Message{ID: 42, From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1, Type: "private"}}
	st := &pending{}
	if e := b.userMenu(ctx, m, d, st); e != nil {
		t.Fatal(e)
	}
	token := st.Token
	u := UnifiedUsers(d)[0]
	if e := b.userCallback(ctx, m, d, "user:pick:"+token+":"+ref(u.Key), st); e != nil {
		t.Fatal(e)
	}
	if e := b.userCallback(ctx, m, d, "user:out:"+token+":"+ref("proxy"), st); e != nil {
		t.Fatal(e)
	}
	cb := "user:confirm:" + st.Token + ":" + ref(u.Key)
	other := m
	other.Chat.ID = 99
	if e := b.userCallback(ctx, other, d, cb, st); e == nil {
		t.Fatal("cross-chat accepted")
	}
	changed := clone(d)
	changed["log"] = Doc{"level": "debug"}
	if e := b.userCallback(ctx, m, changed, cb, st); e == nil {
		t.Fatal("stale config accepted")
	}
	if e := b.userCallback(ctx, m, d, cb, st); e != nil {
		t.Fatal(e)
	}
	if e := b.userCallback(ctx, m, d, cb, st); e == nil {
		t.Fatal("replay accepted")
	}
	loaded, e := Load(b.Config.Path)
	if e != nil || UnifiedUsers(loaded)[0].Outbound != "proxy" {
		t.Fatal("binding not persisted", e)
	}
	for _, c := range *calls {
		if k, ok := c["reply_markup"].(map[string]any); ok {
			raw, _ := json.Marshal(k)
			if strings.Contains(string(raw), "password") {
				t.Fatal("secret in keyboard")
			}
		}
	}
}
func TestMigrationProposalKeepsOldClients(t *testing.T) {
	d := unifiedDoc(t)
	d["inbounds"] = list(d["inbounds"])[2:3]
	in := clone(obj(list(d["inbounds"])[0]))
	in["tag"] = "legacy-other"
	in["listen_port"] = 19334
	d["inbounds"] = append(list(d["inbounds"]), in)
	nested(d, "route")["final"] = "direct"
	nested(d, "route")["rules"] = []any{Doc{"inbound": []string{"legacy-other"}, "outbound": "proxy"}}
	before := configVersion(d)
	plan, e := PlanMigration(d)
	if e != nil || len(plan) != 2 {
		t.Fatal(plan, e)
	}
	if plan[0].ProposedUUID != plan[0].OldUUID || plan[1].ProposedUUID == plan[1].OldUUID || !plan[0].Conflict || plan[1].Outbound != "proxy" {
		t.Fatal(plan)
	}
	if configVersion(d) != before {
		t.Fatal("source modified")
	}
	staged, e := StageUnified(d, "legacy", "sbbot-unified-new", "127.0.0.1", 19555)
	if e != nil {
		t.Fatal(e)
	}
	for _, u := range plan {
		if _, e = AddUnifiedUser(staged, "sbbot-unified-new", u.ProposedName, u.ProposedUUID, u.Outbound); e != nil {
			t.Fatal(e)
		}
	}
	if e = ValidateUnified(staged); e != nil {
		t.Fatal(e)
	}
}

func TestUnifiedHistoryAdoptionPoliciesReportingAndRollback(t *testing.T) {
	b, calls := testBot(t)
	ctx := context.Background()
	d := unifiedDoc(t)
	installDoc(t, b, d)
	b.Config.AutoRestart = true
	u := UnifiedUsers(d)[0]
	b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?),(?,?,?)", time.Now().Unix(), "legacy", 80, time.Now().Unix(), u.Key, 30)
	b.Store.SetLimit(ctx, "legacy", 100)
	b.Store.SetExpiry(ctx, "legacy", 7)
	oldExpiry, _ := b.Store.Expiry(ctx, "legacy")
	if err := b.Store.AdoptLegacy(ctx, u.Key, "legacy", func() error { return b.enforce(ctx, d) }); err != nil {
		t.Fatal(err)
	}
	if n, _ := b.Store.Usage(ctx, u.Key, time.Unix(0, 0)); n != 110 {
		t.Fatal("history not included", n)
	}
	all, err := b.Store.UsageAll(ctx, time.Unix(0, 0))
	if err != nil || all[u.Key] != 110 || all["legacy"] != 80 {
		t.Fatal(all, err)
	}
	if n, _ := b.Store.Limit(ctx, u.Key); n != 100 {
		t.Fatal(n)
	}
	if n, _ := b.Store.Expiry(ctx, u.Key); n != oldExpiry {
		t.Fatal(n, oldExpiry)
	}
	if !Blocked(d)[u.Key] || !Blocked(d)["legacy"] {
		t.Fatal("combined quota did not pause old and new clients", Blocked(d))
	}
	if err = b.Store.AttachLegacy(ctx, u.Key, "alice"); err == nil {
		t.Fatal("overwrote history link")
	}
	rows, err := b.reportingInbounds(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range rows {
		if str(obj(raw)["tag"]) == "legacy" {
			t.Fatal("double counted legacy display")
		}
	}
	if err = b.Store.UpdateUserPolicy(ctx, u.Key, "limits", 0, func() error { return errors.New("injected") }); err == nil {
		t.Fatal("failure accepted")
	}
	if n, _ := b.Store.Limit(ctx, u.Key); n != 100 {
		t.Fatal("policy rollback failed", n)
	}
	second := UnifiedUsers(d)[1]
	b.Store.SetManual(ctx, map[string]bool{"bob": true})
	if err = b.Store.AdoptLegacy(ctx, second.Key, "bob", func() error { return errors.New("injected") }); err == nil {
		t.Fatal("adoption failure accepted")
	}
	links, err := b.Store.LegacyLinks(ctx)
	if err != nil || links[second.Key] != "" {
		t.Fatal(links, err)
	}
	manual, _ := b.Store.Manual(ctx)
	if len(manual) != 1 || !manual["bob"] {
		t.Fatal("manual rollback", manual)
	}
	// Commands retain single-user policies and display their accumulated history.
	m := tg.Message{From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1, Type: "private"}}
	st := &pending{}
	for _, cmd := range []struct {
		c string
		a []string
	}{{"limit", []string{u.Key, "0"}}, {"setexpiry", []string{u.Key, "0"}}, {"usage", []string{u.Key}}} {
		if err = b.command(ctx, m, cmd.c, cmd.a, st); err != nil {
			t.Fatal(cmd, err)
		}
	}
	last := (*calls)[len(*calls)-1]
	if !strings.Contains(str(last["text"]), "110.00 B") {
		t.Fatal(last)
	}
	yesterday := Day(time.Now().UTC()).AddDate(0, 0, -1).Unix() + 1
	if _, err = b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?),(?,?,?)", yesterday, "legacy", 20, yesterday, u.Key, 30); err != nil {
		t.Fatal(err)
	}
	if err = b.Daily(ctx, time.Now().UTC().Hour()); err != nil {
		t.Fatal(err)
	}
	report := str((*calls)[len(*calls)-1]["text"])
	if !strings.Contains(report, "合计：50.00 B") || strings.Contains(report, "legacy：") {
		t.Fatal("history report duplicated", report)
	}

}

func TestUnifiedValidationRejectsBypassesAndBadAliases(t *testing.T) {
	for _, which := range []string{"global", "missing-guard", "direct-route", "alias-loop", "duplicate-route"} {
		t.Run(which, func(t *testing.T) {
			d := unifiedDoc(t)
			u := UnifiedUsers(d)[0]
			r := nested(d, "route")
			switch which {
			case "global":
				r["rules"] = append([]any{Doc{"action": "route", "outbound": "direct"}}, list(r["rules"])...)
			case "missing-guard":
				var rules []any
				for _, raw := range list(r["rules"]) {
					if str(obj(raw)["action"]) != "reject" {
						rules = append(rules, raw)
					}
				}
				r["rules"] = rules
			case "direct-route":
				for _, raw := range list(r["rules"]) {
					if ruleForUser(obj(raw), u) {
						obj(raw)["outbound"] = "direct"
					}
				}
			case "alias-loop":
				for _, raw := range list(d["outbounds"]) {
					if str(obj(raw)["tag"]) == userOutbound(u.Key) {
						obj(raw)["outbounds"] = []string{userOutbound(u.Key)}
					}
				}
			case "duplicate-route":
				r["rules"] = append([]any{Doc{"inbound": []string{u.Inbound}, "auth_user": []string{u.Name}, "action": "route", "outbound": userOutbound(u.Key)}}, list(r["rules"])...)
			}
			if err := ValidateUnified(d); err == nil {
				t.Fatal("accepted bypass")
			}
		})
	}
}

func TestLegacyCreatorRejectsReservedUnifiedPrefix(t *testing.T) {
	c := ConfigManager{}
	before := configVersion(testDoc())
	d := testDoc()
	if _, err := c.AddInbound(context.Background(), d, "vless", "sbbot-unified-bypass", ""); err == nil {
		t.Fatal("legacy creator accepted reserved unified tag")
	}
	if configVersion(d) != before {
		t.Fatal("legacy rejection mutated config")
	}
}

func TestUnifiedPublicPortPreservesLegacyLinks(t *testing.T) {
	d := unifiedDoc(t)
	users := UnifiedUsers(d)
	var legacy string
	for _, raw := range list(d["inbounds"]) {
		if !isUnified(obj(raw)) && str(obj(raw)["type"]) == "vless" {
			legacy = str(obj(raw)["tag"])
			break
		}
	}
	old, err := ShareLinks(d, []string{legacy}, "example.org", "")
	if err != nil {
		t.Fatal(err)
	}
	links, err := ShareLinks(d, []string{users[0].Key, legacy}, "example.org", "", 443)
	if err != nil || len(links) != 2 {
		t.Fatal(err)
	}
	parsed, err := ParseOutbound(links[0])
	if err != nil || integer(parsed["server_port"]) != 443 {
		t.Fatal("public port missing")
	}
	if links[1] != old[0] {
		t.Fatal("legacy link changed")
	}
	if _, err := ShareLinks(d, []string{users[0].Key}, "example.org", "", 65536); err == nil {
		t.Fatal("invalid port accepted")
	}
}
