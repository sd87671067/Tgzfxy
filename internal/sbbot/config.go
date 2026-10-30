package sbbot

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Doc map[string]any

const ManagedBlock = "__sbbot_monthly_limit_block__"
const ManualBlock = "__manual_block__"

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func integer(v any) int64 { n, _ := strconv.ParseInt(str(v), 10, 64); return n }
func obj(v any) Doc {
	switch x := v.(type) {
	case Doc:
		return x
	case map[string]any:
		return Doc(x)
	}
	return Doc{}
}
func list(v any) []any {
	if x, ok := v.([]any); ok {
		return x
	}
	return nil
}
func stringsOf(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, v := range x {
			out = append(out, str(v))
		}
	case []string:
		return x
	case string:
		return []string{x}
	}
	return out
}
func nested(d Doc, key string) Doc { v := obj(d[key]); d[key] = v; return v }
func clone(d Doc) Doc              { raw, _ := json.Marshal(d); var out Doc; json.Unmarshal(raw, &out); return out }
func Load(path string) (Doc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Doc
	err = json.Unmarshal(raw, &d)
	return d, err
}
func Kind(d Doc, tag string) string {
	for _, kind := range []string{"inbound", "outbound"} {
		for _, v := range list(d[kind+"s"]) {
			if str(obj(v)["tag"]) == tag {
				return kind
			}
		}
	}
	return ""
}
func Bind(d Doc, first, second string) error {
	a, b := Kind(d, first), Kind(d, second)
	if a == "outbound" && b == "inbound" {
		first, second = second, first
		a, b = b, a
	}
	if a != "inbound" || b != "outbound" {
		return errors.New("必须选择一个入站 tag 和一个出站 tag")
	}
	for _, raw := range list(d["inbounds"]) {
		if str(obj(raw)["tag"]) == first && isUnified(obj(raw)) {
			return errors.New("统一入口请使用 UUID 用户绑定")
		}
	}
	route := nested(d, "route")
	var blocks, remaining []any
	for _, raw := range list(route["rules"]) {
		r := obj(raw)
		if str(r["outbound"]) == ManagedBlock {
			blocks = append(blocks, r)
			continue
		}
		in := stringsOf(r["inbound"])
		if slices.Contains(in, first) {
			in = slices.DeleteFunc(in, func(s string) bool { return s == first })
			if len(in) == 0 {
				continue
			}
			r["inbound"] = in
		}
		remaining = append(remaining, r)
	}
	rules := append(blocks, Doc{"inbound": []string{first}, "outbound": second})
	route["rules"] = append(rules, remaining...)
	return nil
}
func Delete(d Doc, kind, tag string) (Doc, error) {
	if kind != "inbound" && kind != "outbound" {
		return nil, errors.New("类型必须是 inbound 或 outbound")
	}
	if kind == "outbound" && (tag == "direct" || tag == ManagedBlock || managedUserOutbound(tag)) {
		return nil, errors.New("系统出站 direct/block 不允许删除")
	}
	if kind == "outbound" {
		for _, u := range UnifiedUsers(d) {
			if u.Outbound == tag {
				return nil, errors.New("出站仍被统一用户引用，请先重新绑定")
			}
		}
	}
	if kind == "inbound" {
		for _, u := range UnifiedUsers(d) {
			if u.Inbound == tag {
				return nil, errors.New("统一入口仍有用户，禁止整入口删除")
			}
		}
	}
	var target Doc
	var kept []any
	for _, v := range list(d[kind+"s"]) {
		if str(obj(v)["tag"]) == tag {
			target = obj(v)
		} else {
			kept = append(kept, v)
		}
	}
	if target == nil {
		return nil, errors.New("tag 不存在")
	}
	d[kind+"s"] = kept
	route := nested(d, "route")
	var rules []any
	for _, v := range list(route["rules"]) {
		r := obj(v)
		if kind == "inbound" {
			in := stringsOf(r["inbound"])
			if slices.Contains(in, tag) {
				in = slices.DeleteFunc(in, func(s string) bool { return s == tag })
				if len(in) == 0 {
					continue
				}
				r["inbound"] = in
			}
		} else if str(r["outbound"]) == tag {
			r["outbound"] = "direct"
		}
		rules = append(rules, r)
	}
	route["rules"] = rules
	return target, nil
}
func Blocked(d Doc) map[string]bool {
	out := map[string]bool{}
	for _, v := range list(obj(d["route"])["rules"]) {
		r := obj(v)
		if str(r["action"]) == "reject" {
			for _, u := range UnifiedUsers(d) {
				if ruleForUser(r, u) {
					out[u.Key] = true
				}
			}
		}
		if str(r["outbound"]) == ManagedBlock {
			for _, t := range stringsOf(r["inbound"]) {
				out[t] = true
			}
		}
	}
	return out
}
func SetBlockedDoc(d Doc, tags map[string]bool) {
	route := nested(d, "route")
	var rules []any
	var legacy []string
	keys := []string{}
	for key, blocked := range tags {
		if blocked {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		if u, ok := findUser(d, key); ok {
			rules = append(rules, Doc{"inbound": []string{u.Inbound}, "auth_user": []string{u.Name}, "action": "reject"})
		} else if !strings.HasPrefix(key, userPrefix) {
			legacy = append(legacy, key)
		}
	}
	if len(legacy) > 0 {
		if Kind(d, ManagedBlock) == "" {
			d["outbounds"] = append(list(d["outbounds"]), Doc{"type": "block", "tag": ManagedBlock})
		}
		rules = append(rules, Doc{"inbound": legacy, "outbound": ManagedBlock})
	}
	for _, raw := range list(route["rules"]) {
		r := obj(raw)
		if str(r["outbound"]) == ManagedBlock {
			continue
		}
		pause := false
		if str(r["action"]) == "reject" {
			for _, u := range UnifiedUsers(d) {
				if ruleForUser(r, u) {
					pause = true
				}
			}
		}
		if !pause {
			rules = append(rules, raw)
		}
	}
	route["rules"] = rules
}
func ParseDuration(text string) (int, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	if slices.Contains([]string{"", "0", "permanent", "none", "never", "forever", "永久", "无期限"}, text) {
		return 0, nil
	}
	re := regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(y|m|d|w)`)
	total := 0.0
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.ParseFloat(m[1], 64)
		total += n * map[string]float64{"y": 365, "m": 30, "w": 7, "d": 1}[m[2]]
	}
	if total > 0 {
		return int(total), nil
	}
	n, e := strconv.ParseFloat(text, 64)
	if e != nil {
		return 0, errors.New("无法解析有效期，请使用数字天数或 1y / 3m / 2w / 7d")
	}
	return max(0, int(n)), nil
}
func ParseOutbound(text string) (Doc, error) {
	text = strings.TrimSpace(text)
	if !strings.Contains(text, "://") {
		aliases := map[string]string{"proxy server": "server", "server": "server", "host": "server", "address": "server", "port": "server_port", "username": "username", "user": "username", "password": "password", "pass": "password"}
		d := Doc{"type": "socks"}
		for _, line := range strings.Split(text, "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			if key := aliases[strings.ToLower(strings.TrimSpace(k))]; key != "" {
				d[key] = strings.TrimSpace(v)
			}
		}
		for _, key := range []string{"server", "server_port", "username", "password"} {
			if str(d[key]) == "" {
				return nil, fmt.Errorf("缺少字段: %s", key)
			}
		}
		port, e := strconv.Atoi(str(d["server_port"]))
		if e != nil || port < 1 || port > 65535 {
			return nil, errors.New("port 必须在 1-65535")
		}
		d["server_port"] = port
		return d, nil
	}
	if strings.ContainsAny(text, " \t\r\n") {
		return nil, errors.New("请一次发送一个链接，空格请使用 URL 编码")
	}
	u, e := url.Parse(text)
	if e != nil {
		return nil, errors.New("链接格式无效")
	}
	kind := map[string]string{"hy2": "hysteria2", "hysteria2": "hysteria2", "anytls": "anytls", "vless": "vless", "socks": "socks", "socks5": "socks", "http": "http", "https": "http"}[strings.ToLower(u.Scheme)]
	if kind == "" {
		return nil, errors.New("支持 SOCKS/HTTP、AnyTLS、VLESS Reality、Hysteria 2 链接")
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if u.Port() != "" {
		port, e = strconv.Atoi(u.Port())
		if e != nil {
			return nil, errors.New("端口无效")
		}
	} else if kind == "socks" {
		return nil, errors.New("SOCKS5 链接必须指定端口")
	}
	if u.Hostname() == "" || port < 1 || port > 65535 {
		return nil, errors.New("缺少服务器地址或端口无效")
	}
	d := Doc{"type": kind, "server": u.Hostname(), "server_port": port}
	q := u.Query()
	if kind == "socks" || kind == "http" {
		if u.Scheme == "https" {
			d["tls"] = Doc{"enabled": true, "server_name": u.Hostname()}
		}
		if u.User != nil {
			d["username"] = u.User.Username()
			p, _ := u.User.Password()
			d["password"] = p
		}
		return d, nil
	}
	if u.User == nil || u.User.String() == "" {
		return nil, errors.New("链接缺少密码或 UUID")
	}
	auth, _ := url.PathUnescape(u.User.String())
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("peer")
	}
	if sni == "" {
		sni = q.Get("serverName")
	}
	if sni == "" {
		sni = u.Hostname()
	}
	insecure := strings.ToLower(q.Get("insecure"))
	if _, ok := q["insecure"]; !ok {
		insecure = strings.ToLower(q.Get("allowInsecure"))
	}
	if insecure == "" {
		insecure = "0"
	}
	if !slices.Contains([]string{"0", "1", "true", "false"}, insecure) {
		return nil, errors.New("insecure 必须为 0/1/true/false")
	}
	tls := Doc{"enabled": true, "server_name": sni, "insecure": insecure == "1" || insecure == "true"}
	if q.Get("alpn") != "" {
		tls["alpn"] = strings.Split(q.Get("alpn"), ",")
	}
	d["tls"] = tls
	if kind == "vless" {
		uuid := strings.ToLower(strings.ReplaceAll(strings.Trim(auth, "{}"), "-", ""))
		if len(uuid) != 32 {
			return nil, errors.New("VLESS UUID 无效")
		}
		if _, e = hex.DecodeString(uuid); e != nil {
			return nil, errors.New("VLESS UUID 无效")
		}
		d["uuid"] = uuid[:8] + "-" + uuid[8:12] + "-" + uuid[12:16] + "-" + uuid[16:20] + "-" + uuid[20:]
		if q.Get("security") != "reality" {
			return nil, errors.New("VLESS 目前支持 security=reality")
		}
		if q.Get("type") != "" && q.Get("type") != "tcp" && q.Get("type") != "raw" {
			return nil, errors.New("VLESS Reality 目前支持 TCP/raw 传输")
		}
		pbk := q.Get("pbk")
		key, e := base64.RawURLEncoding.DecodeString(pbk)
		if e != nil || len(key) != 32 || len(pbk) != 43 {
			return nil, errors.New("Reality 缺少有效 pbk 公钥")
		}
		sid := q.Get("sid")
		if !regexp.MustCompile(`^(?:[0-9a-fA-F]{2}){0,8}$`).MatchString(sid) {
			return nil, errors.New("Reality sid 必须为至多 16 位、偶数长度的十六进制")
		}
		flow := q.Get("flow")
		if flow != "" && flow != "xtls-rprx-vision" {
			return nil, errors.New("不支持的 VLESS flow")
		}
		if flow != "" {
			d["flow"] = flow
		}
		fp := q.Get("fp")
		if fp == "" {
			fp = "chrome"
		}
		tls["insecure"] = false
		tls["utls"] = Doc{"enabled": true, "fingerprint": fp}
		tls["reality"] = Doc{"enabled": true, "public_key": pbk, "short_id": sid}
	} else {
		d["password"] = auth
		if kind == "hysteria2" && q.Get("obfs") != "" {
			if q.Get("obfs") != "salamander" || q.Get("obfs-password") == "" {
				return nil, errors.New("Hysteria 2 混淆需 obfs=salamander 和 obfs-password")
			}
			d["obfs"] = Doc{"type": "salamander", "password": q.Get("obfs-password")}
		}
		if kind == "hysteria2" && q.Get("mport") != "" {
			ports := strings.Split(strings.ReplaceAll(q.Get("mport"), "-", ":"), ",")
			for _, p := range ports {
				bounds := strings.Split(p, ":")
				if len(bounds) > 2 {
					return nil, errors.New("mport 端口范围无效")
				}
				last := 0
				for _, v := range bounds {
					n, e := strconv.Atoi(v)
					if e != nil || n < 1 || n > 65535 || n < last {
						return nil, errors.New("mport 端口范围无效")
					}
					last = n
				}
			}
			delete(d, "server_port")
			d["server_ports"] = ports
		}
	}
	return d, nil
}
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
func AddOutbound(d Doc, values Doc) (string, error) {
	for i := 0; i < 20; i++ {
		suffix, e := randomHex(4)
		if e != nil {
			return "", e
		}
		tag := str(values["type"]) + "-" + suffix
		if Kind(d, tag) == "" {
			v := clone(values)
			v["tag"] = tag
			d["outbounds"] = append(list(d["outbounds"]), v)
			return tag, nil
		}
	}
	return "", errors.New("无法生成唯一出站 tag")
}
func PublicKey(private string) (string, error) {
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimRight(private, "="))
	if e != nil {
		return "", e
	}
	k, e := ecdh.X25519().NewPrivateKey(b)
	if e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}
func ShareLinks(d Doc, tags []string, host, domain string) ([]string, error) {
	var links []string
	var expanded []string
	for _, tag := range tags {
		found := false
		for _, u := range UnifiedUsers(d) {
			if u.Inbound == tag {
				expanded = append(expanded, u.Key)
				found = true
			}
		}
		if !found {
			expanded = append(expanded, tag)
		}
	}
	for _, tag := range expanded {
		actual := tag
		selected, scoped := findUser(d, tag)
		if scoped {
			actual = selected.Inbound
		}
		var in Doc
		for _, v := range list(d["inbounds"]) {
			if str(obj(v)["tag"]) == actual {
				in = obj(v)
				break
			}
		}
		if in == nil || len(list(in["users"])) == 0 {
			return nil, fmt.Errorf("%s 无效或无用户", tag)
		}
		user := obj(list(in["users"])[0])
		if scoped {
			for _, raw := range list(in["users"]) {
				if str(obj(raw)["name"]) == selected.Name {
					user = obj(raw)
					break
				}
			}
		}
		tls := obj(in["tls"])
		kind := str(in["type"])
		sni := str(tls["server_name"])
		if sni == "" {
			sni = domain
		}
		params := url.Values{}
		auth := ""
		path := ""
		if kind == "anytls" || kind == "hysteria2" {
			auth = str(user["password"])
			if auth == "" {
				return nil, errors.New("无密码")
			}
			params.Set("sni", sni)
			params.Set("insecure", "0")
			path = "/"
		} else if kind == "vless" {
			auth = str(user["uuid"])
			params.Set("encryption", "none")
			if str(user["flow"]) != "" {
				params.Set("flow", str(user["flow"]))
			}
			params.Set("type", "tcp")
			params.Set("headerType", "none")
			if tls["enabled"] == true {
				params.Set("security", "tls")
				params.Set("sni", sni)
				params.Set("fp", "chrome")
				r := obj(tls["reality"])
				if r["enabled"] == true {
					params.Set("security", "reality")
					if sni == "" {
						params.Set("sni", str(obj(r["handshake"])["server"]))
					}
					pk, e := PublicKey(str(r["private_key"]))
					if e != nil {
						return nil, e
					}
					params.Set("pbk", pk)
					ids := stringsOf(r["short_id"])
					if len(ids) > 0 {
						params.Set("sid", ids[0])
					}
				}
			}
		} else {
			return nil, fmt.Errorf("%s 类型暂不支持", tag)
		}
		address := net.JoinHostPort(host, str(in["listen_port"]))
		u := url.URL{Scheme: kind, User: url.User(auth), Host: address, Path: path, RawQuery: params.Encode(), Fragment: tag}
		links = append(links, u.String())
	}
	return links, nil
}

type ConfigManager struct {
	Path, Domain, Cert, Key, SNI string
	AutoRestart                  bool
	Run                          func(context.Context, string, ...string) (string, error)
}

func HostRun(ctx context.Context, program string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	all := append([]string{"-t", "1", "-m", "-p", "--", program}, args...)
	out, err := exec.CommandContext(ctx, "nsenter", all...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("host command %s failed: %w", filepath.Base(program), err)
	}
	return strings.TrimSpace(string(out)), nil
}
func (c *ConfigManager) restart(ctx context.Context) error {
	if !c.AutoRestart {
		return nil
	}
	if _, err := c.Run(ctx, "/usr/local/bin/sing-box", "check", "-c", "/etc/sing-box/config.json"); err != nil {
		return err
	}
	_, err := c.Run(ctx, "/usr/bin/systemctl", "restart", "sing-box")
	return err
}
func (c *ConfigManager) write(raw []byte) error {
	f, e := os.CreateTemp(filepath.Dir(c.Path), ".sbbot-go-*.json")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0o600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(raw); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(name, c.Path)
}
func (c *ConfigManager) Apply(ctx context.Context, d Doc, after func() error) error {
	old, err := os.ReadFile(c.Path)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	backup := c.Path + ".sbbot-go-backup-" + time.Now().UTC().Format("20060102T150405.000000000")
	if err = os.WriteFile(backup, old, 0o600); err != nil {
		return err
	}
	if err = c.write(append(raw, '\n')); err != nil {
		return err
	}
	err = c.restart(ctx)
	if err == nil && after != nil {
		err = after()
	}
	if err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if e := c.write(old); e != nil {
			return fmt.Errorf("apply failed: %v; rollback write failed: %v", err, e)
		}
		if e := c.restart(rollbackCtx); e != nil {
			return fmt.Errorf("apply failed: %v; rollback restart failed: %v", err, e)
		}
		return fmt.Errorf("配置失败，已回滚：%w", err)
	}
	return nil
}
func RandomPort(d Doc) (int, error) {
	used := map[int]bool{}
	for _, v := range list(d["inbounds"]) {
		used[int(integer(obj(v)["listen_port"]))] = true
	}
	for i := 0; i < 256; i++ {
		n, e := rand.Int(rand.Reader, big.NewInt(52001))
		if e != nil {
			return 0, e
		}
		port := 10000 + int(n.Int64())
		if used[port] {
			continue
		}
		addr := fmt.Sprintf("[::]:%d", port)
		tcp, e := net.Listen("tcp6", addr)
		if e != nil {
			continue
		}
		udp, e := net.ListenPacket("udp6", addr)
		tcp.Close()
		if e != nil {
			continue
		}
		udp.Close()
		return port, nil
	}
	return 0, errors.New("无法找到空闲随机端口")
}
func (c *ConfigManager) AddInbound(ctx context.Context, d Doc, kind, tag, sni string) (Doc, error) {
	if strings.HasPrefix(tag, UnifiedPrefix) {
		return nil, errors.New("统一入口名称保留，请使用离线规划工具预置带默认拒绝规则的入口")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(tag) {
		return nil, errors.New("tag 仅支持 1-64 位英文、数字、下划线和连字符")
	}
	if Kind(d, tag) != "" {
		return nil, errors.New("tag 已存在")
	}
	port, e := RandomPort(d)
	if e != nil {
		return nil, e
	}
	in := Doc{"type": kind, "tag": tag, "listen": "::", "listen_port": port}
	tls := Doc{"enabled": true}
	in["tls"] = tls
	if kind == "vless" {
		key, e := ecdh.X25519().GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		raw := make([]byte, 16)
		if _, e = rand.Read(raw); e != nil {
			return nil, e
		}
		raw[6] = (raw[6] & 0x0f) | 0x40
		raw[8] = (raw[8] & 0x3f) | 0x80
		x := hex.EncodeToString(raw)
		uuid := x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
		sid, e := randomHex(4)
		if e != nil {
			return nil, e
		}
		if sni == "" {
			sni = c.SNI
		}
		tls["server_name"] = sni
		tls["reality"] = Doc{"enabled": true, "handshake": Doc{"server": sni, "server_port": 443}, "private_key": base64.RawURLEncoding.EncodeToString(key.Bytes()), "short_id": []any{sid}}
		in["users"] = []any{Doc{"uuid": uuid, "flow": "xtls-rprx-vision"}}
	} else if kind == "anytls" || kind == "hysteria2" {
		if c.Domain == "" || c.Cert == "" || c.Key == "" {
			return nil, errors.New("请在 .env 中设置 SHARE_DOMAIN、ANYTLS_CERT_PATH、ANYTLS_KEY_PATH")
		}
		for _, p := range []string{c.Cert, c.Key} {
			if info, e := os.Stat(p); e != nil || !info.Mode().IsRegular() {
				return nil, errors.New("证书文件不存在")
			}
		}
		pw, e := randomHex(16)
		if e != nil {
			return nil, e
		}
		in["users"] = []any{Doc{"name": tag, "password": pw}}
		tls["server_name"] = c.Domain
		tls["certificate_path"] = c.Cert
		tls["key_path"] = c.Key
	} else {
		return nil, errors.New("不支持的入站类型")
	}
	d["inbounds"] = append(list(d["inbounds"]), in)
	p := strconv.Itoa(port)
	if kind == "hysteria2" {
		p += "/udp"
	}
	e = c.Apply(ctx, d, func() error {
		_, err := c.Run(ctx, "/usr/sbin/ufw", "allow", p, "comment", "sbbot-"+kind+"-"+tag)
		return err
	})
	return in, e
}
