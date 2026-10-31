package sbbot

import (
	"context"
	"errors"
	"fmt"
	tg "local/bots/internal/telegram"
	"strings"
	"time"
)

func (b *Bot) trafficPage(ctx context.Context, m tg.Message, d Doc, st *pending, period string, page int, edit bool) error {
	rows, e := b.reportingInbounds(ctx, d)
	if e != nil {
		return e
	}
	// These aggregates are scoped to this rendering, never cached across updates.
	now := time.Now().In(b.TZ)
	start := Month(now)
	if period == "day" {
		start = Day(now)
	}
	used, e := b.Store.UsageAll(ctx, start)
	if e != nil {
		return e
	}
	daily := used
	if period != "day" {
		daily, e = b.Store.UsageAll(ctx, Day(now))
		if e != nil {
			return e
		}
	}
	blocked := Blocked(d)
	if period == "blocks" {
		filtered := []any{}
		for _, raw := range rows {
			if blocked[str(obj(raw)["tag"])] {
				filtered = append(filtered, raw)
			}
		}
		rows = filtered
	}
	pages := max(1, (len(rows)+userPageSize-1)/userPageSize)
	page = max(0, min(page, pages-1))
	lines := []string{fmt.Sprintf("📊 流量与有效期（第 %d / %d 页）", page+1, pages)}
	for _, raw := range rows[min(page*userPageSize, len(rows)):min((page+1)*userPageSize, len(rows))] {
		tag := str(obj(raw)["tag"])
		if u, ok := findUser(d, tag); ok {
			text, e := b.userStatus(ctx, d, u)
			if e != nil {
				return e
			}
			if period == "day" {
				n, e := b.Store.Usage(ctx, tag, Day(time.Now().In(b.TZ)))
				if e != nil {
					return e
				}
				text += "\n今日 " + Human(n)
			}
			lines = append(lines, text)
		} else {
			limit, e := b.Store.Limit(ctx, tag)
			if e != nil {
				return e
			}
			quota := Human(limit)
			if limit == 0 {
				quota = "不限"
			}
			label := "本月"
			if period == "day" {
				label = "今日"
			}
			text := fmt.Sprintf("%s :%v｜今日 %s｜%s %s / %s", tag, obj(raw)["listen_port"], Human(daily[tag]), label, Human(used[tag]), quota)
			state := "✅ "
			if blocked[tag] {
				state = "⛔ "
			}
			lines = append(lines, state+text)
		}
	}
	token, e := randomHex(8)
	if e != nil {
		return e
	}
	k := tg.K()
	nav := []tg.Button{}
	if page > 0 {
		nav = append(nav, tg.B("上一页", fmt.Sprintf("traffic:%s:%d", token, page-1)))
	}
	nav = append(nav, tg.B(fmt.Sprintf("%d / %d", page+1, pages), fmt.Sprintf("traffic:%s:%d", token, page)))
	if page+1 < pages {
		nav = append(nav, tg.B("下一页", fmt.Sprintf("traffic:%s:%d", token, page+1)))
	}
	k.Rows = append(k.Rows, nav, []tg.Button{tg.B("UUID 用户管理", "user:list"), tg.B("返回", "panel")})
	next := pending{Mode: "traffic", Action: period, Page: page, Token: token, Version: configVersion(d), ChatID: m.Chat.ID, MessageID: m.ID, Until: time.Now().Add(5 * time.Minute)}
	if edit {
		e = b.API.Edit(ctx, m.Chat.ID, m.ID, strings.Join(lines, "\n\n"), k, false)
	} else {
		var sent tg.Message
		sent, e = b.API.Send(ctx, m.Chat.ID, strings.Join(lines, "\n\n"), k, false)
		next.MessageID = sent.ID
	}
	if e == nil {
		*st = next
	}
	return e
}
func (b *Bot) trafficCallback(ctx context.Context, m tg.Message, d Doc, cb string, st *pending) error {
	p := strings.Split(cb, ":")
	if len(p) != 3 || st.Mode != "traffic" || p[1] != st.Token || st.Token == "" || st.ChatID != m.Chat.ID || st.MessageID != m.ID || time.Now().After(st.Until) || st.Version != configVersion(d) {
		return errors.New("流量列表已过期，请重新打开")
	}
	page, e := callbackPage(p[2])
	if e != nil {
		return e
	}
	return b.trafficPage(ctx, m, d, st, st.Action, page, true)
}
