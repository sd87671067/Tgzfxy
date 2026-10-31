package forwarder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	tg "local/bots/internal/telegram"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrPasswordNeeded = errors.New("two-factor password required")

type State struct {
	Rule                     int64
	Step, Phone, Hash, Field string
	Draft                    *Draft
}
type Draft struct {
	Rule            int64
	Category, Token string
	Original, Words []string
	Selected        int
	Expires         time.Time
}

func nonce() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func wordsJSON(words []string) string {
	if words == nil {
		words = []string{}
	}
	b, _ := json.Marshal(words)
	return string(b)
}
func (e *Engine) cancel(owner int64) {
	st := e.States[owner]
	if st != nil {
		st.Step = ""
		st.Phone = ""
		st.Hash = ""
		st.Field = ""
		st.Draft = nil
	}
	if e.LoginOwner == owner {
		e.LoginOwner = 0
		e.LoginUntil = time.Time{}
	}
}
func (e *Engine) Cancel(owner int64) { e.Mu.Lock(); defer e.Mu.Unlock(); e.cancel(owner) }
func (e *Engine) panel(ctx context.Context, chat, owner, rid int64) error {
	var text string
	k := &tg.Keyboard{}
	if rid == 0 {
		rules, err := e.Rules(ctx)
		if err != nil {
			return err
		}
		text = "📨 Telegram频道转发助手\n" + e.Status + "\n新规则从创建后的消息开始，每 30 秒轮询；实时模式即时监听并补取遗漏。\n目标填 bot 可投递到自己的机器人。"
		for _, r := range rules {
			if r.Owner == owner {
				text += fmt.Sprintf("\nID%d：%s → %s", r.ID, r.SourceLabel, r.TargetLabel)
			}
		}
		k = tg.K([]tg.Button{tg.B("➕ 添加转发规则", "fw:add")}, []tg.Button{tg.B("⚙️ 切换至转发规则设置", "fw:switch")}, []tg.Button{tg.B("« 返回面板", "panel")})
	} else {
		r, err := e.Rule(ctx, rid, owner)
		if err != nil {
			return err
		}
		text = fmt.Sprintf("⚙️ ID%d：%s → %s\n频道按钮解析成链接默认关闭；开启后支持可访问的 TG 消息和查看内容回调，外部网页保留链接。", rid, r.SourceLabel, r.TargetLabel)
		button := func(label, action string) tg.Button { return tg.B(label, fmt.Sprintf("fw:%s:%d", action, rid)) }
		toggle := func(v bool) string {
			if v {
				return "开启"
			}
			return "关闭"
		}
		mode := "轮询 → 点击实时"
		if r.Mode == "live" {
			mode = "实时 → 点击轮询"
		}
		filter := "黑名单 → 点击白名单"
		if r.Filter == "white" {
			filter = "白名单 → 点击黑名单"
		}
		enabled := "暂停"
		if !r.Enabled {
			enabled = "启用"
		}
		k = tg.K([]tg.Button{button("监控："+mode, "mode")}, []tg.Button{button("过滤："+filter, "filter")}, []tg.Button{button("列出关键词", "words"), button("设置关键词", "edit")}, []tg.Button{button("日志（最近十条）", "logs"), button("清除日志", "clear")}, []tg.Button{button("🔁 转发重试", "retry"), button("清除转发重试", "retryclear")}, []tg.Button{button("转发冷却修改", "retrycfg"), button("设置转发失败的关键词", "failurewords")}, []tg.Button{button("频道按钮解析成链接："+toggle(r.ButtonLinks)+"（点击切换）", "buttonlinks")}, []tg.Button{button("转发成功通知："+toggle(r.Notify)+"（点击切换）", "notify")}, []tg.Button{button(enabled, "toggle")}, []tg.Button{button("🗑 删除规则", "delete")}, []tg.Button{tg.B("« 返回规则列表", "fw:switch")}, []tg.Button{tg.B("« 返回转发助手", "fw:menu")})
	}
	runes := []rune(text)
	if len(runes) > 4000 {
		text = string(runes[:4000])
	}
	_, err := e.Bot.Send(ctx, chat, text, k, false)
	return err
}
func (e *Engine) list(ctx context.Context, chat, owner int64, page int) error {
	all, err := e.Rules(ctx)
	if err != nil {
		return err
	}
	var rules []Rule
	for _, r := range all {
		if r.Owner == owner {
			rules = append(rules, r)
		}
	}
	page = max(0, min(page, max(0, (len(rules)-1)/20)))
	k := &tg.Keyboard{}
	for i := page * 20; i < min((page+1)*20, len(rules)); i++ {
		r := rules[i]
		k.Rows = append(k.Rows, []tg.Button{tg.B(fmt.Sprintf("ID%d：%s → %s", r.ID, r.SourceLabel, r.TargetLabel), fmt.Sprintf("fw:rule:%d", r.ID))})
	}
	var nav []tg.Button
	if page > 0 {
		nav = append(nav, tg.B("上一页", fmt.Sprintf("fw:list:%d", page-1)))
	}
	if (page+1)*20 < len(rules) {
		nav = append(nav, tg.B("下一页", fmt.Sprintf("fw:list:%d", page+1)))
	}
	if len(nav) > 0 {
		k.Rows = append(k.Rows, nav)
	}
	k.Rows = append(k.Rows, []tg.Button{tg.B("➕ 添加转发规则", "fw:add")}, []tg.Button{tg.B("« 返回转发助手", "fw:menu")})
	_, err = e.Bot.Send(ctx, chat, fmt.Sprintf("请选择要设置的规则（共 %d 条）：", len(rules)), k, false)
	return err
}
func (e *Engine) draftPage(ctx context.Context, chat int64, d *Draft, page int) error {
	page = max(0, min(page, max(0, (len(d.Words)-1)/10)))
	k := &tg.Keyboard{}
	prefix := fmt.Sprintf("%d:%s", d.Rule, d.Token)
	for i := page * 10; i < min((page+1)*10, len(d.Words)); i++ {
		label := []rune(d.Words[i])
		if len(label) > 80 {
			label = label[:80]
		}
		k.Rows = append(k.Rows, []tg.Button{tg.B(string(label), fmt.Sprintf("fw:wordpick:%s:%d", prefix, i))})
	}
	text := fmt.Sprintf("%s关键词 · 共%d个\n点击关键词 → 删除 → 保存；未保存的删除不生效。", map[string]string{"black": "黑名单", "white": "白名单", "failure": "触发转发重试"}[d.Category], len(d.Words))
	if d.Selected >= 0 && d.Selected < len(d.Words) {
		text += "\n已选择：" + d.Words[d.Selected]
		k.Rows = append(k.Rows, []tg.Button{tg.B("删除", "fw:worddelete:"+prefix)})
	}
	var nav []tg.Button
	if page > 0 {
		nav = append(nav, tg.B("上一页", fmt.Sprintf("fw:wordpage:%s:%d", prefix, page-1)))
	}
	if (page+1)*10 < len(d.Words) {
		nav = append(nav, tg.B("下一页", fmt.Sprintf("fw:wordpage:%s:%d", prefix, page+1)))
	}
	if len(nav) > 0 {
		k.Rows = append(k.Rows, nav)
	}
	k.Rows = append(k.Rows, []tg.Button{tg.B("保存", "fw:wordsave:"+prefix), tg.B("取消 / 返回分类", fmt.Sprintf("fw:words:%d", d.Rule))})
	_, err := e.Bot.Send(ctx, chat, text, k, false)
	return err
}
func (e *Engine) setWords(ctx context.Context, r Rule, field string, words []string) error {
	if !slices.Contains([]string{"black_words", "white_words", "failure_words"}, field) {
		return errors.New("关键词分类无效")
	}
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v := wordsJSON(words)
	if _, err = tx.ExecContext(ctx, "UPDATE rules SET "+field+"=? WHERE id=?", v, r.ID); err != nil {
		return err
	}
	if field == r.Filter+"_words" {
		if _, err = tx.ExecContext(ctx, "UPDATE rules SET words=? WHERE id=?", v, r.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func wordsFor(r Rule, cat string) []string {
	if cat == r.Filter {
		return r.Words
	}
	switch cat {
	case "black":
		return r.BlackWords
	case "white":
		return r.WhiteWords
	case "failure":
		return r.FailureWords
	}
	return nil
}
func (e *Engine) Handle(ctx context.Context, m tg.Message, callback, command string, args []string) (handled bool, result error) {
	defer e.Changed()
	e.Mu.Lock()
	defer e.Mu.Unlock()
	defer func() {
		recovery, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.saveRetry(recovery); err != nil {
			result = errors.Join(result, err)
		}
	}()
	owner := m.From.ID
	st := e.States[owner]
	if st == nil {
		st = &State{}
		e.States[owner] = st
	}
	reply := func(text string) error { _, err := e.Bot.Send(ctx, m.Chat.ID, text, nil, false); return err }
	if command == "bind" {
		if len(args) != 2 {
			return true, reply("用法：/bind 源频道/群/用户链接 目标频道/群/用户链接\n目标可填 bot，表示自己的机器人。")
		}
		ready, err := e.Transport.Ready(ctx)
		if err != nil {
			return true, err
		}
		if !ready {
			return true, reply("用户会话未连接，请先打开转发助手登录。")
		}
		source, left, err := e.Transport.Resolve(ctx, args[0])
		if err != nil {
			return true, err
		}
		if left {
			return true, reply("请先使用配置的 Telegram 用户账号加入来源频道/群")
		}
		dest := args[1]
		if dest == "bot" {
			dest = "@" + e.BotName
		}
		target, _, err := e.Transport.Resolve(ctx, dest)
		if err != nil {
			return true, err
		}
		if source == target || source == e.BotID {
			return true, reply("来源和目标不能相同，不能监控本机器人。")
		}
		rules, err := e.Rules(ctx)
		if err != nil {
			return true, err
		}
		graph := map[int64][]int64{}
		for _, r := range rules {
			if r.Owner == owner && r.Source == source && r.Target == target {
				return true, reply("相同规则已存在")
			}
			graph[r.Source] = append(graph[r.Source], r.Target)
		}
		todo := []int64{target}
		seen := map[int64]bool{}
		for len(todo) > 0 {
			id := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			if id == source {
				return true, reply("该规则会形成转发循环")
			}
			if !seen[id] {
				seen[id] = true
				todo = append(todo, graph[id]...)
			}
		}
		messages, err := e.Transport.History(ctx, source, 0, true)
		if err != nil {
			return true, err
		}
		cursor := 0
		if len(messages) > 0 {
			cursor = messages[0].ID
		}
		res, err := e.DB.ExecContext(ctx, "INSERT INTO rules(owner,source,target,source_label,target_label,cursor) VALUES(?,?,?,?,?,?)", owner, source, target, args[0], args[1], cursor)
		if err != nil {
			return true, err
		}
		st.Rule, err = res.LastInsertId()
		if err != nil {
			return true, err
		}
		return true, e.panel(ctx, m.Chat.ID, owner, st.Rule)
	}
	if command == "switch" {
		if len(args) == 0 {
			return true, e.list(ctx, m.Chat.ID, owner, 0)
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "ID"), 10, 64)
		if err != nil {
			return true, reply("请发送 /switch 1 切换至 ID1")
		}
		if _, err = e.Rule(ctx, id, owner); err != nil {
			return true, err
		}
		st.Rule = id
		return true, e.panel(ctx, m.Chat.ID, owner, id)
	}
	if command == "cs" {
		r, err := e.Rule(ctx, st.Rule, owner)
		if err != nil {
			return true, reply("请先打开要修改的转发规则。")
		}
		if len(args) != 2 {
			return true, reply("用法：/cs 时间 次数，例如 /cs 5 6；时间单位为分钟。")
		}
		minutes, e1 := strconv.Atoi(args[0])
		maximum, e2 := strconv.Atoi(args[1])
		if e1 != nil || e2 != nil || minutes < 1 || minutes > 10080 || maximum < 0 || maximum > 100 {
			return true, reply("时间为 1–10080 分钟，次数为 0–100（0 表示关闭重试）。")
		}
		if _, err = e.DB.ExecContext(ctx, "UPDATE rules SET retry_minutes=?,retry_max=? WHERE id=?", minutes, maximum, r.ID); err != nil {
			return true, err
		}
		for k, d := range e.Queue {
			if k.Rule == r.ID && d.State == "queued" {
				if d.Attempts >= maximum {
					e.remove(k)
				} else {
					d.Due = time.Now().Add(time.Duration(minutes) * time.Minute)
				}
			}
		}
		return true, reply(fmt.Sprintf("✅ ID%d 重试冷却 %d 分钟，最多 %d 次；待重试消息从现在重新计时。", r.ID, minutes, maximum))
	}
	if strings.HasPrefix(callback, "fw:") {
		parts := strings.Split(callback, ":")
		action := parts[1]
		if action == "menu" {
			st.Step = ""
			if m.Chat.Type != "private" {
				return true, reply("请在与机器人的私聊中打开转发助手。")
			}
			ready, err := e.Transport.Ready(ctx)
			if err != nil {
				return true, err
			}
			if ready {
				e.Status = "用户会话已连接"
				return true, e.panel(ctx, m.Chat.ID, owner, 0)
			}
			if e.LoginOwner != 0 && e.LoginOwner != owner && e.LoginUntil.After(time.Now()) {
				return true, reply("另一位管理员正在登录，请稍后重试。")
			}
			e.LoginOwner = owner
			e.LoginUntil = time.Now().Add(10 * time.Minute)
			st.Step = "phone"
			return true, reply("尚未登录 Telegram。请输入带国家区号的手机号码，例如 +12025550123。发送 /cancel 取消。")
		}
		if action == "switch" || action == "list" {
			st.Step = ""
			page := 0
			if len(parts) > 2 {
				page, _ = strconv.Atoi(parts[2])
			}
			return true, e.list(ctx, m.Chat.ID, owner, page)
		}
		if action == "add" {
			return true, reply("发送 /bind 源链接 目标链接\n目标可填 bot。用户会话需有访问权限。")
		}
		if len(parts) < 3 {
			return true, reply("按钮已失效。")
		}
		rid, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return true, err
		}
		r, err := e.Rule(ctx, rid, owner)
		if err != nil {
			return true, err
		}
		st.Rule = rid
		switch action {
		case "rule":
			return true, e.panel(ctx, m.Chat.ID, owner, rid)
		case "retry":
			e.prune()
			var items []*Delivery
			for k, d := range e.Queue {
				if k.Rule == rid && d.State == "queued" {
					items = append(items, d)
				}
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Due.Before(items[j].Due) })
			page := 0
			if len(parts) > 3 {
				page, _ = strconv.Atoi(parts[3])
			}
			pages := max(1, (len(items)+9)/10)
			page = max(0, min(page, pages-1))
			lines := []string{fmt.Sprintf("转发重试 · ID%d · %d/%d 页 · 待重试 %d 条", rid, page+1, pages, len(items))}
			for i := page * 10; i < min((page+1)*10, len(items)); i++ {
				d := items[i]
				text := []rune(d.Text)
				if len(text) > 180 {
					text = text[:180]
				}
				source := fmt.Sprint(r.Source)
				link := ""
				if strings.HasPrefix(source, "-100") {
					link = fmt.Sprintf("\nhttps://t.me/c/%s/%d", source[4:], d.Key.Mid)
				}
				lines = append(lines, fmt.Sprintf("%s%s\n已重试 %d/%d，约 %d 秒后重试", string(text), link, d.Attempts, r.RetryMax, max(0, int(time.Until(d.Due).Seconds()))))
			}
			lines = append(lines, "仅缓存文本和链接：最多500条、24小时过期，重启容器清空。")
			k := tg.K()
			var nav []tg.Button
			if page > 0 {
				nav = append(nav, tg.B("上一页", fmt.Sprintf("fw:retry:%d:%d", rid, page-1)))
			}
			if page+1 < pages {
				nav = append(nav, tg.B("下一页", fmt.Sprintf("fw:retry:%d:%d", rid, page+1)))
			}
			if len(nav) > 0 {
				k.Rows = append(k.Rows, nav)
			}
			k.Rows = append(k.Rows, []tg.Button{tg.B("刷新", fmt.Sprintf("fw:retry:%d:%d", rid, page))}, []tg.Button{tg.B("返回规则", fmt.Sprintf("fw:rule:%d", rid))})
			err = e.Bot.Call(ctx, "sendMessage", map[string]any{"chat_id": m.Chat.ID, "text": strings.Join(lines, "\n\n"), "reply_markup": k, "disable_web_page_preview": true}, nil)
			return true, err
		case "retryclear":
			n := 0
			for k, d := range e.Queue {
				if k.Rule == rid && d.State == "queued" {
					e.remove(k)
					n++
				}
			}
			if err = reply(fmt.Sprintf("✅ 已清除 ID%d 全部冷却中的待转发消息，共 %d 条。", rid, n)); err != nil {
				return true, err
			}
		case "retrycfg":
			return true, reply(fmt.Sprintf("修改 ID%d（当前 %d 分钟 / %d 次）：发送 /cs 时间 次数", rid, r.RetryMinutes, r.RetryMax))
		case "delete":
			_, err = e.Bot.Send(ctx, m.Chat.ID, fmt.Sprintf("确定删除规则 ID%d？将停止该规则转发并删除它的关键词、日志和去重记录。", rid), tg.K([]tg.Button{tg.B("确认删除", fmt.Sprintf("fw:confirm_delete:%d", rid)), tg.B("取消", fmt.Sprintf("fw:rule:%d", rid))}), false)
			return true, err
		case "confirm_delete":
			tx, err := e.DB.BeginTx(ctx, nil)
			if err != nil {
				return true, err
			}
			defer tx.Rollback()
			for _, table := range []string{"logs", "sent"} {
				if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE rule=?", rid); err != nil {
					return true, err
				}
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM rules WHERE id=? AND owner=?", rid, owner); err != nil {
				return true, err
			}
			if err = tx.Commit(); err != nil {
				return true, err
			}
			for k := range e.Queue {
				if k.Rule == rid {
					e.remove(k)
				}
			}
			st.Step = ""
			return true, e.list(ctx, m.Chat.ID, owner, 0)
		case "buttonlinks", "notify", "toggle":
			field := map[string]string{"buttonlinks": "button_links", "notify": "success_notify", "toggle": "enabled"}[action]
			if _, err = e.DB.ExecContext(ctx, "UPDATE rules SET "+field+"=1-"+field+" WHERE id=?", rid); err != nil {
				return true, err
			}
		case "mode":
			mode := "live"
			if r.Mode == "live" {
				mode = "poll"
			}
			if _, err = e.DB.ExecContext(ctx, "UPDATE rules SET mode=? WHERE id=?", mode, rid); err != nil {
				return true, err
			}
		case "filter":
			mode := "white"
			if r.Filter == "white" {
				mode = "black"
			}
			if _, err = e.DB.ExecContext(ctx, "UPDATE rules SET "+r.Filter+"_words=words,words="+mode+"_words,filter=? WHERE id=?", mode, rid); err != nil {
				return true, err
			}
		case "words":
			st.Step = ""
			st.Draft = nil
			k := tg.K()
			for _, cat := range []string{"black", "white", "failure"} {
				k.Rows = append(k.Rows, []tg.Button{tg.B(map[string]string{"black": "黑名单关键词", "white": "白名单关键词", "failure": "触发转发重试关键词"}[cat], fmt.Sprintf("fw:wordcat:%d:%s", rid, cat))})
			}
			k.Rows = append(k.Rows, []tg.Button{tg.B("返回规则", fmt.Sprintf("fw:rule:%d", rid))})
			_, err = e.Bot.Send(ctx, m.Chat.ID, "请选择关键词分类：", k, false)
			return true, err
		case "wordcat":
			if len(parts) < 4 || !slices.Contains([]string{"black", "white", "failure"}, parts[3]) {
				return true, reply("分类无效")
			}
			words := wordsFor(r, parts[3])
			st.Draft = &Draft{Rule: rid, Category: parts[3], Token: nonce()[:8], Original: slices.Clone(words), Words: slices.Clone(words), Selected: -1, Expires: time.Now().Add(30 * time.Minute)}
			return true, e.draftPage(ctx, m.Chat.ID, st.Draft, 0)
		case "wordpick", "worddelete", "wordsave", "wordpage":
			d := st.Draft
			if d == nil || len(parts) < 4 || d.Rule != rid || parts[3] != d.Token || d.Expires.Before(time.Now()) {
				return true, reply("按钮已失效，请重新列出关键词。")
			}
			page := 0
			if action == "wordpick" || action == "wordpage" {
				if len(parts) < 5 {
					return true, reply("按钮已失效")
				}
				n, err := strconv.Atoi(parts[4])
				if err != nil {
					return true, err
				}
				if action == "wordpick" {
					if n < 0 || n >= len(d.Words) {
						return true, reply("索引无效")
					}
					d.Selected = n
				} else {
					page = n
				}
			} else if action == "worddelete" {
				if d.Selected < 0 || d.Selected >= len(d.Words) {
					return true, reply("请先选择关键词")
				}
				d.Words = slices.Delete(d.Words, d.Selected, d.Selected+1)
				d.Selected = -1
				d.Token = nonce()[:8]
			} else {
				if !slices.Equal(wordsFor(r, d.Category), d.Original) {
					st.Draft = nil
					return true, reply("关键词已被其他操作修改，请重新列出关键词后操作。")
				}
				if err = e.setWords(ctx, r, d.Category+"_words", d.Words); err != nil {
					return true, err
				}
				st.Draft = nil
				return true, e.panel(ctx, m.Chat.ID, owner, rid)
			}
			return true, e.draftPage(ctx, m.Chat.ID, d, page)
		case "edit", "failurewords":
			st.Step = "keywords"
			st.Field = r.Filter + "_words"
			if action == "failurewords" {
				st.Field = "failure_words"
			}
			return true, reply("发送关键词，每行一个（最多100个，每个100字）。发送 - 清空，/cancel 取消。")
		case "logs":
			rows, err := e.DB.QueryContext(ctx, "SELECT ts,message FROM logs WHERE rule=? ORDER BY id DESC LIMIT 10", rid)
			if err != nil {
				return true, err
			}
			var lines []string
			for rows.Next() {
				var ts int64
				var text string
				if err = rows.Scan(&ts, &text); err != nil {
					rows.Close()
					return true, err
				}
				lines = append(lines, time.Unix(ts, 0).Local().Format("01-02 15:04:05")+" "+text)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return true, err
			}
			if len(lines) == 0 {
				lines = append(lines, "暂无日志")
			}
			return true, reply(strings.Join(lines, "\n"))
		case "clear":
			_, err = e.DB.ExecContext(ctx, "DELETE FROM logs WHERE rule=?", rid)
			if err != nil {
				return true, err
			}
			return true, reply("✅ 此规则服务器日志已清除。")
		default:
			return true, reply("按钮已失效")
		}
		return true, e.panel(ctx, m.Chat.ID, owner, rid)
	}
	if callback != "" || command != "" {
		return false, nil
	}
	if st.Step == "keywords" {
		r, err := e.Rule(ctx, st.Rule, owner)
		if err != nil {
			st.Step = ""
			return true, err
		}
		var words []string
		if strings.TrimSpace(m.Text) != "-" {
			for _, line := range strings.Split(m.Text, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					words = append(words, line)
				}
			}
		}
		words = unique(words)
		if len(words) > 100 {
			return true, reply("最多100个关键词")
		}
		for _, w := range words {
			if len([]rune(w)) > 100 {
				return true, reply("每个关键词不超过100字")
			}
		}
		if err = e.setWords(ctx, r, st.Field, words); err != nil {
			return true, err
		}
		st.Step = ""
		if err = reply("✅ 关键词已保存。"); err != nil {
			return true, err
		}
		return true, e.panel(ctx, m.Chat.ID, owner, r.ID)
	}
	if st.Step == "phone" || st.Step == "code" || st.Step == "password" {
		if m.Chat.Type != "private" {
			return true, nil
		}
		e.Bot.Call(ctx, "deleteMessage", map[string]any{"chat_id": m.Chat.ID, "message_id": m.ID}, nil)
		if e.LoginOwner != owner || e.LoginUntil.Before(time.Now()) {
			e.cancel(owner)
			return true, reply("登录操作已过期，请重新点击转发助手。")
		}
		var err error
		switch st.Step {
		case "phone":
			phone := strings.TrimSpace(m.Text)
			if !regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`).MatchString(phone) {
				return true, reply("手机号格式不正确，例如 +12025550123")
			}
			st.Hash, err = e.Transport.SendCode(ctx, phone)
			if err == nil {
				st.Phone = phone
				st.Step = "code"
				return true, reply("验证码已请求，请查看 Telegram 官方消息或短信。请输入验证码，数字间加空格，例如 1 2 3 4 5。/cancel 取消。")
			}
		case "code":
			code := strings.Map(func(r rune) rune {
				if r == ' ' || r == '-' || r == '\t' || r == '\n' {
					return -1
				}
				return r
			}, m.Text)
			if !regexp.MustCompile(`^[0-9]{4,8}$`).MatchString(code) {
				return true, reply("请输入验证码数字，数字间加空格。")
			}
			err = e.Transport.SignIn(ctx, st.Phone, code, st.Hash)
			if errors.Is(err, ErrPasswordNeeded) {
				st.Step = "password"
				return true, reply("账号启用了两步验证，请输入 Telegram 两步验证密码；输入消息会尝试自动删除。/cancel 取消。")
			}
		case "password":
			err = e.Transport.Password(ctx, m.Text)
		}
		if err != nil {
			return true, reply("登录失败，请重试或 /cancel 后重新打开：" + err.Error())
		}
		ready, err := e.Transport.Ready(ctx)
		if err != nil {
			return true, err
		}
		if !ready {
			return true, reply("登录尚未完成，请重新打开转发助手。")
		}
		e.cancel(owner)
		e.Status = "用户会话已连接"
		if err = reply("✅ Telegram 登录成功。"); err != nil {
			return true, err
		}
		return true, e.panel(ctx, m.Chat.ID, owner, 0)
	}
	return false, nil
}
