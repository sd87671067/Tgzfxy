package sbbot

import (
	"context"
	"errors"
	"fmt"
	"local/bots/internal/forwarder"
	tg "local/bots/internal/telegram"
	"log"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type pending struct {
	Mode                           string
	Tags                           []string
	Kind, Tag, Token, File, Action string
	Until                          time.Time
	Version, User, Outbound        string
	ChatID, MessageID              int64
	Healthy                        map[string]string
	Page                           int
}
type Bot struct {
	API                                              *tg.Client
	Store                                            *Store
	Config                                           *ConfigManager
	Forward                                          *forwarder.Engine
	Admins                                           map[int64]bool
	Features                                         map[string]bool
	States                                           map[int64]*pending
	TZ                                               *time.Location
	HTTP                                             *http.Client
	Host, APIURL, APISecret, BWHURL, BWHKey, BWHVEID string
	Mu                                               sync.Mutex
}

func (b *Bot) InitStates()                      { b.States = map[int64]*pending{} }
func (b *Bot) CommandAvailable(cmd string) bool { return b.available(cmd, "") }
func (b *Bot) send(ctx context.Context, chat int64, text string, k *tg.Keyboard) error {
	r := []rune(text)
	if len(r) == 0 {
		text = "无数据"
		r = []rune(text)
	}
	for len(r) > 3900 {
		if _, err := b.API.Send(ctx, chat, string(r[:3900]), nil, false); err != nil {
			return err
		}
		r = r[3900:]
	}
	_, err := b.API.Send(ctx, chat, string(r), k, false)
	return err
}
func back() *tg.Keyboard {
	return tg.K([]tg.Button{tg.B("« 返回面板", "panel"), tg.B("取消操作", "cancel")})
}
func (b *Bot) panel() *tg.Keyboard {
	k := &tg.Keyboard{}
	if b.Features["singbox"] {
		k.Rows = append(k.Rows, []tg.Button{tg.B("📊 今日流量", "use:day"), tg.B("📅 本月流量", "use:month")}, []tg.Button{tg.B("🚦 用户额度", "user:list"), tg.B("🚧 封禁管理", "blocks")}, []tg.Button{tg.B("🌐 统一入口管理", "unified:menu"), tg.B("🔐 协议管理", "proto:menu")})
	}
	if b.Features["ufw"] {
		k.Rows = append(k.Rows, []tg.Button{tg.B("🧱 UFW端口管理", "ufw:list")})
	}
	if b.Features["bwh"] {
		k.Rows = append(k.Rows, []tg.Button{tg.B("🖥 服务器状态", "bwh:status"), tg.B("📶 本轮流量", "bwh:transfer")}, []tg.Button{tg.B("⏻ 电源控制", "bwh:power"), tg.B("🔑 重置root密码", "bwh:resetpwd")}, []tg.Button{tg.B("📸 快照管理", "bwh:snap")})
	}
	k.Rows = append(k.Rows, []tg.Button{tg.B("📨 Telegram频道转发助手", "fw:menu")}, []tg.Button{tg.B("📜 操作日志", "logs"), tg.B("取消操作", "cancel")}, []tg.Button{tg.B("❓ 使用帮助", "help")})
	return k
}
func protocolMenu() *tg.Keyboard {
	return tg.K([]tg.Button{tg.B("添加 Hysteria 2", "proto:hy2")}, []tg.Button{tg.B("添加 VLESS", "proto:vless"), tg.B("添加 AnyTLS", "proto:anytls")}, []tg.Button{tg.B("添加出站", "out:add"), tg.B("入站绑定出站", "bind:start")}, []tg.Button{tg.B("删除入站/出站", "delete:start"), tg.B("转化链接", "share")}, []tg.Button{tg.B("查看所有入站", "proto:list:0"), tg.B("设置有效期", "proto:exp")}, []tg.Button{tg.B("« 返回面板", "panel")})
}
func (b *Bot) available(cmd, cb string) bool {
	feature := ""
	if slices.Contains(strings.Fields("today month status usage alllimit block unblock share addout addvless addhy2 addanytls add protocols setexpiry users adduser adopthistory"), cmd) {
		feature = "singbox"
	}
	if cmd == "ufw" || strings.HasPrefix(cb, "ufw:") {
		feature = "ufw"
	}
	if cmd == "bwh" || strings.HasPrefix(cb, "bwh:") {
		feature = "bwh"
	}
	for _, p := range []string{"use:", "proto:", "share", "out:", "bind:", "block:", "delete:", "protodel:", "user:", "unified:", "select:", "traffic:"} {
		if strings.HasPrefix(cb, p) {
			feature = "singbox"
		}
	}
	if cb == "blocks" {
		feature = "singbox"
	}
	return feature == "" || b.Features[feature]
}
func (b *Bot) Handle(ctx context.Context, u tg.Update) error {
	start := time.Now()
	defer func() {
		log.Printf("bot_operation callback=%t duration_ms=%d", u.Callback != nil, time.Since(start).Milliseconds())
	}()
	var m tg.Message
	cb := ""
	if u.Callback != nil {
		if !b.Admins[u.Callback.From.ID] {
			return b.API.Answer(ctx, u.Callback.ID, "无权限。")
		}
		if u.Callback.Message == nil {
			return b.API.Answer(ctx, u.Callback.ID, "消息已失效")
		}
		m = *u.Callback.Message
		m.From = u.Callback.From
		m.Text = ""
		cb = u.Callback.Data
		if err := b.acknowledge(ctx, u.Callback); err != nil {
			return err
		}
	} else if u.Message != nil {
		m = *u.Message
	} else {
		return nil
	}
	if !b.Admins[m.From.ID] {
		return b.send(ctx, m.Chat.ID, "无权限。", nil)
	}
	fields := strings.Fields(m.Text)
	cmd := ""
	var args []string
	if len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
		cmd = strings.Split(strings.TrimPrefix(fields[0], "/"), "@")[0]
		args = fields[1:]
	}
	if !b.available(cmd, cb) {
		return b.send(ctx, m.Chat.ID, "本机未安装此功能或 .env 未配置所需 API。", b.panel())
	}
	if cmd == "cancel" || cmd == "start" || cmd == "panel" || cb == "panel" || cb == "cancel" {
		if b.Forward != nil {
			b.Forward.Cancel(m.From.ID)
		}
		b.Mu.Lock()
		delete(b.States, m.From.ID)
		b.Mu.Unlock()
		return b.send(ctx, m.Chat.ID, "🤖 sbbot 操作面板", b.panel())
	}
	forwardInput := strings.HasPrefix(cb, "fw:") || slices.Contains([]string{"bind", "switch", "cs"}, cmd) || (cmd == "" && cb == "")
	if b.Forward != nil && forwardInput {
		fwCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		handled, err := b.Forward.Handle(fwCtx, m, cb, cmd, args)
		cancel()
		if err != nil {
			return b.send(ctx, m.Chat.ID, "操作失败："+err.Error(), back())
		}
		if handled {
			return nil
		}
	} else if strings.HasPrefix(cb, "fw:") || slices.Contains([]string{"bind", "switch", "cs"}, cmd) {
		return b.send(ctx, m.Chat.ID, "请在 .env 中配置 FORWARD_API_ID 和 FORWARD_API_HASH。", back())
	}
	b.Mu.Lock()
	defer b.Mu.Unlock()
	st := b.States[m.From.ID]
	if st == nil {
		st = &pending{}
		b.States[m.From.ID] = st
	}
	var err error
	if cb == "" && cmd == "" && strings.HasPrefix(st.Mode, "entry-input-") {
		err = b.entryInput(ctx, m, st)
	} else if cb != "" {
		err = b.callback(ctx, m, cb, st)
	} else {
		err = b.command(ctx, m, cmd, args, st)
	}
	if err != nil {
		return b.send(ctx, m.Chat.ID, "操作失败："+err.Error(), back())
	}
	return nil
}
func (b *Bot) resolve(d Doc, arg string) string {
	if _, ok := findUser(d, arg); ok {
		return arg
	}
	for _, v := range list(d["inbounds"]) {
		in := obj(v)
		if str(in["tag"]) == arg || str(in["listen_port"]) == arg {
			if isUnified(in) {
				return ""
			}
			return str(in["tag"])
		}
	}
	key, _ := resolveUnified(d, arg)
	return key
}
func (b *Bot) usage(ctx context.Context, d Doc, period, target string) (string, error) {
	now := time.Now().In(b.TZ)
	start := Month(now)
	if period == "day" {
		start = Day(now)
	}
	used, err := b.Store.UsageAll(ctx, start)
	if err != nil {
		return "", err
	}
	daily, err := b.Store.UsageAll(ctx, Day(now))
	if err != nil {
		return "", err
	}
	lines := []string{"流量统计（" + now.Format("2006-01-02 15:04") + "）"}
	rows, err := b.reportingInbounds(ctx, d)
	if err != nil {
		return "", err
	}
	for _, v := range rows {
		in := obj(v)
		tag := str(in["tag"])
		if target != "" && tag != target {
			continue
		}
		lim, err := b.Store.Limit(ctx, tag)
		if err != nil {
			return "", err
		}
		quota := Human(lim)
		if lim == 0 {
			quota = "不限"
		}
		lines = append(lines, fmt.Sprintf("%s :%v｜今日 %s｜%s %s / %s", tag, in["listen_port"], Human(daily[tag]), map[bool]string{true: "今日", false: "本月"}[period == "day"], Human(used[tag]), quota))
	}
	return strings.Join(lines, "\n"), nil
}
func gib(s string) (int64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f >= float64(math.MaxInt64)/(1<<30) {
		return 0, errors.New("流量额度无效")
	}
	return int64(f * (1 << 30)), nil
}
func (b *Bot) command(ctx context.Context, m tg.Message, cmd string, args []string, st *pending) error {
	reply := func(s string) error { return b.send(ctx, m.Chat.ID, s, back()) }
	if cmd == "help" {
		return reply("/panel 操作面板\n/users UUID 用户与出站 IP\n/adopthistory 用户标识 旧入口：确认继承历史、额度、到期\n/adduser 统一入口 名称 出站 [UUID]\n/bind 来源 目标：添加转发规则（目标可填 bot）\n/switch [ID] 规则设置\n/cs 分钟 次数：重试冷却与次数\n规则支持实时/轮询、黑白名单、失败关键词、重试、按钮链接、成功通知。\n/today /month /status 流量\n/usage tag\n/alllimit tag [流量MB] [时间day]（默认 512000MB、30天）\n/block tag /unblock tag\n/share tag1 tag2\n/addout 添加出站\n/addvless tag [伪装域名] [有效期]\n/addhy2 tag [有效期]\n/addanytls tag [有效期]\n/add tag 有效期 流量G\n/protocols [页码]\n/setexpiry tag 有效期（0 永久；支持 1y/3m/2w/7d）\n/ufw list|allow|delete [端口] [tcp|udp|both]\n/bwh status|transfer|start|stop|reboot|resetpwd|snap|snap_create|snap_delete|snap_restore|logs\n/cancel 取消")
	}
	if cmd == "bwh" {
		return b.bwhCommand(ctx, m, args, st)
	}
	if cmd == "ufw" {
		return b.ufw(ctx, m, args)
	}
	if cmd == "addout" {
		*st = pending{Mode: "out"}
		if b.Forward != nil {
			b.Forward.Cancel(m.From.ID)
		}
		return reply("请发送 SOCKS/HTTP/AnyTLS/VLESS Reality/Hysteria 2 链接，或 SOCKS5 信息：\nProxy server: <地址>\nport: <端口>\nusername: <用户名>\npassword: <密码>\n/cancel 取消")
	}
	if cmd == "" && st.Mode != "out" {
		return nil
	}
	d, err := Load(b.Config.Path)
	if err != nil {
		return err
	}
	actor := strconv.FormatInt(m.From.ID, 10)
	switch cmd {
	case "adopthistory":
		if len(args) != 2 {
			return reply("用法：/adopthistory <user/统一入口/用户名> <旧入站 tag>；预览并确认继承历史、额度和到期")
		}
		if _, ok := findUser(d, args[0]); !ok {
			return errors.New("统一用户不存在")
		}
		legacy := b.resolve(d, args[1])
		if legacy == "" || strings.HasPrefix(legacy, userPrefix) {
			return errors.New("旧入站无效")
		}
		token, e := randomHex(8)
		if e != nil {
			return e
		}
		*st = pending{Mode: "user-history", User: args[0], Tag: legacy, Token: token, Version: configVersion(d), ChatID: m.Chat.ID, Until: time.Now().Add(5 * time.Minute)}
		return b.send(ctx, m.Chat.ID, "确认将 "+legacy+" 的历史流量、额度、到期和手动暂停关联到 "+args[0]+"？旧记录保留；旧、新客户端将共用额度；已有用户设置时拒绝覆盖。", tg.K([]tg.Button{tg.B("确认继承", "user:history:"+token+":"+ref(args[0])), tg.B("取消", "cancel")}))
	case "users":
		return b.userMenu(ctx, m, d, st)
	case "adduser":
		if len(args) < 3 || len(args) > 4 {
			return reply("用法：/adduser <统一入口 tag> <用户名称> <出站 tag> [UUID]；不填写 UUID 时生成新值")
		}
		version := configVersion(d)
		id := ""
		if len(args) == 4 {
			id = args[3]
		}
		u, e := AddUnifiedUser(d, args[0], args[1], id, args[2])
		if e != nil {
			return e
		}
		if e = b.Config.applyUnifiedVersion(ctx, d, version); e != nil {
			return e
		}
		page := st.Page
		*st = pending{}
		if err := reply("✅ 已添加用户：" + u.Key + "\n使用 /share " + u.Key + " 获取链接；/alllimit、/setexpiry 支持此用户标识"); err != nil {
			return err
		}
		return b.userPage(ctx, m, d, st, page, false)
	case "":
		out, err := ParseOutbound(m.Text)
		if err != nil {
			return err
		}
		tag, err := AddOutbound(d, out)
		if err != nil {
			return err
		}
		if err = b.Config.Apply(ctx, d, nil); err != nil {
			return err
		}
		*st = pending{}
		if err = b.Store.Log(ctx, actor, "add_outbound", tag); err != nil {
			return err
		}
		return reply("✅ 已添加出站：" + tag)
	case "today", "month", "status", "usage":
		period := "month"
		if cmd == "today" {
			period = "day"
		}
		target := ""
		if cmd == "usage" {
			if len(args) != 1 {
				return reply("用法：/usage <tag 或端口>")
			}
			target = b.resolve(d, args[0])
			if target == "" {
				return errors.New("未找到 tag 或端口")
			}
		}
		if target == "" {
			return b.trafficPage(ctx, m, d, st, period, 0, false)
		}
		if u, ok := findUser(d, target); ok {
			text, e := b.userStatus(ctx, d, u)
			if e != nil {
				return e
			}
			return reply(text)
		}
		text, err := b.usage(ctx, d, period, target)
		if err != nil {
			return err
		}
		return reply(text)
	case "setexpiry", "block", "unblock":
		count := 1
		if cmd == "setexpiry" {
			count = 2
		}
		if len(args) != count {
			return reply("用法：/" + cmd + " <tag 或端口>" + map[bool]string{true: " <额度或有效期>", false: ""}[count == 2])
		}
		tag := b.resolve(d, args[0])
		if tag == "" {
			return errors.New("未找到 tag 或端口")
		}
		if _, unified := findUser(d, tag); unified && cmd == "setexpiry" {
			if !b.Config.AutoRestart {
				return errors.New("未启用配置检查与重载，未更新用户策略")
			}
			table := "proto_expiry"
			var value int64
			days, err := ParseDuration(args[1])
			if days > 0 {
				value = time.Now().AddDate(0, 0, days).Unix()
			}
			if err != nil {
				return err
			}
			if err = b.Store.UpdateUserPolicy(ctx, tag, table, value, func() error { return b.enforce(ctx, d) }); err != nil {
				return err
			}
			return reply("✅ 用户策略已更新并检查：" + tag)
		}
		if cmd == "setexpiry" {
			days, err := ParseDuration(args[1])
			if err != nil {
				return err
			}
			if err = b.Store.SetExpiry(ctx, tag, days); err != nil {
				return err
			}
		} else {
			manual, err := b.Store.Manual(ctx)
			if err != nil {
				return err
			}
			old := map[string]bool{}
			for k, v := range manual {
				old[k] = v
			}
			if cmd == "block" {
				manual[tag] = true
			} else {
				delete(manual, tag)
			}
			links, e := b.Store.LegacyLinks(ctx)
			if e != nil {
				return e
			}
			if legacy := links[tag]; legacy != "" {
				if cmd == "block" {
					manual[legacy] = true
				} else {
					delete(manual, legacy)
				}
			}
			if err = b.Store.SetManual(ctx, manual); err != nil {
				return err
			}
			if err = b.enforce(ctx, d); err != nil {
				if rollback := b.Store.SetManual(ctx, old); rollback != nil {
					return fmt.Errorf("%v; 手动状态回滚失败：%v", err, rollback)
				}
				return err
			}
		}
		if err = b.Store.Log(ctx, actor, cmd, strings.Join(args, " ")); err != nil {
			return err
		}
		return reply("✅ 已更新 " + tag + "；超额或到期的入站仍将自动暂停。")
	case "alllimit":
		return b.allLimit(ctx, m, d, args, st)
	case "share":
		if len(args) == 0 {
			return reply("用法：/share <tag1> [tag2 ...]")
		}
		links, err := ShareLinks(d, args, b.Host, b.Config.Domain, b.Config.UnifiedPublicPort)
		if err != nil {
			return err
		}
		if err = b.Store.Log(ctx, actor, "share", strings.Join(args, ",")); err != nil {
			return err
		}
		return b.send(ctx, m.Chat.ID, strings.Join(links, "\n\n"), tg.K([]tg.Button{tg.B("复制说明", "unified:copy"), tg.B("返回用户", "user:list")}))
	case "protocols":
		page := 0
		if len(args) > 0 {
			page = int(integer(args[0])) - 1
		}
		return b.protocols(ctx, m, d, page)
	case "add", "addvless", "addhy2", "addanytls":
		if len(args) == 0 {
			return reply("用法：/" + cmd + " <tag> " + map[string]string{"add": "[有效期] [流量GiB]", "addvless": "[伪装域名] [有效期]", "addhy2": "[有效期]", "addanytls": "[有效期]"}[cmd] + "\n域名：" + b.Config.Domain + "\n证书：" + b.Config.Cert + "\n私钥：" + b.Config.Key)
		}
		kind := map[string]string{"add": "vless", "addvless": "vless", "addhy2": "hysteria2", "addanytls": "anytls"}[cmd]
		sni := ""
		days := 0
		idx := 1
		if cmd == "addvless" {
			idx = 2
			if len(args) > 1 {
				sni = args[1]
			}
		}
		if len(args) > idx {
			days, err = ParseDuration(args[idx])
			if err != nil {
				return err
			}
		}
		var limit *int64
		if cmd == "add" && len(args) > 2 {
			n, e := gib(args[2])
			if e != nil {
				return e
			}
			limit = &n
		}
		_, err = b.Config.AddInbound(ctx, d, kind, args[0], sni)
		if err != nil {
			return err
		}
		if err = b.Store.SetExpiry(ctx, args[0], days); err != nil {
			return err
		}
		if limit != nil {
			if err = b.Store.SetLimit(ctx, args[0], *limit); err != nil {
				return err
			}
		}
		if err = b.Store.Log(ctx, actor, cmd, args[0]); err != nil {
			return err
		}
		links, err := ShareLinks(d, []string{args[0]}, b.Host, b.Config.Domain, b.Config.UnifiedPublicPort)
		if err != nil {
			return err
		}
		return reply("✅ 已添加入站 " + args[0] + "\n" + strings.Join(links, "\n"))
	}
	return reply("未知命令，请发送 /help")
}
func (b *Bot) protocols(ctx context.Context, m tg.Message, d Doc, page int) error {
	ins := accountingInbounds(d)
	page = max(0, min(page, max(0, (len(ins)-1)/8)))
	lines := []string{fmt.Sprintf("入站列表（共 %d 个，第 %d 页）", len(ins), page+1)}
	for _, v := range ins[min(page*8, len(ins)):min((page+1)*8, len(ins))] {
		in := obj(v)
		tag := str(in["tag"])
		expiry, err := b.Store.Expiry(ctx, tag)
		if err != nil {
			return err
		}
		exp := "永久"
		if expiry > 0 {
			exp = time.Unix(expiry, 0).In(b.TZ).Format("2006-01-02 15:04")
		}
		lines = append(lines, fmt.Sprintf("%s｜%v :%v｜到期 %s", tag, in["type"], in["listen_port"], exp))
	}
	k := tg.K()
	nav := []tg.Button{}
	if page > 0 {
		nav = append(nav, tg.B("上一页", fmt.Sprintf("proto:list:%d", page-1)))
	}
	if (page+1)*8 < len(ins) {
		nav = append(nav, tg.B("下一页", fmt.Sprintf("proto:list:%d", page+1)))
	}
	if len(nav) > 0 {
		k.Rows = append(k.Rows, nav)
	}
	k.Rows = append(k.Rows, []tg.Button{tg.B("返回协议管理", "proto:menu")})
	return b.send(ctx, m.Chat.ID, strings.Join(lines, "\n"), k)
}
func (b *Bot) logs(ctx context.Context, m tg.Message) error {
	rows, err := b.Store.DB.QueryContext(ctx, "SELECT ts,actor,action,detail FROM opslog ORDER BY id DESC LIMIT 20")
	if err != nil {
		return err
	}
	defer rows.Close()
	lines := []string{"最近操作日志："}
	for rows.Next() {
		var ts int64
		var actor, action, detail string
		if err = rows.Scan(&ts, &actor, &action, &detail); err != nil {
			return err
		}
		lines = append(lines, time.Unix(ts, 0).In(b.TZ).Format("01-02 15:04")+" ["+actor+"] "+action+" "+detail)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return b.send(ctx, m.Chat.ID, strings.Join(lines, "\n"), back())
}
func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}
