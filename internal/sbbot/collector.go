package sbbot

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

func (b *Bot) enforce(ctx context.Context, d Doc) error {
	target, err := b.Store.Manual(ctx)
	if err != nil {
		return err
	}
	used, err := b.Store.UsageAll(ctx, Month(time.Now().In(b.TZ)))
	if err != nil {
		return err
	}
	version := configVersion(d)
	current := Blocked(d)
	for _, v := range accountingInbounds(d) {
		tag := str(obj(v)["tag"])
		lim, err := b.Store.Limit(ctx, tag)
		if err != nil {
			return err
		}
		expiry, err := b.Store.Expiry(ctx, tag)
		if err != nil {
			return err
		}
		cycle, e := b.Store.Cycle(ctx, tag)
		if e != nil {
			return e
		}
		cycleBlocked := false
		if cycle != nil {
			total, e := b.Store.Usage(ctx, tag, time.Unix(0, 0))
			if e != nil {
				return e
			}
			cycleBlocked = total-cycle.Baseline >= cycle.Allowance || time.Now().Unix() >= cycle.Expires
		}
		if cycleBlocked || lim > 0 && used[tag] >= lim || expiry > 0 && expiry <= time.Now().Unix() {
			target[tag] = true
		}
	}
	links, err := b.Store.LegacyLinks(ctx)
	if err != nil {
		return err
	}
	for key, legacy := range links {
		if _, ok := findUser(d, key); ok && target[key] {
			target[legacy] = true
		}
	}
	if maps.Equal(current, target) && (len(target) == 0 || len(UnifiedUsers(d)) > 0 || Kind(d, ManagedBlock) == "outbound") {
		return nil
	}
	SetBlockedDoc(d, target)
	if len(UnifiedUsers(d)) > 0 {
		return b.Config.applyUnifiedVersion(ctx, d, version)
	}
	return b.Config.Apply(ctx, d, nil)
}
func (b *Bot) Collect(ctx context.Context) error {
	if !b.Features["singbox"] {
		return nil
	}
	headers := map[string]string{}
	if b.APISecret != "" {
		headers["Authorization"] = "Bearer " + b.APISecret
	}
	requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	payload, err := JSONGet(requestCtx, b.HTTP, b.APIURL+"/connections", headers)
	cancel()
	if err != nil {
		return err
	}
	b.Mu.Lock()
	defer b.Mu.Unlock()
	d, err := Load(b.Config.Path)
	if err != nil {
		return err
	}
	if err = b.Store.Sample(ctx, payload, d); err != nil {
		return err
	}
	current := Blocked(d)
	if err = b.enforce(ctx, d); err != nil {
		return err
	}
	for tag := range Blocked(d) {
		if current[tag] {
			continue
		}
		lim, err := b.Store.Limit(ctx, tag)
		if err != nil {
			return err
		}
		used, err := b.Store.Usage(ctx, tag, Month(time.Now().In(b.TZ)))
		if err != nil {
			return err
		}
		if lim > 0 && used >= lim {
			for admin := range b.Admins {
				if err = b.send(ctx, admin, fmt.Sprintf("⛔ 入站 %s 本月流量已达限额 %s，已自动暂停。", tag, Human(lim)), nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (b *Bot) Daily(ctx context.Context, hour int) error {
	now := time.Now().In(b.TZ)
	if !b.Features["singbox"] || now.Hour() != hour {
		return nil
	}
	start := Day(now).AddDate(0, 0, -1)
	key := start.Format("2006-01-02")
	var exists int
	if err := b.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM reports WHERE day=?", key).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	b.Mu.Lock()
	d, err := Load(b.Config.Path)
	b.Mu.Unlock()
	if err != nil {
		return err
	}
	valid := map[string]bool{}
	reporting, err := b.reportingInbounds(ctx, d)
	if err != nil {
		return err
	}
	for _, v := range reporting {
		valid[str(obj(v)["tag"])] = true
	}
	used, err := b.Store.UsageRange(ctx, start, Day(now))
	if err != nil {
		return err
	}
	lines := []string{"📅 昨日流量报告（" + key + " 00:00-24:00）"}
	var total int64
	var keys []string
	for tag := range used {
		if valid[tag] {
			keys = append(keys, tag)
		}
	}
	slices.SortFunc(keys, func(a, c string) int {
		if used[a] > used[c] {
			return -1
		}
		if used[a] < used[c] {
			return 1
		}
		return strings.Compare(a, c)
	})
	for _, tag := range keys {
		total += used[tag]
		lines = append(lines, tag+"："+Human(used[tag]))
	}
	lines = append(lines, "合计："+Human(total))
	for admin := range b.Admins {
		if err = b.send(ctx, admin, strings.Join(lines, "\n"), nil); err != nil {
			return err
		}
	}
	_, err = b.Store.DB.ExecContext(ctx, "INSERT OR IGNORE INTO reports(day) VALUES(?)", key)
	return err
}
func (b *Bot) EnsureAPI(ctx context.Context) error {
	if !b.Features["singbox"] {
		return nil
	}
	b.Mu.Lock()
	defer b.Mu.Unlock()
	d, err := Load(b.Config.Path)
	if err != nil {
		return err
	}
	clash := nested(nested(d, "experimental"), "clash_api")
	if str(clash["external_controller"]) == "127.0.0.1:9090" && str(clash["secret"]) == b.APISecret {
		return nil
	}
	clash["external_controller"] = "127.0.0.1:9090"
	clash["secret"] = b.APISecret
	return b.Config.Apply(ctx, d, nil)
}
