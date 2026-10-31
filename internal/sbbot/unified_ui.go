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
	return b.send(ctx, m.Chat.ID, "点击 UUID，选择出站 IP，再确认绑定。出站 IP 按钮显示配置中的上游地址，选择时会实时验证实际出口 IP。", k)
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
		st.Mode = "user-detail"
		st.User = u.Key
		st.MessageID = m.ID
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "用户 "+u.Name+"\nUUID "+u.UUID+"\n当前出站 "+u.Outbound, tg.K([]tg.Button{tg.B("更换出站", "user:change:"+st.Token+":"+ref(u.Key)), tg.B("取消", "cancel")}), false)
	case "change":
		if st.Mode != "user-detail" || st.MessageID != m.ID || ref(st.User) != p[3] {
			return errors.New("请先选择 UUID")
		}
		st.Mode = "user-out"
		st.Healthy = map[string]string{}
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
			if b.Config.Probe != nil {
				var err error
				address, err = b.Config.Probe(ctx, d, tag)
				if err != nil {
					continue
				}
			}
			st.Healthy[tag] = address
			k.Rows = append(k.Rows, []tg.Button{tg.B(tag+" · "+address, "user:out:"+st.Token+":"+ref(tag))})
		}
		k.Rows = append(k.Rows, []tg.Button{tg.B("取消", "cancel")})
		return b.API.Edit(ctx, m.Chat.ID, m.ID, "选择健康出站及实际出口 IP：", k, false)
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
		return b.send(ctx, m.Chat.ID, "✅ 已绑定并保存："+key+" → "+out, back())
	}
	return errors.New("未知用户操作")
}

func (b *Bot) unifiedMenu(ctx context.Context, m tg.Message, d Doc, cb string, st *pending) error {
	if cb == "unified:menu" {
		return b.send(ctx, m.Chat.ID, "🌐 统一入口管理", tg.K(
			[]tg.Button{tg.B("🔗 UUID 出站绑定", "user:list")},
			[]tg.Button{tg.B("🌍 出站 IP 管理", "unified:outbounds")},
			[]tg.Button{tg.B("👥 入站用户管理", "user:list")},
			[]tg.Button{tg.B("📊 绑定状态", "unified:status")},
			[]tg.Button{tg.B("⚙️ REALITY 设置", "unified:reality")},
			[]tg.Button{tg.B("⬅️ 返回", "panel")}))
	}
	k := tg.K([]tg.Button{tg.B("⬅️ 返回", "unified:menu")})
	lines := []string{}
	switch cb {
	case "unified:status":
		for _, u := range UnifiedUsers(d) {
			lines = append(lines, u.Name+" · "+u.UUID+" → "+u.Outbound)
		}
	case "unified:outbounds":
		for _, raw := range list(d["outbounds"]) {
			o := obj(raw)
			tag := str(o["tag"])
			if tag == ManagedBlock || managedUserOutbound(tag) {
				continue
			}
			ip := "未验证"
			if b.Config.Probe != nil {
				var err error
				ip, err = b.Config.Probe(ctx, d, tag)
				if err != nil {
					ip = "暂不可用"
				}
			}
			lines = append(lines, tag+" · "+ip)
		}
		k.Rows = append([][]tg.Button{{tg.B("添加出站", "out:add"), tg.B("删除出站", "delete:start")}}, k.Rows...)
	case "unified:reality":
		for _, raw := range list(d["inbounds"]) {
			in := obj(raw)
			if !isUnified(in) {
				continue
			}
			tls := obj(in["tls"])
			r := obj(tls["reality"])
			h := obj(r["handshake"])
			lines = append(lines, str(in["tag"])+"\nVLESS · TCP · xtls-rprx-vision\n公网 TCP "+strconv.Itoa(b.Config.UnifiedPublicPort)+"\n内部 "+str(in["listen"])+":"+str(in["listen_port"])+"\nSNI "+str(tls["server_name"])+"\n握手 "+str(h["server"])+":"+str(h["server_port"])+"\n设置由统一 443 配置管理；/share 用户标识 生成链接。")
		}
	default:
		return errors.New("未知统一入口菜单")
	}
	if len(lines) == 0 {
		lines = append(lines, "尚无统一入口用户")
	}
	return b.send(ctx, m.Chat.ID, strings.Join(lines, "\n"), k)
}
