package sbbot

import (
	"context"
	"errors"
	"fmt"
	tg "local/bots/internal/telegram"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (b *Bot) selection(ctx context.Context, m tg.Message, d Doc, st *pending) error {
	k := tg.K()
	for _, kind := range []string{"inbound", "outbound"} {
		if st.Mode == "share" && kind == "outbound" {
			continue
		}
		row := []tg.Button{}
		for _, v := range list(d[kind+"s"]) {
			tag := str(obj(v)["tag"])
			if managedUserOutbound(tag) || tag == ManagedBlock || st.Mode == "delete" && tag == "direct" {
				continue
			}
			label := kind + ": " + tag
			selected := slices.Contains(st.Tags, tag) || st.Kind == kind && st.Tag == tag
			if selected {
				label = "✅ " + label
			}
			data := st.Mode + ":pick:" + tag
			if st.Mode == "delete" {
				data = "delete:pick:" + kind + ":" + tag
			}
			if st.Mode == "share" {
				data = "sharepick:" + tag
			}

			row = append(row, tg.B(label, data))
			if len(row) == 2 {
				k.Rows = append(k.Rows, row)
				row = nil
			}
		}
		if len(row) > 0 {
			k.Rows = append(k.Rows, row)
		}
	}
	done := st.Mode + ":save"
	if st.Mode == "share" {
		done = "sharedone"
	}
	label, text := "✅ 保存 / 生成", "选择 tag 后点击保存 / 生成"
	if st.Mode == "bind" {
		label, text = "✅ 保存绑定", "选择入站（可多选）和出站（只能选一个），然后点击保存绑定"
	} else if st.Mode == "delete" {
		label, text = "确认删除", "选择要删除的入站或出站，然后点击确认删除"
	}

	// Paginate existing selection rows; keep the established selection/save logic.
	count := 0
	for _, row := range k.Rows {
		count += len(row)
	}
	pages := max(1, (count+userPageSize-1)/userPageSize)
	st.Page = max(0, min(st.Page, pages-1))
	first, last := st.Page*userPageSize, min((st.Page+1)*userPageSize, count)
	rows := [][]tg.Button{}
	index := 0
	for _, row := range k.Rows {
		visible := []tg.Button{}
		for _, button := range row {
			if index >= first && index < last {
				visible = append(visible, button)
			}
			index++
		}
		if len(visible) > 0 {
			rows = append(rows, visible)
		}
	}
	k.Rows = rows
	nav := []tg.Button{}
	if st.Page > 0 {
		nav = append(nav, tg.B("上一页", fmt.Sprintf("page:%d", st.Page-1)))
	}
	nav = append(nav, tg.B(fmt.Sprintf("%d / %d", st.Page+1, pages), "noop"))
	if st.Page+1 < pages {
		nav = append(nav, tg.B("下一页", fmt.Sprintf("page:%d", st.Page+1)))
	}
	k.Rows = append(k.Rows, nav, []tg.Button{tg.B(label, done), tg.B("返回", "proto:menu")})
	token, e := randomHex(8)
	if e != nil {
		return e
	}
	st.Token = token
	st.Version = configVersion(d)
	st.ChatID = m.Chat.ID
	st.MessageID = m.ID
	st.Until = time.Now().Add(5 * time.Minute)
	st.Healthy = map[string]string{}
	for i, row := range k.Rows {
		for j, button := range row {
			if button.Data == "proto:menu" {
				continue
			}
			id := ref(button.Data)
			st.Healthy[id] = button.Data
			k.Rows[i][j].Data = "select:" + token + ":" + id
		}
	}

	err := b.API.Edit(ctx, m.Chat.ID, m.ID, text, k, false)
	var apiErr *tg.APIError
	if errors.As(err, &apiErr) && strings.Contains(apiErr.Description, "message is not modified") {
		return nil
	}
	return err
}
func (b *Bot) callback(ctx context.Context, m tg.Message, cb string, st *pending) error {
	reply := func(s string) error { return b.send(ctx, m.Chat.ID, s, back()) }
	if strings.HasPrefix(cb, "bwh:") {
		return b.bwhCallback(ctx, m, strings.TrimPrefix(cb, "bwh:"), st)
	}
	if cb == "help" {
		return b.command(ctx, m, "help", nil, st)
	}
	if cb == "logs" {
		return b.logs(ctx, m)
	}
	if strings.HasPrefix(cb, "ufw:") {
		p := strings.Split(cb, ":")
		if len(p) > 2 {
			return reply("发送：/ufw " + p[2] + " <端口> [tcp|udp|both]")
		}
		return b.ufw(ctx, m, []string{"list"})
	}
	if cb == "proto:menu" {
		return b.send(ctx, m.Chat.ID, "🔐 协议管理", protocolMenu())
	}
	if cmd := map[string]string{"out:add": "addout", "proto:exp": "setexpiry", "proto:vless": "addvless", "proto:anytls": "addanytls", "proto:hy2": "addhy2"}[cb]; cmd != "" {
		return b.command(ctx, m, cmd, nil, st)
	}
	d, err := Load(b.Config.Path)
	if err != nil {
		return err
	}
	if b.Config.ControlSocket != "" && st.Version != "" && st.Version != configVersion(d) && (strings.HasPrefix(cb, "select:") || strings.HasPrefix(cb, "bind:") || strings.HasPrefix(cb, "delete:") || strings.HasPrefix(cb, "user:") && cb != "user:list" && !strings.HasPrefix(cb, "user:page:") || strings.HasPrefix(cb, "unified:confirm:")) {
		d, err = snapshot(st.Version)
		if err != nil {
			return err
		}
	}
	if strings.HasPrefix(cb, "select:") {
		p := strings.Split(cb, ":")
		if len(p) != 3 || p[1] != st.Token || st.Token == "" || st.ChatID != m.Chat.ID || st.MessageID != m.ID || time.Now().After(st.Until) || st.Version != configVersion(d) {
			return errors.New("列表已过期，请重新打开")
		}
		original := st.Healthy[p[2]]
		if original == "" {
			return errors.New("按钮已失效")
		}
		if original == "noop" {
			return nil
		}
		if strings.HasPrefix(original, "page:") {
			page, e := callbackPage(strings.TrimPrefix(original, "page:"))
			if e != nil {
				return e
			}
			st.Page = page
			return b.selection(ctx, m, d, st)
		}
		st.Token = ""
		cb = original
	} else if st.Token != "" && (strings.HasPrefix(cb, "sharepick:") || strings.HasPrefix(cb, "bind:pick:") || strings.HasPrefix(cb, "delete:pick:") || cb == "sharedone" || cb == "bind:save" || cb == "delete:save") {
		return errors.New("旧按钮已失效，请重新打开列表")
	}
	if strings.HasPrefix(cb, "unified:") {
		return b.unifiedMenu(ctx, m, d, cb, st)
	}
	if strings.HasPrefix(cb, "user:") {
		return b.userCallback(ctx, m, d, cb, st)
	}
	if strings.HasPrefix(cb, "proto:list:") {
		return b.protocols(ctx, m, d, int(integer(strings.TrimPrefix(cb, "proto:list:"))))
	}
	if strings.HasPrefix(cb, "traffic:") {
		return b.trafficCallback(ctx, m, d, cb, st)
	}
	if strings.HasPrefix(cb, "use:") || cb == "blocks" {
		period := strings.TrimPrefix(cb, "use:")
		return b.trafficPage(ctx, m, d, st, period, 0, true)
	}

	if cb == "share" || cb == "bind:start" || cb == "delete:start" {
		mode := strings.Split(cb, ":")[0]
		*st = pending{Mode: mode}
		return b.selection(ctx, m, d, st)
	}
	if cb == "delete:noop" || cb == "bind:noop" {
		return nil
	}
	if cb == "delete:cancel" || cb == "bind:cancel" {
		*st = pending{}
		return reply("已取消")
	}
	if strings.HasPrefix(cb, "sharepick:") || strings.HasPrefix(cb, "bind:pick:") {
		mode := "bind"
		tag := strings.TrimPrefix(cb, "bind:pick:")
		if strings.HasPrefix(cb, "sharepick:") {
			mode = "share"
			tag = strings.TrimPrefix(cb, "sharepick:")
		}
		if st.Mode != mode {
			return errors.New("按钮已失效，请重新选择")
		}
		kind := Kind(d, tag)
		if kind == "" || mode == "share" && kind != "inbound" {
			return errors.New("tag 不存在")
		}
		if slices.Contains(st.Tags, tag) {
			st.Tags = slices.DeleteFunc(st.Tags, func(s string) bool { return s == tag })
		} else {
			if mode == "bind" && kind == "outbound" {
				st.Tags = slices.DeleteFunc(st.Tags, func(s string) bool { return Kind(d, s) == "outbound" })
			}
			st.Tags = append(st.Tags, tag)
		}
		return b.selection(ctx, m, d, st)
	}
	if strings.HasPrefix(cb, "delete:pick:") {
		p := strings.SplitN(cb, ":", 4)
		if st.Mode != "delete" || len(p) != 4 || Kind(d, p[3]) != p[2] {
			return errors.New("选择已失效")
		}
		st.Kind = p[2]
		st.Tag = p[3]
		return b.selection(ctx, m, d, st)
	}
	if cb == "sharedone" {
		if st.Mode != "share" || len(st.Tags) == 0 {
			return errors.New("请先选择入站")
		}
		return b.command(ctx, m, "share", st.Tags, st)
	}
	if cb == "bind:save" {
		if st.Mode != "bind" {
			return errors.New("按钮已失效，请重新选择")
		}
		var inbounds []string
		var outbounds []string
		for _, tag := range st.Tags {
			switch Kind(d, tag) {
			case "inbound":
				inbounds = append(inbounds, tag)
			case "outbound":
				outbounds = append(outbounds, tag)
			default:
				return errors.New("选择已失效，请重新选择")
			}
		}
		if len(inbounds) == 0 || len(outbounds) != 1 {
			return errors.New("请至少选择一个入站，并选择一个出站")
		}
		for _, tag := range inbounds {
			if err = Bind(d, tag, outbounds[0]); err != nil {
				return err
			}
		}
		if err = b.Config.Apply(ctx, d, nil); err != nil {
			return err
		}
		if err = b.Store.Log(ctx, strconv.FormatInt(m.From.ID, 10), "bind_route", strings.Join(st.Tags, " -> ")); err != nil {
			return err
		}
		*st = pending{}
		return reply("✅ 已保存绑定")
	}
	if cb == "delete:save" || strings.HasPrefix(cb, "protodel:") {
		kind, tag := st.Kind, st.Tag
		if strings.HasPrefix(cb, "protodel:") {
			kind = "inbound"
			tag = strings.TrimPrefix(cb, "protodel:")
		} else if st.Mode != "delete" || tag == "" {
			return errors.New("请先选择 tag")
		}
		removed, err := Delete(d, kind, tag)
		if err != nil {
			return err
		}
		if err = b.Config.Apply(ctx, d, nil); err != nil {
			return err
		}
		if kind == "inbound" {
			port := str(removed["listen_port"])
			if str(removed["type"]) == "hysteria2" {
				port += "/udp"
			}
			if b.Features["ufw"] {
				if _, err = b.Config.Run(ctx, "/usr/sbin/ufw", "delete", "allow", port); err != nil {
					return fmt.Errorf("配置已删除，但防火墙清理失败：%w", err)
				}
			}
		}
		if err = b.Store.Log(ctx, strconv.FormatInt(m.From.ID, 10), "delete_tag", kind+":"+tag); err != nil {
			return err
		}
		*st = pending{}
		return reply("✅ 已删除 " + kind + "：" + tag)
	}
	return errors.New("未知操作，请重新打开面板")
}
func (b *Bot) ufw(ctx context.Context, m tg.Message, args []string) error {
	action := "list"
	if len(args) > 0 {
		action = args[0]
	}
	var argv []string
	if action == "list" {
		argv = []string{"status", "numbered"}
	} else {
		if action != "allow" && action != "delete" || len(args) < 2 || len(args) > 3 {
			return b.send(ctx, m.Chat.ID, "用法：/ufw list|allow|delete <端口> [tcp|udp|both]", back())
		}
		if !regexp.MustCompile(`^\d{1,5}$`).MatchString(args[1]) || integer(args[1]) < 1 || integer(args[1]) > 65535 {
			return errors.New("端口必须在 1-65535")
		}
		protocol := "both"
		if len(args) > 2 {
			protocol = args[2]
		}
		if !slices.Contains([]string{"tcp", "udp", "both"}, protocol) {
			return errors.New("协议必须是 tcp、udp 或 both")
		}
		port := args[1]
		if protocol != "both" {
			port += "/" + protocol
		}
		argv = []string{"allow", port}
		if action == "delete" {
			argv = append([]string{"delete"}, argv...)
		}
	}
	text, err := b.Config.Run(ctx, "/usr/sbin/ufw", argv...)
	if err != nil {
		return err
	}
	if action != "list" {
		if err = b.Store.Log(ctx, strconv.FormatInt(m.From.ID, 10), "ufw", strings.Join(argv, " ")); err != nil {
			return err
		}
	}
	return b.send(ctx, m.Chat.ID, text, tg.K([]tg.Button{tg.B("放行端口", "ufw:help:allow"), tg.B("删除端口", "ufw:help:delete")}, []tg.Button{tg.B("刷新", "ufw:list"), tg.B("返回面板", "panel")}))
}
