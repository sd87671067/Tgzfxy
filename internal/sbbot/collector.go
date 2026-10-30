package sbbot

import (
	"context"
	"fmt"
	"maps"
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
	current := Blocked(d)
	for _, v := range list(d["inbounds"]) {
		tag := str(obj(v)["tag"])
		lim, err := b.Store.Limit(ctx, tag)
		if err != nil {
			return err
		}
		expiry, err := b.Store.Expiry(ctx, tag)
		if err != nil {
			return err
		}
		if lim > 0 && used[tag] >= lim || expiry > 0 && expiry <= time.Now().Unix() {
			target[tag] = true
		}
	}
	if maps.Equal(current, target) && (len(target) == 0 || Kind(d, ManagedBlock) == "outbound") {
		return nil
	}
	SetBlockedDoc(d, target)
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
	for _, v := range list(d["inbounds"]) {
		valid[str(obj(v)["tag"])] = true
	}
	rows, err := b.Store.DB.QueryContext(ctx, "SELECT tag,SUM(bytes) FROM samples WHERE ts>=? AND ts<? GROUP BY tag ORDER BY SUM(bytes) DESC", start.Unix(), Day(now).Unix())
	if err != nil {
		return err
	}
	lines := []string{"📅 昨日流量报告（" + key + " 00:00-24:00）"}
	var total int64
	for rows.Next() {
		var tag string
		var n int64
		if err = rows.Scan(&tag, &n); err != nil {
			rows.Close()
			return err
		}
		if valid[tag] {
			total += n
			lines = append(lines, tag+"："+Human(n))
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
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
