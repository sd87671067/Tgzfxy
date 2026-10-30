package sbbot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	tg "local/bots/internal/telegram"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (b *Bot) bwh(ctx context.Context, actor int64, endpoint string, params map[string]string) (Doc, error) {
	q := url.Values{"veid": {b.BWHVEID}, "api_key": {b.BWHKey}}
	for k, v := range params {
		q.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	d, err := JSONGet(ctx, b.HTTP, b.BWHURL+"/"+endpoint+"?"+q.Encode(), nil)
	status := "OK"
	if err != nil {
		status = "FAIL"
	} else if integer(d["error"]) != 0 {
		status = "FAIL"
		err = errors.New("搬瓦工 API 返回失败：" + str(d["message"]))
	}
	if logErr := b.Store.Log(ctx, strconv.FormatInt(actor, 10), "bwh:"+endpoint, status); logErr != nil && err == nil {
		err = logErr
	}
	return d, err
}
func snapID(name string) string { h := sha256.Sum256([]byte(name)); return fmt.Sprintf("%x", h[:12]) }
func (b *Bot) bwhCommand(ctx context.Context, m tg.Message, args []string, st *pending) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	reply := func(s string) error { return b.send(ctx, m.Chat.ID, s, back()) }
	if sub == "logs" {
		return b.logs(ctx, m)
	}
	if sub == "status" || sub == "transfer" {
		d, err := b.bwh(ctx, m.From.ID, "getServiceInfo", nil)
		if err != nil {
			return err
		}
		used, total := integer(d["data_counter"]), integer(d["plan_monthly_data"])
		reset := time.Unix(integer(d["data_next_reset"]), 0).In(b.TZ)
		remain := max(int64(0), total-used)
		days := max(int64(0), int64(time.Until(reset).Hours()/24))
		text := fmt.Sprintf("📶 本轮流量\n已用：%s\n总量：%s\n剩余：%s\n重置：%s（%d 天）\n计量倍率：×%v\n日均可用：%s", Human(used), Human(total), Human(remain), reset.Format("2006-01-02 15:04"), days, d["monthly_data_multiplier"], Human(remain/max(int64(1), days)))
		if sub == "status" {
			live, err := b.bwh(ctx, m.From.ID, "getLiveServiceInfo", nil)
			if err != nil {
				return err
			}
			text = fmt.Sprintf("🖥 %v（%v）\n系统：%v｜类型：%v\n状态：%v｜停机：%v\n负载：%v\n内存可用：%s\n", d["hostname"], d["node_location"], d["os"], d["vm_type"], live["ve_status"], d["suspended"], live["load_average"], Human(integer(live["mem_available_kb"])*1024)) + text
		}
		return reply(text)
	}
	if sub == "snap" {
		return b.bwhCallback(ctx, m, "snap", st)
	}
	endpoint := map[string]string{"start": "start", "stop": "stop", "reboot": "reboot", "resetpwd": "resetRootPassword", "snap_create": "snapshot/create", "snap_delete": "snapshot/delete", "snap_restore": "snapshot/restore"}[sub]
	if endpoint == "" {
		return reply("用法：/bwh status|transfer|start|stop|reboot|resetpwd|snap|snap_create|snap_delete|snap_restore|logs")
	}
	p := map[string]string{}
	if sub == "snap_create" {
		description := strings.Join(args[1:], " ")
		if description == "" {
			description = "sbbot-" + time.Now().In(b.TZ).Format("20060102-150405")
		}
		p["description"] = description
	}
	if sub == "snap_delete" || sub == "snap_restore" {
		if len(args) != 2 {
			return reply("用法：/bwh " + sub + " <fileName>")
		}
		p["snapshot"] = args[1]
	}
	d, err := b.bwh(ctx, m.From.ID, endpoint, p)
	if err != nil {
		return err
	}
	if sub == "resetpwd" {
		return reply("✅ root 密码已重置\n" + str(d["password"]))
	}
	return reply("✅ 操作请求已提交：" + sub)
}
func (b *Bot) bwhCallback(ctx context.Context, m tg.Message, sub string, st *pending) error {
	reply := func(text string, k *tg.Keyboard) error { return b.send(ctx, m.Chat.ID, text, k) }
	kb := tg.K([]tg.Button{tg.B("返回快照列表", "bwh:snap")})
	if sub == "status" || sub == "transfer" {
		return b.bwhCommand(ctx, m, []string{sub}, st)
	}
	if sub == "power" {
		return reply("电源控制（谨慎操作）", tg.K([]tg.Button{tg.B("重启", "bwh:do:reboot"), tg.B("开机", "bwh:do:start"), tg.B("强制关机", "bwh:do:stop")}, []tg.Button{tg.B("返回面板", "panel")}))
	}
	if strings.HasPrefix(sub, "do:") {
		action := strings.TrimPrefix(sub, "do:")
		if action != "reboot" && action != "start" && action != "stop" {
			return errors.New("未知电源操作")
		}
		return b.bwhCommand(ctx, m, []string{action}, st)
	}
	if sub == "resetpwd" {
		token, err := randomHex(12)
		if err != nil {
			return err
		}
		*st = pending{Mode: "confirm", Action: "resetpwd", Token: token, Until: time.Now().Add(10 * time.Minute)}
		return reply("重置后原 root 密码立即失效。确认重置？", tg.K([]tg.Button{tg.B("确认重置", "bwh:resetpwd:go:"+token), tg.B("取消", "cancel")}))
	}
	if strings.HasPrefix(sub, "resetpwd:go") {
		token := strings.TrimPrefix(sub, "resetpwd:go:")
		if st.Mode != "confirm" || st.Action != "resetpwd" || token != st.Token || time.Now().After(st.Until) {
			return errors.New("确认已失效，请重新操作")
		}
		*st = pending{}
		return b.bwhCommand(ctx, m, []string{"resetpwd"}, st)
	}
	if sub == "auto" {
		return reply("自动备份功能已移除。", kb)
	}
	if sub == "snapnew" {
		return b.bwhCommand(ctx, m, []string{"snap_create"}, st)
	}
	d, err := b.bwh(ctx, m.From.ID, "snapshot/list", nil)
	if err != nil {
		return err
	}
	raw, ok := d["snapshots"].([]any)
	if !ok {
		return errors.New("快照列表格式错误")
	}
	snaps := []Doc{}
	for _, v := range raw {
		s := obj(v)
		if str(s["fileName"]) != "" {
			snaps = append(snaps, s)
		}
	}
	if sub == "snap" || strings.HasPrefix(sub, "snappage:") {
		*st = pending{}
		page := 0
		if sub != "snap" {
			page = int(integer(strings.TrimPrefix(sub, "snappage:")))
		}
		page = max(0, min(page, max(0, (len(snaps)-1)/8)))
		k := tg.K()
		lines := []string{fmt.Sprintf("📸 快照管理（共 %d 个，第 %d 页）", len(snaps), page+1)}
		for i := page * 8; i < min((page+1)*8, len(snaps)); i++ {
			name := str(snaps[i]["fileName"])
			desc := []rune(str(snaps[i]["description"]))
			lines = append(lines, fmt.Sprintf("%d. %s\n%s", i+1, name, string(desc[:min(120, len(desc))])))
			id := snapID(name)
			k.Rows = append(k.Rows, []tg.Button{tg.B(fmt.Sprintf("选择快照 %d", i+1), "bwh:snapview:"+id), tg.B("恢复", "bwh:snaprestore:"+id)})
		}
		nav := []tg.Button{}
		if page > 0 {
			nav = append(nav, tg.B("上一页", fmt.Sprintf("bwh:snappage:%d", page-1)))
		}
		if (page+1)*8 < len(snaps) {
			nav = append(nav, tg.B("下一页", fmt.Sprintf("bwh:snappage:%d", page+1)))
		}
		if len(nav) > 0 {
			k.Rows = append(k.Rows, nav)
		}
		k.Rows = append(k.Rows, []tg.Button{tg.B("创建快照", "bwh:snapnew"), tg.B("刷新列表", "bwh:snap")}, []tg.Button{tg.B("返回面板", "panel")})
		return reply(strings.Join(lines, "\n\n"), k)
	}
	action, token, _ := strings.Cut(sub, ":")
	filename := ""
	if action == "snapdelete" || action == "snapconfirm" {
		expected := "delete"
		if action == "snapconfirm" {
			expected = "restore"
		}
		if st.Mode != "snapshot" || st.Action != expected || st.Token != token || time.Now().After(st.Until) {
			return errors.New("确认已失效，请重新选择快照")
		}
		filename = st.File
		*st = pending{}
	} else if action == "snapview" || action == "snaprestore" {
		for _, s := range snaps {
			if snapID(str(s["fileName"])) == token {
				filename = str(s["fileName"])
				break
			}
		}
	} else {
		return errors.New("旧按钮已失效，请刷新快照列表")
	}
	exists := false
	for _, s := range snaps {
		exists = exists || str(s["fileName"]) == filename
	}
	if filename == "" || !exists {
		return errors.New("快照不存在或按钮已失效")
	}
	if action == "snapdelete" || action == "snapconfirm" {
		endpoint := "snapshot/delete"
		if action == "snapconfirm" {
			endpoint = "snapshot/restore"
		}
		if _, err = b.bwh(ctx, m.From.ID, endpoint, map[string]string{"snapshot": filename}); err != nil {
			return err
		}
		return reply("✅ 请求已提交："+endpoint, kb)
	}
	nonce, err := randomHex(12)
	if err != nil {
		return err
	}
	act, cb, label, text := "delete", "snapdelete", "删除快照", "点击删除将永久删除此快照：\n"
	if action == "snaprestore" {
		act = "restore"
		cb = "snapconfirm"
		label = "确认恢复"
		text = "恢复将覆盖服务器数据并使其停机。确认恢复？\n"
	}
	*st = pending{Mode: "snapshot", Action: act, Token: nonce, File: filename, Until: time.Now().Add(10 * time.Minute)}
	return reply(text+filename, tg.K([]tg.Button{tg.B(label, "bwh:"+cb+":"+nonce)}, []tg.Button{tg.B("取消", "bwh:snap")}))
}
