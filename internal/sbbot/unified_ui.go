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
	token, err := randomHex(8)
	if err != nil {
		return err
	}
	*st = pending{Mode: "users", Token: token, Version: configVersion(d), Until: time.Now().Add(5 * time.Minute), ChatID: m.Chat.ID}
	k := tg.K()
	for _, u := range UnifiedUsers(d) {
		label := u.UUID + " · " + u.Name + " → " + u.Outbound
		k.Rows = append(k.Rows, []tg.Button{tg.B(label, "user:pick:"+token+":"+ref(u.Key))})
	}
	k.Rows = append(k.Rows, []tg.Button{tg.B("添加用户", "user:add"), tg.B("返回", "panel")})
	return b.send(ctx, m.Chat.ID, "点击 UUID，选择出站 IP，再确认绑定。出站 IP 按钮显示配置中的上游地址，实际落地 IP 以探测报告为准。", k)
}
func (b *Bot) userCallback(ctx context.Context, m tg.Message, d Doc, cb string, st *pending) error {
	if cb == "user:list" {
		return b.userMenu(ctx, m, d, st)
	}
	if cb == "user:add" {
		return b.send(ctx, m.Chat.ID, "发送 /adduser <统一入口 tag> <用户名称> <出站 tag> [UUID]；旧入站不会自动合并。", back())
	}
	p := strings.Split(cb, ":")
	if len(p) != 4 || p[2] != st.Token || st.Token == "" || time.Now().After(st.Until) || st.ChatID != m.Chat.ID || st.Version != configVersion(d) {
		return errors.New("选择已过期或配置已变化，请重新打开 UUID 用户菜单")
	}
	switch p[1] {
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
		if st.Mode != "users" && st.Mode != "user-out" {
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
		st.Mode = "user-out"
		st.User = u.Key
		st.MessageID = m.ID
		k := tg.K()
		for _, raw := range list(d["outbounds"]) {
			out := obj(raw)
			tag := str(out["tag"])
			if tag == ManagedBlock || managedUserOutbound(tag) {
				continue
			}
			address := str(out["server"])
			if address == "" {
				address = "本机出口"
			}
			k.Rows = append(k.Rows, []tg.Button{tg.B(tag+" · "+address, "user:out:"+st.Token+":"+ref(tag))})
		}
		k.Rows = append(k.Rows, []tg.Button{tg.B("取消", "cancel")})
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "用户 "+u.Name+"\nUUID "+u.UUID+"\n选择出站 IP（上游地址）：", k, false)
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
		if out == "" {
			return errors.New("出站不存在")
		}
		st.Outbound = out
		st.Mode = "user-confirm"
		token, err := randomHex(8)
		if err != nil {
			return err
		}
		st.Token = token
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "确认绑定 "+st.User+" → "+out+"？已有连接将随 sing-box 重载断开并重新连接。", tg.K([]tg.Button{tg.B("确认绑定", "user:confirm:"+token+":"+ref(st.User)), tg.B("取消", "cancel")}), false)
	case "confirm":
		if st.Mode != "user-confirm" || st.MessageID != m.ID || ref(st.User) != p[3] {
			return errors.New("确认已失效")
		}
		key, out, version := st.User, st.Outbound, st.Version
		*st = pending{} // consume before any mutation; no replay on partial failure
		if err := BindUser(d, key, out); err != nil {
			return err
		}
		if err := b.Config.applyUnifiedVersion(ctx, d, version); err != nil {
			return err
		}
		if err := b.Store.Log(ctx, strconv.FormatInt(m.From.ID, 10), "bind_auth_user", key+" -> "+out); err != nil {
			return fmt.Errorf("绑定已生效，但审计日志写入失败：%w", err)
		}
		return b.send(ctx, m.Chat.ID, "✅ 已绑定并保存："+key+" → "+out, back())
	}
	return errors.New("未知用户操作")
}
