package sbbot

import (
	"context"
	tg "local/bots/internal/telegram"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestNaturalMonthRecoveryIsolationAndRestart(t *testing.T) {
	for _, mode := range []string{"monthly", "manual", "expiry", "cycle-bytes", "cycle-days"} {
		t.Run(mode, func(t *testing.T) {
			b, _ := testBot(t)
			b.Config.AutoRestart = true
			b.TZ, _ = time.LoadLocation("Asia/Shanghai")
			d := unifiedDoc(t)
			installDoc(t, b, d)
			key := UnifiedUsers(d)[0].Key
			other := UnifiedUsers(d)[1].Key
			ctx := context.Background()
			boundary := time.Date(2027, 1, 1, 0, 0, 0, 0, b.TZ)
			before := boundary.Add(-time.Second)
			if err := b.Store.SetLimit(ctx, key, 100); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?)", before.Unix(), key, 100); err != nil {
				t.Fatal(err)
			}
			cycle := &UserCycle{Baseline: 100, Allowance: 500, Expires: boundary.AddDate(0, 2, 0).Unix(), Started: before.Unix()}
			if mode == "manual" {
				if err := b.Store.SetManual(ctx, map[string]bool{key: true}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "expiry" {
				if _, err := b.Store.DB.Exec("INSERT INTO proto_expiry VALUES(?,?)", key, before.Unix()); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cycle-bytes" {
				cycle.Baseline = 0
				cycle.Allowance = 100
			}
			if mode == "cycle-days" {
				cycle.Expires = before.Unix()
			}
			if err := b.Store.putCycle(ctx, key, cycle); err != nil {
				t.Fatal(err)
			}
			if err := b.enforceAt(ctx, d, before); err != nil {
				t.Fatal(err)
			}
			if !Blocked(d)[key] {
				t.Fatal("not paused before rollover")
			}
			// Simulate a restart plus an entire missed month without changing any counters.
			var path string
			if err := b.Store.DB.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(path, b.Store.DefaultLimit)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.DB.Close()
			b.Store = reopened
			now := boundary
			if mode == "monthly" {
				now = boundary.AddDate(0, 1, 0)
			}
			d, err = Load(b.Config.Path)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := b.enforceAt(ctx, d, now); err != nil {
					t.Fatal(err)
				}
			}
			if Blocked(d)[key] != (mode != "monthly") || Blocked(d)[other] {
				t.Fatal("restriction isolation", Blocked(d))
			}
			monthly, err := b.Store.Usage(ctx, key, Month(now))
			if err != nil || monthly != 0 {
				t.Fatal(monthly, err)
			}
			total, err := b.Store.Usage(ctx, key, time.Unix(0, 0))
			if err != nil || total != 100 {
				t.Fatal(total, err)
			}
			got, err := b.Store.Cycle(ctx, key)
			if err != nil || *got != *cycle {
				t.Fatal("cycle changed", got, err)
			}
			limit, err := b.Store.Limit(ctx, key)
			if err != nil || limit != 100 {
				t.Fatal(limit, err)
			}
		})
	}
}

func TestMonthConnectionBaselinePersists(t *testing.T) {
	b, _ := testBot(t)
	d := unifiedDoc(t)
	u := UnifiedUsers(d)[1]
	ctx := context.Background()
	tz, _ := time.LoadLocation("Asia/Shanghai")
	boundary := time.Date(2026, 11, 1, 0, 0, 0, 0, tz)
	payload := func(n int) Doc {
		return Doc{"connections": []any{Doc{"id": "retained", "metadata": Doc{"inbound": u.Inbound}, "chains": []string{userOutbound(u.Key)}, "upload": n}}}
	}
	if err := b.Store.SampleAt(ctx, payload(100), d, boundary.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	var path string
	b.Store.DB.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path)
	s, err := OpenStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	for i := 0; i < 2; i++ {
		if err := s.SampleAt(ctx, payload(130), d, boundary); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []struct {
		start time.Time
		want  int64
	}{{boundary, 30}, {time.Unix(0, 0), 130}} {
		got, err := s.Usage(ctx, u.Key, v.start)
		if err != nil || got != v.want {
			t.Fatal(got, v.want, err)
		}
	}
}

func TestCallbackPrepareBypassesOperationLock(t *testing.T) {
	b, calls := testBot(t)
	m := tg.Message{ID: 1, From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1, Type: "private"}}
	u := tg.Update{Callback: &tg.Callback{ID: "test", From: m.From, Message: &m, Data: "panel"}}
	b.Mu.Lock()
	start := time.Now()
	err := b.Prepare(context.Background(), &u)
	elapsed := time.Since(start)
	b.Mu.Unlock()
	if err != nil || elapsed > 100*time.Millisecond {
		t.Fatal(elapsed, err)
	}
	if err = b.Handle(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, c := range *calls {
		if c["method"] == "answerCallbackQuery" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("duplicate ack", count)
	}
	t.Logf("callback with operation mutex held: %s", elapsed)
}

func TestRemovedLimitAndMaintenanceSafety(t *testing.T) {
	b, calls := testBot(t)
	ctx := context.Background()
	b.Store.SetLimit(ctx, "alice", 123)
	m := tg.Message{From: tg.User{ID: 1}, Chat: tg.Chat{ID: 1, Type: "private"}, Text: "/limit alice 0"}
	if err := b.Handle(ctx, tg.Update{Message: &m}); err != nil {
		t.Fatal(err)
	}
	n, err := b.Store.Limit(ctx, "alice")
	if err != nil || n != 123 {
		t.Fatal("old command mutated", n, err)
	}
	m.Text = "/help"
	if err := b.Handle(ctx, tg.Update{Message: &m}); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if strings.Contains(str(c["text"]), "/limit ") {
			t.Fatal("old help", c)
		}
	}
	var path string
	b.Store.DB.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path)
	dir := filepath.Dir(path)
	old := time.Now().Add(-48 * time.Hour)
	b.Store.DB.Exec("INSERT INTO opslog(ts) VALUES(?),(?)", old.Unix(), time.Now().Unix())
	b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?)", old.Unix(), "alice", 999)
	cache := filepath.Join(dir, "forwarder-cache")
	os.Mkdir(cache, 0700)
	expired := filepath.Join(cache, "expired.tmp")
	fresh := filepath.Join(cache, "fresh.tmp")
	protected := filepath.Join(dir, "session.json")
	for _, p := range []string{expired, fresh, protected} {
		os.WriteFile(p, []byte("keep"), 0600)
	}
	os.Chtimes(expired, old, old)
	os.Chtimes(protected, old, old)
	os.Symlink(protected, filepath.Join(cache, "symlink"))
	if err := Maintenance(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatal("expired survived", err)
	}
	for _, p := range []string{fresh, protected} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal("protected removed", p, err)
		}
	}
	var logs int
	b.Store.DB.QueryRow("SELECT COUNT(*) FROM opslog").Scan(&logs)
	if logs != 1 {
		t.Fatal(logs)
	}
	n, err = b.Store.Usage(ctx, "alice", time.Unix(0, 0))
	if err != nil || n != 999 {
		t.Fatal("history lost", n, err)
	}
	n, err = b.Store.Limit(ctx, "alice")
	if err != nil || n != 123 {
		t.Fatal("quota lost", n, err)
	}
}

func TestCollectorRecoversMonthlyBlockWithoutTrafficAPI(t *testing.T) {
	b, _ := testBot(t)
	b.Config.AutoRestart = true
	b.APIURL = "http://127.0.0.1:1"
	d := unifiedDoc(t)
	us := UnifiedUsers(d)
	ctx := context.Background()
	b.Store.SetLimit(ctx, us[0].Key, 100)
	b.Store.SetManual(ctx, map[string]bool{us[1].Key: true})
	b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?)", Month(time.Now()).Add(-time.Second).Unix(), us[0].Key, 100)
	b.Store.DB.Exec("INSERT INTO connections VALUES(?,?,?)", "keep", us[0].Key, 100)
	SetBlockedDoc(d, map[string]bool{us[0].Key: true, us[1].Key: true})
	installDoc(t, b, d)
	if err := b.Collect(ctx); err == nil {
		t.Fatal("API failure hidden")
	}
	after, err := Load(b.Config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if Blocked(after)[us[0].Key] || !Blocked(after)[us[1].Key] {
		t.Fatal(Blocked(after))
	}
	var total int64
	if err := b.Store.DB.QueryRow("SELECT total FROM connections WHERE id='keep'").Scan(&total); err != nil || total != 100 {
		t.Fatal("baseline lost", total, err)
	}
}

func TestPolicySaveAvoidsTrafficCopy(t *testing.T) {
	b, _ := testBot(t)
	ctx := context.Background()
	if _, err := b.Store.DB.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO samples SELECT 1,'synthetic-history',x FROM n`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := b.Store.SetCycle(ctx, "user:test", 2048, 7, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	var path string
	b.Store.DB.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path)
	backups, err := filepath.Glob(path + ".policy-backup-*")
	if err != nil || len(backups) != 0 {
		t.Fatal(backups, err)
	}
	stat, err := os.Stat(path + ".policy-recovery.json")
	if err != nil || stat.Size() > 4096 || stat.Mode().Perm() != 0600 {
		t.Fatal(stat, err)
	}
	t.Logf("policy persistence: %s; recovery record %d bytes; history rows untouched", elapsed, stat.Size())
}

func TestMonthRecoveryPreservesAdditionalRouteRestrictions(t *testing.T) {
	b, _ := testBot(t)
	b.Config.AutoRestart = true
	d := unifiedDoc(t)
	u := UnifiedUsers(d)[0]
	restriction := Doc{"inbound": []string{u.Inbound}, "auth_user": []string{u.Name}, "action": "reject", "ip_cidr": []string{"192.0.2.0/24"}}
	route := nested(d, "route")
	route["rules"] = append([]any{restriction}, list(route["rules"])...)
	SetBlockedDoc(d, map[string]bool{u.Key: true})
	installDoc(t, b, d)
	// Exercise the exact route-rebuild operation used during monthly recovery.
	SetBlockedDoc(d, map[string]bool{})
	found := false
	for _, raw := range list(nested(d, "route")["rules"]) {
		if len(stringsOf(obj(raw)["ip_cidr"])) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("unrelated route restriction removed")
	}
}
