package sbbot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const UnifiedPrefix = "sbbot-unified-"
const userPrefix = "user/"
const userOutboundPrefix = "sbbot-user-"

func userOutbound(key string) string      { return userOutboundPrefix + ref(key) }
func managedUserOutbound(tag string) bool { return strings.HasPrefix(tag, userOutboundPrefix) }

// The config is the durable source of truth for bindings. No second SQLite copy
// can disagree with the live config following a crash or rollback.
type UnifiedUser struct{ Key, Inbound, Name, UUID, Outbound string }

func userKey(in, name string) string { return userPrefix + in + "/" + name }
func isUnified(in Doc) bool          { return strings.HasPrefix(str(in["tag"]), UnifiedPrefix) }
func UnifiedUsers(d Doc) []UnifiedUser {
	var users []UnifiedUser
	for _, raw := range list(d["inbounds"]) {
		in := obj(raw)
		if !isUnified(in) {
			continue
		}
		for _, raw := range list(in["users"]) {
			u := obj(raw)
			x := UnifiedUser{Key: userKey(str(in["tag"]), str(u["name"])), Inbound: str(in["tag"]), Name: str(u["name"]), UUID: str(u["uuid"])}
			for _, raw := range list(obj(d["route"])["rules"]) {
				r := obj(raw)
				if str(r["action"]) == "route" && str(r["outbound"]) != "" && slices.Contains(stringsOf(r["inbound"]), x.Inbound) && slices.Contains(stringsOf(r["auth_user"]), x.Name) {
					x.Outbound = str(r["outbound"])
					if x.Outbound == userOutbound(x.Key) {
						for _, raw := range list(d["outbounds"]) {
							o := obj(raw)
							if str(o["tag"]) == x.Outbound && str(o["type"]) == "selector" && len(stringsOf(o["outbounds"])) == 1 {
								x.Outbound = stringsOf(o["outbounds"])[0]
								break
							}
						}
					}
					break
				}
			}
			users = append(users, x)
		}
	}
	return users
}
func findUser(d Doc, key string) (UnifiedUser, bool) {
	for _, u := range UnifiedUsers(d) {
		if u.Key == key {
			return u, true
		}
	}
	return UnifiedUser{}, false
}
func accountingInbounds(d Doc) []any {
	var out []any
	for _, raw := range list(d["inbounds"]) {
		if !isUnified(obj(raw)) {
			out = append(out, raw)
		}
	}
	for _, u := range UnifiedUsers(d) {
		var port any
		for _, raw := range list(d["inbounds"]) {
			if str(obj(raw)["tag"]) == u.Inbound {
				port = obj(raw)["listen_port"]
			}
		}
		out = append(out, Doc{"tag": u.Key, "listen_port": port, "type": "vless"})
	}
	return out
}
func ConnectionTag(meta Doc, d Doc) string {
	tag := str(meta["inbound"])
	if tag == "" {
		_, tag, _ = strings.Cut(str(meta["type"]), "/")
	}
	for _, raw := range list(d["inbounds"]) {
		in := obj(raw)
		if str(in["tag"]) != tag {
			continue
		}
		if !isUnified(in) {
			return tag
		}
		name := str(meta["user"])
		if name == "" {
			name = str(meta["auth_user"])
		}
		key := userKey(tag, name)
		if _, ok := findUser(d, key); ok {
			return key
		}
		return ""
	}
	return ""
}

// sing-box 1.13 omits the username in metadata, but exposes the matched rule.
// Match the entire validated per-user direct rule, never a shared exit chain.
// Older configurations still use dedicated selector chains.
func ConnectionAccounting(c Doc, d Doc) string {
	meta := obj(c["metadata"])
	key := ConnectionTag(meta, d)
	if key != "" {
		return key
	}
	inbound := str(meta["inbound"])
	if inbound == "" {
		_, inbound, _ = strings.Cut(str(meta["type"]), "/")
	}
	for _, u := range UnifiedUsers(d) {
		if u.Inbound == inbound && u.Outbound == "direct" &&
			str(c["rule"]) == "inbound="+u.Inbound+" auth_user="+u.Name+" => route(direct)" &&
			slices.Contains(stringsOf(c["chains"]), "direct") {
			return u.Key
		}
		if u.Inbound == inbound && slices.Contains(stringsOf(c["chains"]), userOutbound(u.Key)) {
			return u.Key
		}
	}
	return ""
}
func ruleForUser(r Doc, u UnifiedUser) bool {
	return slices.Equal(stringsOf(r["inbound"]), []string{u.Inbound}) && slices.Equal(stringsOf(r["auth_user"]), []string{u.Name})
}
func BindUser(d Doc, key, out string) error {
	u, ok := findUser(d, key)
	if !ok {
		return errors.New("用户不存在")
	}
	if Kind(d, out) != "outbound" || out == ManagedBlock || managedUserOutbound(out) {
		return errors.New("出站不存在或不可绑定")
	}
	alias := userOutbound(key)
	var outs []any
	for _, raw := range list(d["outbounds"]) {
		if str(obj(raw)["tag"]) != alias {
			outs = append(outs, raw)
		}
	}
	if out == "direct" {
		alias = "direct"
		d["outbounds"] = outs
	} else {
		d["outbounds"] = append(outs, Doc{"type": "selector", "tag": alias, "outbounds": []string{out}, "default": out, "interrupt_exist_connections": true})
	}
	route := nested(d, "route")
	rules := list(route["rules"])
	for _, raw := range rules {
		r := obj(raw)
		if !ruleForUser(r, u) || str(r["action"]) != "route" {
			continue
		}
		simple := true
		for k := range r {
			if !slices.Contains([]string{"inbound", "auth_user", "action", "outbound"}, k) {
				simple = false
			}
		}
		if simple {
			r["outbound"] = alias
			return nil
		}
	}
	pos := 0
	for pos < len(rules) && (str(obj(rules[pos])["action"]) == "reject" && len(stringsOf(obj(rules[pos])["auth_user"])) > 0 || str(obj(rules[pos])["outbound"]) == ManagedBlock) {
		pos++
	}
	route["rules"] = slices.Insert(rules, pos, any(Doc{"inbound": []string{u.Inbound}, "auth_user": []string{u.Name}, "action": "route", "outbound": alias}))
	return nil
}

// NormalizeDirectUsers only removes the managed, one-target direct aliases.
// Rebinding each user separately preserves identity, pauses and route ownership.
func NormalizeDirectUsers(d Doc) error {
	// Existing aliases are valid sing-box configuration. Migrate only the target
	// during an explicit BindUser; saving another entity must leave routes intact.
	return nil
}

func validName(s string) bool { return regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`).MatchString(s) }
func canonicalUUID(s string) (string, error) {
	v := strings.ToLower(strings.ReplaceAll(s, "-", ""))
	if len(v) != 32 {
		return "", errors.New("UUID 无效")
	}
	if _, err := hex.DecodeString(v); err != nil {
		return "", errors.New("UUID 无效")
	}
	return v[:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:], nil
}
func NewUUID() (string, error) {
	h, e := randomHex(16)
	if e != nil {
		return "", e
	}
	return canonicalUUID(h[:12] + "4" + h[13:16] + "8" + h[17:])
}
func AddUnifiedUser(d Doc, inbound, name, id, out string) (UnifiedUser, error) {
	if !validName(name) {
		return UnifiedUser{}, errors.New("名称仅支持 1-48 位英文数字、下划线和连字符")
	}
	var in Doc
	for _, raw := range list(d["inbounds"]) {
		if str(obj(raw)["tag"]) == inbound {
			in = obj(raw)
		}
	}
	if in == nil || !isUnified(in) || str(in["type"]) != "vless" {
		return UnifiedUser{}, errors.New("请选择已预置的统一 VLESS 入站")
	}
	if Kind(d, out) != "outbound" || out == ManagedBlock || managedUserOutbound(out) {
		return UnifiedUser{}, errors.New("出站不可用")
	}
	var err error
	if id == "" {
		id, err = NewUUID()
	} else {
		id, err = canonicalUUID(id)
	}
	if err != nil {
		return UnifiedUser{}, err
	}
	for _, raw := range list(in["users"]) {
		u := obj(raw)
		normalized, _ := canonicalUUID(str(u["uuid"]))
		if str(u["name"]) == name || normalized == id {
			return UnifiedUser{}, errors.New("同一入站名称或 UUID 重复")
		}
	}
	in["users"] = append(list(in["users"]), Doc{"name": name, "uuid": id, "flow": "xtls-rprx-vision"})
	key := userKey(inbound, name)
	if err = BindUser(d, key, out); err != nil {
		return UnifiedUser{}, err
	}
	u, _ := findUser(d, key)
	return u, nil
}

// StageUnified appends a new listener to a COPY. Legacy listeners, keys, routes,
// UUIDs, traffic and SQLite records are deliberately never rewritten.
func StageUnified(d Doc, template, tag, listen string, port int) (Doc, error) {
	if !strings.HasPrefix(tag, UnifiedPrefix) || !validName(tag) || Kind(d, tag) != "" || port < 1 || port > 65535 {
		return nil, errors.New("统一 tag/端口无效或重复")
	}
	for _, raw := range list(d["inbounds"]) {
		if int(integer(obj(raw)["listen_port"])) == port {
			return nil, errors.New("端口已在配置中使用")
		}
	}
	out := clone(d)
	var in Doc
	for _, raw := range list(out["inbounds"]) {
		if str(obj(raw)["tag"]) == template {
			in = clone(obj(raw))
		}
	}
	if in == nil || str(in["type"]) != "vless" || obj(obj(in["tls"])["reality"])["enabled"] != true {
		return nil, errors.New("模板必须为 VLESS REALITY")
	}
	in["tag"] = tag
	in["listen"] = listen
	in["listen_port"] = port
	in["users"] = []any{}
	out["inbounds"] = append(list(out["inbounds"]), in)
	route := nested(out, "route")
	route["rules"] = append([]any{Doc{"inbound": []string{tag}, "action": "reject"}}, list(route["rules"])...)
	return out, nil
}
func ValidateUnified(d Doc) error {
	for _, raw := range list(d["inbounds"]) {
		in := obj(raw)
		if !isUnified(in) {
			continue
		}
		if str(in["type"]) != "vless" || obj(obj(in["tls"])["reality"])["enabled"] != true {
			return errors.New("统一入口必须为 REALITY VLESS")
		}
		names, ids := map[string]bool{}, map[string]bool{}
		for _, raw := range list(in["users"]) {
			u := obj(raw)
			name := str(u["name"])
			id, e := canonicalUUID(str(u["uuid"]))
			if e != nil || !validName(name) || names[name] || ids[id] || str(u["flow"]) != "xtls-rprx-vision" {
				return errors.New("统一用户名称、UUID 或 Vision 无效/重复")
			}
			names[name] = true
			ids[id] = true
		}
		for _, u := range UnifiedUsers(d) {
			if u.Inbound != str(in["tag"]) {
				continue
			}
			if u.Outbound != "" && (Kind(d, u.Outbound) != "outbound" || managedUserOutbound(u.Outbound)) {
				return errors.New("统一用户 selector 出站无效")
			}
		}
		guard := false
		routed := map[string]bool{}
		for _, raw := range list(obj(d["route"])["rules"]) {
			r := obj(raw)
			ins := stringsOf(r["inbound"])
			if len(ins) == 0 {
				if !guard {
					return errors.New("统一入口兜底前存在全局/逻辑路由，需人工审阅")
				}
				continue
			}
			if !slices.Contains(ins, str(in["tag"])) {
				continue
			}
			if guard {
				return errors.New("统一入口兜底后存在不可达规则")
			}
			if str(r["action"]) == "reject" && len(r) == 2 && len(ins) == 1 {
				guard = true
				continue
			}
			namesInRule := stringsOf(r["auth_user"])
			if len(namesInRule) != 1 || len(ins) != 1 || !names[namesInRule[0]] {
				return errors.New("统一入口存在非逐用户路由")
			}
			name := namesInRule[0]
			if str(r["action"]) == "reject" && len(r) == 3 {
				if routed[name] {
					return errors.New("封禁规则必须在用户路由之前")
				}
				continue
			}
			key := userKey(str(in["tag"]), name)
			target := str(r["outbound"])
			if str(r["action"]) != "route" || len(r) != 4 || (target != userOutbound(key) && target != "direct") || routed[name] {
				return errors.New("用户必须通过逐用户 direct 或专属 selector 路由")
			}
			if target == "direct" {
				directOK := false
				for _, raw := range list(d["outbounds"]) {
					o := obj(raw)
					if str(o["tag"]) == "direct" && str(o["type"]) == "direct" {
						directOK = true
					}
				}
				if !directOK {
					return errors.New("direct 出站无效")
				}
				routed[name] = true
				continue
			}
			aliasOK := false
			for _, raw := range list(d["outbounds"]) {
				o := obj(raw)
				if str(o["tag"]) != userOutbound(key) {
					continue
				}
				targets := stringsOf(o["outbounds"])
				if str(o["type"]) == "selector" && len(targets) == 1 && !managedUserOutbound(targets[0]) && Kind(d, targets[0]) == "outbound" && targets[0] != ManagedBlock && str(o["default"]) == targets[0] {
					aliasOK = true
				}
			}
			if !aliasOK {
				return errors.New("专属 selector 缺失或目标无效")
			}
			routed[name] = true
		}
		if !guard {
			return errors.New("统一入口缺少兜底拒绝规则")
		}
	}
	return nil
}
func configVersion(d Doc) string {
	raw, _ := json.Marshal(d)
	h := sha256.Sum256(raw)
	return fmt.Sprintf("%x", h)
}
func (c *ConfigManager) ApplyUnified(ctx context.Context, d Doc) error {
	return c.applyUnifiedVersion(ctx, d, "")
}
func (c *ConfigManager) applyUnifiedVersion(ctx context.Context, d Doc, expected string) error {
	return c.applyUnifiedVerified(ctx, d, expected, nil)
}
func (c *ConfigManager) applyUnifiedVerified(ctx context.Context, d Doc, expected string, verify func() error) error {
	if c.ControlSocket != "" {
		if verify != nil {
			return errors.New("控制模式需要事务内验证，未写入")
		}
		return c.bridgeApply(ctx, d, expected, "", "", "", "")
	}
	if !c.AutoRestart || c.Run == nil {
		return errors.New("统一用户变更需要启用配置检查与 sing-box 重载；未写入配置")
	}
	baseline, err := os.ReadFile(c.Path)
	if err != nil {
		return err
	}
	if expected != "" {
		var active Doc
		if err = json.Unmarshal(baseline, &active); err != nil {
			return err
		}
		if configVersion(active) != expected {
			return errors.New("配置已被外部修改，未写入")
		}
	}
	if err := NormalizeDirectUsers(d); err != nil {
		return err
	}
	if err := ValidateUnified(d); err != nil {
		return err
	}
	// Check a sibling candidate before touching the active config. HostRun sees
	// /config through the host's /etc/sing-box bind mount.
	f, err := os.CreateTemp(filepath.Dir(c.Path), ".sbbot-candidate-*.json")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	raw, err := json.Marshal(d)
	if err == nil {
		_, err = f.Write(raw)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	checkPath := name
	if filepath.Dir(c.Path) == "/config" {
		checkPath = filepath.Join("/etc/sing-box", filepath.Base(name))
	}
	if _, err = c.Run(ctx, "/usr/local/bin/sing-box", "check", "-c", checkPath); err != nil {
		return fmt.Errorf("候选配置检查失败，未修改运行配置：%w", err)
	}
	current, err := os.ReadFile(c.Path)
	if err != nil {
		return err
	}
	if string(current) != string(baseline) {
		return errors.New("预检期间配置被外部修改，未写入")
	}
	return c.Apply(ctx, d, func() error {
		_, err := c.Run(ctx, "/usr/bin/systemctl", "is-active", "--quiet", "sing-box")
		if err != nil {
			return err
		}
		if verify != nil {
			return verify()
		}
		return nil
	})
}
