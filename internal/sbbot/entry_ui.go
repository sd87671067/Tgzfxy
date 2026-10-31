package sbbot

import (
	"context"
	"errors"
	"fmt"
	tg "local/bots/internal/telegram"
	"strconv"
	"strings"
	"time"
)

func (b *Bot) userStatus(ctx context.Context, d Doc, u UnifiedUser) (string, error) {
	monthly, e := b.Store.Usage(ctx, u.Key, Month(time.Now().In(b.TZ)))
	if e != nil {
		return "", e
	}
	limit, e := b.Store.Limit(ctx, u.Key)
	if e != nil {
		return "", e
	}
	expiry, e := b.Store.Expiry(ctx, u.Key)
	if e != nil {
		return "", e
	}
	remaining := "不限"
	if limit > 0 {
		remaining = Human(max(0, limit-monthly))
	}
	state := "✅"
	if Blocked(d)[u.Key] {
		state = "⛔"
	}
	line := fmt.Sprintf("%s %s → %s\n月已用 %s / 剩余 %s", state, u.Name, u.Outbound, Human(monthly), remaining)
	c, e := b.Store.Cycle(ctx, u.Key)
	if e != nil {
		return "", e
	}
	if c != nil {
		total, e := b.Store.Usage(ctx, u.Key, time.Unix(0, 0))
		if e != nil {
			return "", e
		}
		used := max(0, total-c.Baseline)
		line += fmt.Sprintf("\n周期已用 %s / 剩余 %s，到期 %s", Human(used), Human(max(0, c.Allowance-used)), time.Unix(c.Expires, 0).In(b.TZ).Format("2006-01-02 15:04"))
	}
	if expiry > 0 {
		line += "\n原有效期 " + time.Unix(expiry, 0).In(b.TZ).Format("2006-01-02 15:04")
	} else if c == nil {
		line += "，永久"
	}
	return line, nil
}
func userButtons(st *pending) *tg.Keyboard {
	button := func(label, action string) tg.Button { return tg.B(label, "user:"+action+":"+st.Token+":"+ref(st.User)) }
	return tg.K([]tg.Button{button("更换出站", "change"), button("分享链接", "share")}, []tg.Button{button("启用 / 暂停", "toggle"), button("流量与有效期", "policy")}, []tg.Button{button("删除用户", "remove")}, []tg.Button{tg.B("返回列表", fmt.Sprintf("user:page:%s:%d", st.Token, st.Page)), tg.B("返回", "unified:menu")})
}
func (b *Bot) userDetail(ctx context.Context, m tg.Message, d Doc, u UnifiedUser, st *pending) error {
	text, e := b.userStatus(ctx, d, u)
	if e != nil {
		return e
	}
	return b.API.Edit(ctx, m.Chat.ID, m.ID, text+"\nUUID "+u.UUID, userButtons(st), false)
}
func (b *Bot) userAction(ctx context.Context, m tg.Message, d Doc, p []string, st *pending) error {
	if ref(st.User) != p[3] || (st.Mode != "user-detail" && st.Mode != "user-remove") {
		return errors.New("用户操作已失效")
	}
	u, ok := findUser(d, st.User)
	if !ok {
		return errors.New("用户不存在")
	}
	switch p[1] {
	case "share":
		links, e := ShareLinks(d, []string{u.Key}, b.Host, b.Config.Domain, b.Config.UnifiedPublicPort)
		if e != nil {
			return e
		}
		return b.API.Edit(ctx, m.Chat.ID, m.ID, strings.Join(links, "\n\n"), tg.K([]tg.Button{tg.B("复制说明", "unified:copy"), tg.B("返回用户", "user:detail:"+st.Token+":"+ref(u.Key))}), false)
	case "policy":
		st.Mode = "entry-input-policy"
		st.User = u.Key
		st.Until = time.Now().Add(5 * time.Minute)
		text, e := b.userStatus(ctx, d, u)
		if e != nil {
			return e
		}
		return b.API.Edit(ctx, m.Chat.ID, m.ID, text+"\n发送：流量MB 天数（例如 2048 7），重设周期可用量；历史保留。\n原月额度：/limit "+u.Name+" GiB\n原有效期：/setexpiry "+u.Name+" 7d", tg.K([]tg.Button{tg.B("返回用户", "user:list")}), false)
	case "remove":
		st.Mode = "user-remove"
		st.Token, _ = randomHex(8)
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "确认删除用户 "+u.Name+"？仅移除 UUID、专属路由和 selector，保留历史流量与策略记录。", tg.K([]tg.Button{tg.B("确认删除", "user:removeok:"+st.Token+":"+ref(u.Key)), tg.B("返回", "user:list")}), false)
	case "removeok":
		if st.Mode != "user-remove" {
			return errors.New("请先确认删除")
		}
		version := st.Version
		page := st.Page
		*st = pending{}
		for _, raw := range list(d["inbounds"]) {
			in := obj(raw)
			if str(in["tag"]) != u.Inbound {
				continue
			}
			var users []any
			for _, x := range list(in["users"]) {
				if str(obj(x)["name"]) != u.Name {
					users = append(users, x)
				}
			}
			in["users"] = users
		}
		route := nested(d, "route")
		var rules []any
		for _, raw := range list(route["rules"]) {
			if !ruleForUser(obj(raw), u) {
				rules = append(rules, raw)
			}
		}
		route["rules"] = rules
		var outs []any
		for _, raw := range list(d["outbounds"]) {
			if str(obj(raw)["tag"]) != userOutbound(u.Key) {
				outs = append(outs, raw)
			}
		}
		d["outbounds"] = outs
		if e := b.Config.applyUnifiedVersion(ctx, d, version); e != nil {
			return e
		}
		return b.userPage(ctx, m, d, st, page, true)
	case "toggle":
		st.Token = ""
		manual, e := b.Store.Manual(ctx)
		if e != nil {
			return e
		}
		cmd := "block"
		if manual[u.Key] {
			cmd = "unblock"
		}
		if e = b.command(ctx, m, cmd, []string{u.Key}, st); e != nil {
			return e
		}
		fresh, e := Load(b.Config.Path)
		if e != nil {
			return e
		}
		return b.userPage(ctx, m, fresh, st, st.Page, true)
	}
	return errors.New("未知用户操作")
}
func (b *Bot) outboundPage(ctx context.Context, m tg.Message, d Doc, st *pending, page int) error {
	var tags []string
	for _, raw := range list(d["outbounds"]) {
		tag := str(obj(raw)["tag"])
		if tag != ManagedBlock && !managedUserOutbound(tag) {
			tags = append(tags, tag)
		}
	}
	pages := max(1, (len(tags)+userPageSize-1)/userPageSize)
	page = max(0, min(page, pages-1))
	token, e := randomHex(8)
	if e != nil {
		return e
	}
	st.Token = token
	k := tg.K()
	selecting := st.Mode == "user-out"
	for _, tag := range tags[min(page*userPageSize, len(tags)):min((page+1)*userPageSize, len(tags))] {
		address := "本机出口"
		for _, raw := range list(d["outbounds"]) {
			o := obj(raw)
			if str(o["tag"]) == tag && str(o["server"]) != "" {
				address = str(o["server"])
			}
		}
		action := "outinfo"
		if selecting {
			action = "out"
		}
		k.Rows = append(k.Rows, []tg.Button{tg.B(tag+" · "+address, "user:"+action+":"+st.Token+":"+ref(tag))})
	}
	nav := []tg.Button{}
	action := "outlistpage"
	if selecting {
		action = "outpage"
	}
	if page > 0 {
		nav = append(nav, tg.B("上一页", fmt.Sprintf("user:%s:%s:%d", action, st.Token, page-1)))
	}
	nav = append(nav, tg.B(fmt.Sprintf("%d / %d", page+1, pages), "user:noop:"+st.Token+":0"))
	if page+1 < pages {
		nav = append(nav, tg.B("下一页", fmt.Sprintf("user:%s:%s:%d", action, st.Token, page+1)))
	}
	k.Rows = append(k.Rows, nav, []tg.Button{tg.B("返回用户", "user:list"), tg.B("返回", "unified:menu")})
	return b.API.Edit(ctx, m.Chat.ID, m.ID, "出站 tag / 地址；选择后检测实际出口 IP，并再次确认保存。", k, false)
}
func (b *Bot) unifiedMenu(ctx context.Context, m tg.Message, d Doc, cb string, st *pending) error {
	if b.Forward != nil {
		b.Forward.Cancel(m.From.ID)
	}
	if strings.HasPrefix(cb, "unified:confirm:") {
		return b.realityConfirm(ctx, m, d, cb, st)
	}
	k := tg.K([]tg.Button{tg.B("返回", "unified:menu")})
	switch cb {
	case "unified:menu":
		*st = pending{}
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "🌐 统一入口管理", tg.K([]tg.Button{tg.B("👥 UUID 用户管理", "user:list")}, []tg.Button{tg.B("🔗 出站 IP 绑定", "user:list"), tg.B("出站节点", "unified:outbounds")}, []tg.Button{tg.B("🌍 REALITY 伪装域名", "unified:reality")}, []tg.Button{tg.B("📋 分享链接", "unified:share")}, []tg.Button{tg.B("📊 流量与有效期", "unified:status")}, []tg.Button{tg.B("⚙️ 入口设置", "unified:settings")}, []tg.Button{tg.B("返回主页", "panel")}), false)
	case "unified:share", "unified:status":
		return b.userPage(ctx, m, d, st, 0, true)
	case "unified:outbounds":
		token, e := randomHex(8)
		if e != nil {
			return e
		}
		*st = pending{Mode: "out-list", Token: token, Version: configVersion(d), ChatID: m.Chat.ID, MessageID: m.ID, Until: time.Now().Add(5 * time.Minute)}
		return b.outboundPage(ctx, m, d, st, 0)
	case "unified:copy":
		return b.send(ctx, m.Chat.ID, "长按上一条消息中的完整 vless:// 链接复制，再在 v2rayN 中选择「从剪贴板导入分享链接」。普通 Telegram 按钮无法写入系统剪贴板；返回用户可重新生成链接。", tg.K([]tg.Button{tg.B("返回用户", "user:list")}))
	case "unified:reality", "unified:settings":
		lines := []string{}
		for _, raw := range list(d["inbounds"]) {
			in := obj(raw)
			if !isUnified(in) {
				continue
			}
			tls := obj(in["tls"])
			h := obj(obj(tls["reality"])["handshake"])
			lines = append(lines, fmt.Sprintf("%s\nVLESS + REALITY · TCP · Vision\n公网 TCP %d，内部 %s:%s\nSNI %s\nhandshake %s:%s", str(in["tag"]), b.Config.UnifiedPublicPort, str(in["listen"]), str(in["listen_port"]), str(tls["server_name"]), str(h["server"]), str(h["server_port"])))
		}
		if cb == "unified:reality" {
			k.Rows = append([][]tg.Button{{tg.B("自定义域名", "unified:editreality")}}, k.Rows...)
		}
		return b.API.Edit(ctx, m.Chat.ID, m.ID, strings.Join(lines, "\n\n"), k, false)
	case "unified:editreality":
		*st = pending{Mode: "entry-input-reality", Version: configVersion(d), ChatID: m.Chat.ID, MessageID: m.ID, Until: time.Now().Add(5 * time.Minute)}
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "输入目标域名，默认同时用于 SNI 和 handshake:443。\n也可输入：SNI域名 handshake域名:端口\n检查 DNS、TLS1.3、证书和 HAProxy 分流后预览确认。", k, false)
	}
	return errors.New("未知统一入口菜单")
}
func (b *Bot) entryInput(ctx context.Context, m tg.Message, st *pending) error {
	if time.Now().After(st.Until) || st.ChatID != m.Chat.ID {
		return errors.New("输入已过期，请重新打开菜单")
	}
	d, e := Load(b.Config.Path)
	if e != nil {
		return e
	}
	if configVersion(d) != st.Version {
		return errors.New("配置已变化，请重新打开菜单")
	}
	if st.Mode == "entry-input-add" {
		var inbound string
		for _, raw := range list(d["inbounds"]) {
			in := obj(raw)
			if isUnified(in) {
				if inbound != "" {
					return errors.New("多个统一入口，请使用 /adduser 指定入口")
				}
				inbound = str(in["tag"])
			}
		}
		args := strings.Fields(m.Text)
		if len(args) < 2 || len(args) > 3 {
			return errors.New("输入：用户名称 出站tag [UUID]")
		}
		return b.command(ctx, m, "adduser", append([]string{inbound}, args...), st)
	}
	if st.Mode == "entry-input-policy" {
		return b.allLimit(ctx, m, d, append([]string{st.User}, strings.Fields(m.Text)...), st)
	}
	return b.realityPreview(ctx, m, d, st)
}

// Keep parsing strict for callbacks, including numeric overflow.
func callbackPage(s string) (int, error) {
	n, e := strconv.Atoi(s)
	if e != nil || n < 0 {
		return 0, errors.New("页码无效")
	}
	return n, nil
}
