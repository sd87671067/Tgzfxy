package sbbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	tg "local/bots/internal/telegram"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEntryCommandBoundsAndShare(t *testing.T) {
	for _, args := range [][]string{nil, {"one", "0"}, {"one", "-1"}, {"one", "1.5"}, {"one", "1", "0"}, {"one", "1", "36501"}, {"one", "8796093022208"}, {"one", "1", "9223372036854775808"}, {"one", "2", "3", "4"}} {
		if _, _, e := parseAllLimit(args); e == nil {
			t.Fatal("invalid accepted", args)
		}
	}
	n, days, e := parseAllLimit([]string{"one"})
	if e != nil || n != 500<<30 || days != 30 {
		t.Fatal(n, days, e)
	}
	n, days, e = parseAllLimit([]string{"one", "10240", "30"})
	if e != nil || n != 10240<<20 || days != 30 {
		t.Fatal(n, days, e)
	}
	d := unifiedDoc(t)
	u := UnifiedUsers(d)[0]
	links, e := ShareLinks(d, []string{u.Name}, "203.0.113.1", "", 443)
	if e != nil || len(links) != 1 {
		t.Fatal(e)
	}
	link, e := url.Parse(links[0])
	if e != nil {
		t.Fatal(e)
	}
	q := link.Query()
	if link.Host != "203.0.113.1:443" || link.User.Username() != u.UUID || link.Scheme != "vless" || q.Get("security") != "reality" || q.Get("flow") != "xtls-rprx-vision" || q.Get("sni") != "www.samsung.com" || q.Get("sid") == "" || q.Get("pbk") == "" || q.Get("encryption") != "none" {
		t.Fatal("incomplete share")
	}
	changed := clone(d)
	in := obj(list(changed["inbounds"])[3])
	nested(in, "tls")["server_name"] = "example.com"
	links, e = ShareLinks(changed, []string{u.Name}, "203.0.113.1", "", 443)
	if e != nil {
		t.Fatal(e)
	}
	link, _ = url.Parse(links[0])
	if link.Query().Get("sni") != "example.com" || link.User.Username() != u.UUID {
		t.Fatal("SNI/identity")
	}
	// Duplicate short names fail rather than silently choosing an inbound.
	dupe := clone(obj(list(d["inbounds"])[3]))
	dupe["tag"] = "sbbot-unified-second"
	d["inbounds"] = append(list(d["inbounds"]), dupe)
	if _, e = ShareLinks(d, []string{u.Name}, "example.org", "", 443); e == nil {
		t.Fatal("ambiguous name accepted")
	}
}
func TestCycleHistoryIsolationMonthExpiryPersistenceRollback(t *testing.T) {
	b, _ := testBot(t)
	b.Config.AutoRestart = true
	d := unifiedDoc(t)
	installDoc(t, b, d)
	ctx := context.Background()
	users := UnifiedUsers(d)
	key, other := users[0].Key, users[1].Key
	old := time.Now().AddDate(0, -2, 0).Unix()
	b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?)", old, key, 1000)
	if e := b.Store.SetCycle(ctx, key, 2048<<20, 7, func() error { return b.enforce(ctx, d) }); e != nil {
		t.Fatal(e)
	}
	c, e := b.Store.Cycle(ctx, key)
	if e != nil || c.Baseline != 1000 || c.Allowance != 2048<<20 || c.Expires-c.Started != 7*86400 {
		t.Fatal(c, e)
	}
	// A month boundary does not renew this cycle; lifetime deltas count all samples.
	b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?)", old, key, 2048<<20)
	if e = b.enforce(ctx, d); e != nil {
		t.Fatal(e)
	}
	if !Blocked(d)[key] || Blocked(d)[other] {
		t.Fatal("cycle isolation", Blocked(d))
	}
	total, _ := b.Store.Usage(ctx, key, time.Unix(0, 0))
	if total != 1000+2048<<20 {
		t.Fatal("history reset", total)
	}
	// Failure restores both persisted cycle and active config.
	active, _ := os.ReadFile(b.Config.Path)
	before := *c
	b.Config.Run = func(context.Context, string, ...string) (string, error) {
		return "", errors.New("injected check failure")
	}
	e = b.Store.SetCycle(ctx, key, 4096<<20, 30, func() error { return b.enforce(ctx, d) })
	if e == nil {
		t.Fatal("failure hidden")
	}
	c, e = b.Store.Cycle(ctx, key)
	if e != nil || *c != before {
		t.Fatal("policy rollback", c, e)
	}
	raw, _ := os.ReadFile(b.Config.Path)
	if string(raw) != string(active) {
		t.Fatal("config rollback")
	}
	b.Config.Run = func(context.Context, string, ...string) (string, error) { return "", nil }
	d, _ = Load(b.Config.Path)
	c.Expires = time.Now().Unix() - 1
	c.Allowance = 4096 << 20
	b.Store.putCycle(ctx, key, c)
	if e = b.enforce(ctx, d); e != nil {
		t.Fatal(e)
	}
	if !Blocked(d)[key] || Blocked(d)[other] {
		t.Fatal("expiry isolation")
	}
	var dbPath string
	b.Store.DB.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&dbPath)
	reopened, e := OpenStore(dbPath, 500<<30)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.DB.Close()
	got, e := reopened.Cycle(ctx, key)
	if e != nil || *got != *c {
		t.Fatal("restart lost cycle", got, e)
	}
	if e = b.Store.SetCycle(ctx, key, math.MaxInt64, 1, func() error { return nil }); e == nil {
		t.Fatal("overflow accepted")
	}
}
func TestEntryMenuPagerAndMutations(t *testing.T) {
	b, calls := testBot(t)
	b.Config.AutoRestart = true
	d := unifiedDoc(t)
	ctx := context.Background()
	m := tg.Message{ID: 1, Chat: tg.Chat{ID: 1}, From: tg.User{ID: 1}}
	st := &pending{}
	for i := 0; i < 17; i++ {
		d["outbounds"] = append(list(d["outbounds"]), Doc{"type": "direct", "tag": fmt.Sprintf("exit-%02d", i)})
	}
	installDoc(t, b, d)
	if e := b.unifiedMenu(ctx, m, d, "unified:menu", st); e != nil {
		t.Fatal(e)
	}
	text, _ := json.Marshal((*calls)[len(*calls)-1])
	for _, want := range []string{"UUID 用户管理", "出站 IP 绑定", "REALITY 伪装域名", "分享链接", "流量与有效期", "入口设置"} {
		if !strings.Contains(string(text), want) {
			t.Fatal(want)
		}
	}
	raw, _ := json.Marshal(protocolMenu())
	if strings.Contains(string(raw), "user:list") || strings.Contains(string(raw), "user:add") {
		t.Fatal("duplicate menu")
	}
	if e := b.unifiedMenu(ctx, m, d, "unified:outbounds", st); e != nil {
		t.Fatal(e)
	}
	for page, want := range []int{8, 8, 3} {
		if page > 0 {
			if e := b.userCallback(ctx, m, d, fmt.Sprintf("user:outlistpage:%s:%d", st.Token, page), st); e != nil {
				t.Fatal(e)
			}
		}
		raw, _ := json.Marshal((*calls)[len(*calls)-1]["reply_markup"])
		var k tg.Keyboard
		json.Unmarshal(raw, &k)
		n := 0
		for _, row := range k.Rows {
			for _, btn := range row {
				if len([]byte(btn.Data)) > 64 {
					t.Fatal("callback length")
				}
				if strings.HasPrefix(btn.Data, "user:outinfo:") {
					n++
				}
			}
		}
		if n != want {
			t.Fatal(page, n)
		}
	}
	if e := b.userPage(ctx, m, d, st, 0, true); e != nil {
		t.Fatal(e)
	}
	u := UnifiedUsers(d)[0]
	if e := b.userCallback(ctx, m, d, "user:pick:"+st.Token+":"+ref(u.Key), st); e != nil {
		t.Fatal(e)
	}
	if e := b.userCallback(ctx, m, d, "user:remove:"+st.Token+":"+ref(u.Key), st); e != nil {
		t.Fatal(e)
	}
	cb := "user:removeok:" + st.Token + ":" + ref(u.Key)
	if e := b.userCallback(ctx, m, d, cb, st); e != nil {
		t.Fatal(e)
	}
	if _, ok := findUser(d, u.Key); ok {
		t.Fatal("not removed")
	}
	if e := ValidateUnified(d); e != nil {
		t.Fatal(e)
	}
	if e := b.userCallback(ctx, m, d, cb, st); e == nil {
		t.Fatal("delete replay")
	}
}
func TestRealityParsingConflictsAndHostRollback(t *testing.T) {
	for _, bad := range []string{"", "https://example.com", "127.0.0.1", "x.com x.com:0", "x.com x.com:99999", "x.com;rm", "a..com"} {
		if _, _, _, e := realityTarget(bad); e == nil {
			t.Fatal("invalid", bad)
		}
	}
	s, h, p, e := realityTarget("Example.COM www.example.com:8443")
	if e != nil || s != "example.com" || h != "www.example.com" || p != 8443 {
		t.Fatal(s, h, p, e)
	}
	hap := "frontend tls\n    acl license req.ssl_sni -i license.example.com\n    acl reality req.ssl_sni -i www.samsung.com\n    use_backend license if license\n    use_backend reality if reality\n"
	if _, e = haproxyReality(hap, "license.example.com"); e == nil {
		t.Fatal("conflict")
	}
	if _, e = haproxyReality(strings.ReplaceAll(hap, "-i license.example.com", "-m reg .*"), "new.example.com"); e == nil {
		t.Fatal("complex overlap")
	}
	changed, e := haproxyReality(hap, "new.example.com")
	if e != nil || !strings.Contains(changed, "-i new.example.com") || !strings.Contains(changed, "-i license.example.com") {
		t.Fatal(changed, e)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "haproxy.cfg")
	os.WriteFile(cfg, []byte(hap), 0600)
	fake := filepath.Join(dir, "host-tool")
	os.WriteFile(fake, []byte("#!/bin/sh\nif [ \"$1\" = reload ] && [ ! -f '"+dir+"/failed' ]; then touch '"+dir+"/failed'; exit 1; fi\nexit 0\n"), 0700)
	script := strings.ReplaceAll(hapInstallScript, "/etc/haproxy/haproxy.cfg", cfg)
	script = strings.ReplaceAll(script, "/usr/sbin/haproxy", fake)
	script = strings.ReplaceAll(script, "/usr/bin/systemctl", fake)
	cmd := exec.Command("python3", "-c", script, hap, changed)
	if e = cmd.Run(); e == nil {
		t.Fatal("reload failure hidden")
	}
	raw, _ := os.ReadFile(cfg)
	if string(raw) != hap {
		t.Fatal("haproxy rollback failed")
	}
	if out, e := exec.Command("python3", "-c", script, hap, changed).CombinedOutput(); e != nil {
		t.Fatalf("%v %s", e, out)
	}
	raw, _ = os.ReadFile(cfg)
	if string(raw) != changed {
		t.Fatal("candidate not applied")
	}
}
func TestEntryAdminSessionIsolation(t *testing.T) {
	b, _ := testBot(t)
	d := unifiedDoc(t)
	installDoc(t, b, d)
	b.Admins[2] = true
	ctx := context.Background()
	m := tg.Message{ID: 1, From: tg.User{ID: 1}, Chat: tg.Chat{ID: 9}}
	if e := b.Handle(ctx, tg.Update{Callback: &tg.Callback{ID: "a", From: m.From, Message: &m, Data: "user:list"}}); e != nil {
		t.Fatal(e)
	}
	st := b.States[1]
	cb := "user:pick:" + st.Token + ":" + ref(UnifiedUsers(d)[0].Key)
	b.Handle(ctx, tg.Update{Callback: &tg.Callback{ID: "b", From: tg.User{ID: 2}, Message: &m, Data: cb}})
	if b.States[2].User != "" || st.User != "" {
		t.Fatal("cross-admin accepted")
	}
}
func selectedCallback(st *pending, original string) string {
	if st == nil {
		return original
	}
	for id, action := range st.Healthy {
		if action == original {
			return "select:" + st.Token + ":" + id
		}
	}
	return original
}
func TestRealityConfirmationIdentityAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			b, _ := testBot(t)
			b.Config.AutoRestart = true
			d := unifiedDoc(t)
			installDoc(t, b, d)
			before, _ := os.ReadFile(b.Config.Path)
			old := "frontend tls\n acl license req.ssl_sni -i license.example.com\n acl reality req.ssl_sni -i www.samsung.com\n"
			active := old
			b.Config.RealityCheck = func(context.Context, string, string, int) error { return nil }
			b.Config.Run = func(_ context.Context, program string, args ...string) (string, error) {
				if program == "/usr/bin/cat" {
					return active, nil
				}
				if program == "/usr/bin/python3" {
					if strings.TrimSpace(active) != strings.TrimSpace(args[2]) {
						return "", errors.New("HAProxy stale")
					}
					active = args[3]
				}
				return "", nil
			}
			b.Config.Probe = func(_ context.Context, candidate Doc, key string) (string, error) {
				if !strings.Contains(active, "new.example.com") {
					t.Fatal("HAProxy not applied before probe")
				}
				if fail {
					return "", errors.New("REALITY handshake failed")
				}
				return "192.0.2.1", nil
			}
			ctx := context.Background()
			m := tg.Message{ID: 1, From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1}, Text: "new.example.com"}
			st := &pending{Mode: "entry-input-reality", Version: configVersion(d), ChatID: 1, Until: time.Now().Add(time.Minute)}
			if e := b.realityPreview(ctx, m, d, st); e != nil {
				t.Fatal(e)
			}
			m.ID = st.MessageID
			cb := "unified:confirm:" + st.Token
			wrong := m
			wrong.Chat.ID = 2
			if e := b.realityConfirm(ctx, wrong, d, cb, st); e == nil {
				t.Fatal("cross-chat accepted")
			}
			e := b.realityConfirm(ctx, m, d, cb, st)
			if (e != nil) != fail {
				t.Fatal(e)
			}
			got, _ := Load(b.Config.Path)
			if fail {
				raw, _ := os.ReadFile(b.Config.Path)
				if string(raw) != string(before) || active != old {
					t.Fatal("joint rollback failed")
				}
			} else {
				if strings.Contains(active, "www.samsung.com") {
					t.Fatal("SNI not switched")
				}
				for i, u := range UnifiedUsers(d) {
					saved := UnifiedUsers(got)[i]
					if saved.UUID != u.UUID || saved.Outbound != u.Outbound {
						t.Fatal("identity/binding changed")
					}
				}
			}
			if e = b.realityConfirm(ctx, m, got, cb, st); e == nil {
				t.Fatal("confirmation replay")
			}
		})
	}
}
func TestEntrySelectionPaginationFreshness(t *testing.T) {
	b, calls := testBot(t)
	d := testDoc()
	for i := 0; i < 19; i++ {
		d["outbounds"] = append(list(d["outbounds"]), Doc{"type": "direct", "tag": fmt.Sprintf("%040d", i)})
	}
	installDoc(t, b, d)
	ctx := context.Background()
	m := tg.Message{ID: 1, Chat: tg.Chat{ID: 1}}
	st := &pending{}
	if e := b.callback(ctx, m, "bind:start", st); e != nil {
		t.Fatal(e)
	}
	old := ""
	for page := 0; page < 3; page++ {
		raw, _ := json.Marshal((*calls)[len(*calls)-1]["reply_markup"])
		var k tg.Keyboard
		json.Unmarshal(raw, &k)
		count := 0
		for _, row := range k.Rows[:len(k.Rows)-2] {
			for _, btn := range row {
				count++
				if len(btn.Data) > 64 {
					t.Fatal("callback length")
				}
			}
		}
		want := 8
		if page == 2 {
			want = 7
		}
		if count != want {
			t.Fatal(page, count)
		}
		if page < 2 {
			cb := selectedCallback(st, fmt.Sprintf("page:%d", page+1))
			if e := b.callback(ctx, m, cb, st); e != nil {
				t.Fatal(e)
			}
			if e := b.callback(ctx, m, cb, st); e == nil {
				t.Fatal("replayed pagination")
			}
			old = cb
		}
	}
	st.Until = time.Now().Add(-time.Second)
	if e := b.callback(ctx, m, old, st); e == nil {
		t.Fatal("expired page")
	}
}
func TestEntryAllLimitTelegramParsingAndInvalidNoMutation(t *testing.T) {
	b, _ := testBot(t)
	b.Config.AutoRestart = true
	d := unifiedDoc(t)
	installDoc(t, b, d)
	ctx := context.Background()
	m := tg.Message{ID: 1, From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1}, Text: "/alllimit@mybot one"}
	started := time.Now().Unix()
	if e := b.Handle(ctx, tg.Update{Message: &m}); e != nil {
		t.Fatal(e)
	}
	key := UnifiedUsers(d)[0].Key
	c, e := b.Store.Cycle(ctx, key)
	if e != nil || c == nil || c.Allowance != 500<<30 || c.Started < started || c.Expires-c.Started != 30*86400 {
		t.Fatal(c, e)
	}
	before := *c
	raw, _ := os.ReadFile(b.Config.Path)
	for _, cmd := range []string{"/alllimit one 0", "/alllimit one -1 3", "/alllimit one 1.5 3", "/alllimit one 1 999999999999999999999", "/alllimit missing 1 1"} {
		m.Text = cmd
		if e := b.Handle(ctx, tg.Update{Message: &m}); e != nil {
			t.Fatal(e)
		}
		c, e = b.Store.Cycle(ctx, key)
		if e != nil || *c != before {
			t.Fatal("invalid mutation", cmd, c, e)
		}
		got, _ := os.ReadFile(b.Config.Path)
		if string(raw) != string(got) {
			t.Fatal("invalid input changed config")
		}
	}
}
func TestRealityDefaultDNSCertificateTLS13(t *testing.T) {
	if e := checkRealityTarget(context.Background(), "www.samsung.com", "www.samsung.com", 443); e != nil {
		t.Fatal(e)
	}
}
func TestEntryTrafficPagination(t *testing.T) {
	b, calls := testBot(t)
	d := unifiedDoc(t)
	for i := 0; i < 15; i++ {
		if _, e := AddUnifiedUser(d, "sbbot-unified-main", fmt.Sprintf("traffic-%02d", i), "", "direct"); e != nil {
			t.Fatal(e)
		}
	}
	ctx := context.Background()
	m := tg.Message{ID: 1, Chat: tg.Chat{ID: 1}}
	st := &pending{}
	if e := b.trafficPage(ctx, m, d, st, "month", 0, true); e != nil {
		t.Fatal(e)
	}
	for page := 1; page < 3; page++ {
		old := fmt.Sprintf("traffic:%s:%d", st.Token, page)
		if e := b.trafficCallback(ctx, m, d, old, st); e != nil {
			t.Fatal(e)
		}
		if e := b.trafficCallback(ctx, m, d, old, st); e == nil {
			t.Fatal("replayed traffic page")
		}
	}
	text := str((*calls)[len(*calls)-1]["text"])
	if !strings.Contains(text, "3 / 3") || strings.Contains(text, "sbbot-user-") {
		t.Fatal(text)
	}
}
func TestEntryToggleRefreshUsesPersistedConfig(t *testing.T) {
	b, _ := testBot(t)
	b.Config.AutoRestart = true
	d := unifiedDoc(t)
	installDoc(t, b, d)
	ctx := context.Background()
	m := tg.Message{ID: 1, Chat: tg.Chat{ID: 1}, From: tg.User{ID: 1}}
	st := &pending{}
	if e := b.userPage(ctx, m, d, st, 0, true); e != nil {
		t.Fatal(e)
	}
	key := UnifiedUsers(d)[0].Key
	if e := b.userCallback(ctx, m, d, "user:pick:"+st.Token+":"+ref(key), st); e != nil {
		t.Fatal(e)
	}
	if e := b.userCallback(ctx, m, d, "user:toggle:"+st.Token+":"+ref(key), st); e != nil {
		t.Fatal(e)
	}
	fresh, e := Load(b.Config.Path)
	if e != nil || !Blocked(fresh)[key] {
		t.Fatal("toggle not applied", e)
	}
	if st.Version != configVersion(fresh) {
		t.Fatal("list kept stale config version")
	}
	if e := b.userCallback(ctx, m, fresh, "user:pick:"+st.Token+":"+ref(key), st); e != nil {
		t.Fatal(e)
	}
}
func TestEntryShareKeepsLegacyNameCollision(t *testing.T) {
	d := unifiedDoc(t)
	if _, e := AddUnifiedUser(d, "sbbot-unified-main", "alice", "", "direct"); e != nil {
		t.Fatal(e)
	}
	links, e := ShareLinks(d, []string{"alice"}, "example.org", "", 443)
	if e != nil || len(links) != 1 || !strings.HasPrefix(links[0], "anytls://") {
		t.Fatal("legacy share collision", e)
	}
	b, _ := testBot(t)
	if b.resolve(d, "alice") != "alice" {
		t.Fatal("legacy policy name collision")
	}
	key, e := resolveUnified(d, "alice")
	if e != nil {
		t.Fatal(e)
	}
	links, e = ShareLinks(d, []string{key}, "example.org", "", 443)
	if e != nil || !strings.HasPrefix(links[0], "vless://") {
		t.Fatal("full key share", e)
	}
}
