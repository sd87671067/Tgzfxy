package sbbot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	tg "local/bots/internal/telegram"
	"strconv"
	"strings"
	"time"
)

func ref(s string) string { h := sha256.Sum256([]byte(s)); return fmt.Sprintf("%x", h[:8]) }
func (b *Bot) userMenu(ctx context.Context, m tg.Message, d Doc, st *pending) error {
	return b.userPage(ctx, m, d, st, 0, false)
}

const userPageSize = 8 // Same page size as the protocol menu.

func (b *Bot) userPage(ctx context.Context, m tg.Message, d Doc, st *pending, page int, edit bool) error {
	token, err := randomHex(8)
	if err != nil {
		return err
	}
	users := UnifiedUsers(d)
	pages := max(1, (len(users)+userPageSize-1)/userPageSize)
	page = max(0, min(page, pages-1))
	next := pending{Mode: "users", Token: token, Version: configVersion(d), Until: time.Now().Add(5 * time.Minute), ChatID: m.Chat.ID, Page: page}
	k := tg.K()
	summaries := []string{}
	for _, u := range users[min(page*userPageSize, len(users)):min((page+1)*userPageSize, len(users))] {
		label := u.Name + " → " + u.Outbound
		status, e := b.userStatus(ctx, d, u)
		if e != nil {
			return e
		}
		summaries = append(summaries, status)
		k.Rows = append(k.Rows, []tg.Button{tg.B(label, "user:pick:"+token+":"+ref(u.Key))})
	}
	nav := []tg.Button{}
	if page > 0 {
		nav = append(nav, tg.B("上一页", fmt.Sprintf("user:page:%s:%d", token, page-1)))
	}
	nav = append(nav, tg.B(fmt.Sprintf("%d / %d", page+1, pages), "user:noop:"+token+":0"))
	if page+1 < pages {
		nav = append(nav, tg.B("下一页", fmt.Sprintf("user:page:%s:%d", token, page+1)))
	}
	k.Rows = append(k.Rows, nav)
	k.Rows = append(k.Rows, []tg.Button{tg.B("添加用户", "user:add"), tg.B("返回", "unified:menu")})
	text := fmt.Sprintf("入站用户绑定（共 %d 人，第 %d / %d 页）\n点击用户查看当前绑定，再选择出站并确认保存。", len(users), page+1, pages) + "\n" + strings.Join(summaries, "\n")
	if edit {
		err = b.API.Edit(ctx, m.Chat.ID, m.ID, text, k, false)
		next.MessageID = m.ID
	} else {
		var sent tg.Message
		sent, err = b.API.Send(ctx, m.Chat.ID, text, k, false)
		next.MessageID = sent.ID
	}
	if err == nil {
		*st = next
	}
	return err
}
func (b *Bot) userCallback(ctx context.Context, m tg.Message, d Doc, cb string, st *pending) error {
	if cb == "user:list" {
		return b.userPage(ctx, m, d, st, 0, true)
	}
	if cb == "user:add" {
		*st = pending{Mode: "entry-input-add", Version: configVersion(d), ChatID: m.Chat.ID, MessageID: m.ID, Until: time.Now().Add(5 * time.Minute)}
		if b.Forward != nil {
			b.Forward.Cancel(m.From.ID)
		}
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "输入：用户名称 出站tag [UUID]（例如 zs direct）。多个统一入口时请使用 /adduser <统一入口tag> <名称> <出站> [UUID]。", tg.K([]tg.Button{tg.B("返回用户", "user:list")}), false)
	}
	p := strings.Split(cb, ":")
	if len(p) != 4 || p[2] != st.Token || st.Token == "" || time.Now().After(st.Until) || st.ChatID != m.Chat.ID || (st.MessageID != 0 && st.MessageID != m.ID) {
		return errors.New("选择已过期，请重新打开入站用户绑定菜单")
	}
	if p[1] == "page" {
		page, err := strconv.Atoi(p[3])
		if err != nil || page < 0 {
			return errors.New("页码无效")
		}
		return b.userPage(ctx, m, d, st, page, true)
	}
	if st.Version != configVersion(d) {
		return errors.New("配置已变化，请重新打开入站用户绑定菜单")
	}
	switch p[1] {
	case "detail":
		if ref(st.User) != p[3] {
			return errors.New("用户会话无效")
		}
		u, ok := findUser(d, st.User)
		if !ok {
			return errors.New("用户不存在")
		}
		st.Mode = "user-detail"
		return b.userDetail(ctx, m, d, u, st)
	case "outlistpage":
		if st.Mode != "out-list" {
			return errors.New("会话已过期")
		}
		page, e := callbackPage(p[3])
		if e != nil {
			return e
		}
		return b.outboundPage(ctx, m, d, st, page)
	case "outinfo":
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "通过「出站 IP 绑定」先选择 UUID，再检测目标出站实际 IP 并确认保存。", tg.K([]tg.Button{tg.B("选择用户", "user:list"), tg.B("返回", "unified:outbounds")}), false)
	case "toggle", "remove", "removeok", "share", "policy":
		return b.userAction(ctx, m, d, p, st)
	case "outpage":
		page, e := strconv.Atoi(p[3])
		if e != nil || page < 0 || st.Mode != "user-out" {
			return errors.New("页码或会话无效")
		}
		return b.outboundPage(ctx, m, d, st, page)
	case "noop":
		return nil
	case "history":
		if st.Mode != "user-history" || ref(st.User) != p[3] {
			return errors.New("历史映射确认已失效")
		}
		key, legacy := st.User, st.Tag
		*st = pending{}
		if !b.Config.AutoRestart {
			return errors.New("未启用配置检查与重载，未关联历史")
		}
		if err := b.Store.AdoptLegacy(ctx, key, legacy, func() error { return b.enforce(ctx, d) }); err != nil {
			return err
		}
		return b.send(ctx, m.Chat.ID, "✅ 已关联历史流量、额度、到期和手动暂停："+legacy+" → "+key, back())
	case "pick":
		if st.Mode != "users" {
			return errors.New("按钮已失效")
		}
		var u UnifiedUser
		for _, x := range UnifiedUsers(d) {
			if ref(x.Key) == p[3] {
				u = x
				break
			}
		}
		if u.Key == "" {
			return errors.New("用户不存在")
		}
		st.Mode = "user-detail"
		st.User = u.Key
		st.MessageID = m.ID
		return b.userDetail(ctx, m, d, u, st)
	case "change":
		if st.Mode != "user-detail" || st.MessageID != m.ID || ref(st.User) != p[3] {
			return errors.New("请先选择 UUID")
		}
		st.Mode = "user-out"
		st.Healthy = map[string]string{}
		for _, raw := range list(d["outbounds"]) {
			o := obj(raw)
			tag := str(o["tag"])
			if tag == ManagedBlock || managedUserOutbound(tag) {
				continue
			}
			address := str(o["server"])
			if address == "" {
				address = "本机出口"
			}
			st.Healthy[tag] = address
		}
		return b.outboundPage(ctx, m, d, st, 0)
	case "out":
		if st.Mode != "user-out" || st.MessageID != m.ID {
			return errors.New("请先选择 UUID")
		}
		out := ""
		for _, raw := range list(d["outbounds"]) {
			tag := str(obj(raw)["tag"])
			if ref(tag) == p[3] && tag != ManagedBlock && !managedUserOutbound(tag) {
				out = tag
			}
		}
		if out == "" || st.Healthy[out] == "" {
			return errors.New("出站不存在")
		}
		if b.Config.Probe != nil {
			ip, e := b.Config.Probe(ctx, d, out)
			if e != nil {
				return errors.New("目标出站检测失败，请选择其他出站")
			}
			st.Healthy[out] = ip
		}
		st.Outbound = out
		st.Mode = "user-confirm"
		token, err := randomHex(8)
		if err != nil {
			return err
		}
		st.Token = token
		u, _ := findUser(d, st.User)
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "确认绑定 "+u.Name+" → "+out+"？已有连接将随 sing-box 重载断开并重新连接。", tg.K([]tg.Button{tg.B("确认绑定", "user:confirm:"+token+":"+ref(st.User)), tg.B("取消", "cancel")}), false)
	case "confirm":
		if st.Mode != "user-confirm" || st.MessageID != m.ID || ref(st.User) != p[3] {
			return errors.New("确认已失效")
		}
		key, out, version, page := st.User, st.Outbound, st.Version, st.Page
		expectedIP := st.Healthy[out]
		if b.Config.Probe != nil {
			ip, err := b.Config.Probe(ctx, d, out)
			if err != nil {
				return err
			}
			if ip != expectedIP {
				return errors.New("出口 IP 已变化，请重新选择")
			}
		}
		*st = pending{} // consume before any mutation; no replay on partial failure
		if err := BindUser(d, key, out); err != nil {
			return err
		}
		if err := b.Config.applyUnifiedVerified(ctx, d, version, func() error {
			if b.Config.Probe == nil {
				return nil
			}
			ip, err := b.Config.Probe(ctx, d, key)
			if err != nil {
				return err
			}
			if ip != expectedIP {
				return errors.New("用户实际出口与目标不一致")
			}
			return nil
		}); err != nil {
			return err
		}
		if err := b.Store.Log(ctx, strconv.FormatInt(m.From.ID, 10), "bind_auth_user", key+" -> "+out); err != nil {
			return fmt.Errorf("绑定已生效，但审计日志写入失败：%w", err)
		}
		u, _ := findUser(d, key)
		if err := b.send(ctx, m.Chat.ID, "✅ 已绑定并保存："+u.Name+" → "+out, nil); err != nil {
			return err
		}
		return b.userPage(ctx, m, d, st, page, true)
	}
	return errors.New("未知用户操作")
}
