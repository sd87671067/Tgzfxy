package sbbot

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	tg "local/bots/internal/telegram"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func realityTarget(text string) (string, string, int, error) {
	p := strings.Fields(strings.ToLower(text))
	if len(p) < 1 || len(p) > 2 {
		return "", "", 0, errors.New("请输入域名，或 SNI域名 handshake域名:端口")
	}
	sni, host, port := strings.TrimSuffix(p[0], "."), strings.TrimSuffix(p[0], "."), 443
	if len(p) == 2 {
		var ps string
		var e error
		host, ps, e = net.SplitHostPort(p[1])
		if e != nil {
			return "", "", 0, errors.New("handshake 必须为域名:端口")
		}
		port, e = strconv.Atoi(ps)
		if e != nil || port < 1 || port > 65535 {
			return "", "", 0, errors.New("端口无效")
		}
	}
	domain := regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
	if net.ParseIP(sni) != nil || net.ParseIP(host) != nil || len(sni) > 253 || len(host) > 253 || !domain.MatchString(sni) || !domain.MatchString(host) {
		return "", "", 0, errors.New("请输入有效的公网 DNS 域名")
	}
	return sni, host, port, nil
}
func (c *ConfigManager) checkReality(ctx context.Context, sni, host string, port int) error {
	if c.RealityCheck != nil {
		return c.RealityCheck(ctx, sni, host, port)
	}
	return checkRealityTarget(ctx, sni, host, port)
}
func checkRealityTarget(ctx context.Context, sni, host string, port int) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	for _, name := range []string{sni, host} {
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, name)
		if e != nil || len(ips) == 0 {
			return errors.New("DNS 解析失败")
		}
		for _, ip := range ips {
			if !ip.IP.IsGlobalUnicast() || ip.IP.IsPrivate() {
				return errors.New("目标必须解析到公网地址")
			}
		}
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 8 * time.Second}, Config: &tls.Config{ServerName: sni, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2", "http/1.1"}}}
	c, e := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if e != nil {
		return errors.New("TLS1.3 或证书主机名/信任链检查失败")
	}
	defer c.Close()
	state := c.(*tls.Conn).ConnectionState()
	if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != "h2" {
		return errors.New("REALITY 目标需要 TLS1.3 和 h2 支持")
	}
	return nil
}
func haproxyReality(raw, sni string) (string, error) {
	lines := strings.Split(raw, "\n")
	count := 0
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "acl" {
			continue
		}
		if fields[1] == "reality" {
			if len(fields) != 5 || fields[2] != "req.ssl_sni" || fields[3] != "-i" {
				return "", errors.New("HAProxy REALITY ACL 非标准格式，拒绝自动改写")
			}
			lines[i] = "    acl reality req.ssl_sni -i " + sni
			count++
			continue
		}
		// Fail closed for wildcard, regex and file based SNI ACLs: their overlap
		// cannot safely be inferred from a literal comparison.
		if strings.Contains(line, "ssl_sni") {
			if len(fields) < 5 || fields[2] != "req.ssl_sni" || fields[3] != "-i" {
				return "", errors.New("HAProxy 存在复杂 SNI ACL，需审阅")
			}
			for _, name := range fields[4:] {
				if strings.EqualFold(name, sni) {
					return "", errors.New("SNI 与 HAProxy 其他服务冲突")
				}
			}
		}
	}
	if count != 1 {
		return "", errors.New("未找到唯一 HAProxy REALITY SNI ACL")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n", nil
}

// Arguments carry data; no user text is interpolated into shell source.
const hapInstallScript = `import sys,pathlib,tempfile,subprocess,os,time
p=pathlib.Path('/etc/haproxy/haproxy.cfg');old=p.read_text()
assert old.strip()==sys.argv[1].strip(), 'HAProxy changed during confirmation'
fd,name=tempfile.mkstemp(prefix='.sbbot-',dir=p.parent)
try:
 os.fchmod(fd,0o600)
 with os.fdopen(fd,'w') as f:f.write(sys.argv[2]);f.flush();os.fsync(f.fileno())
 subprocess.run(['/usr/sbin/haproxy','-c','-f',name],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 backup=p.with_name(p.name+'.sbbot-backup-'+str(time.time_ns()));backup.write_text(old);backup.chmod(0o600)
 os.replace(name,p)
 try:
  subprocess.run(['/usr/bin/systemctl','reload','haproxy'],check=True)
  subprocess.run(['/usr/bin/systemctl','is-active','--quiet','haproxy'],check=True)
 except BaseException:
  p.write_text(old);p.chmod(0o600)
  subprocess.run(['/usr/bin/systemctl','reload','haproxy'],check=True)
  raise
finally:
 if os.path.exists(name):os.unlink(name)
`

func (b *Bot) realityPreview(ctx context.Context, m tg.Message, d Doc, st *pending) error {
	sni, host, port, e := realityTarget(m.Text)
	if e != nil {
		return e
	}
	if e = b.Config.checkReality(ctx, sni, host, port); e != nil {
		return e
	}
	if b.Config.Run == nil {
		return errors.New("未配置宿主机执行器")
	}
	hap, e := b.Config.Run(ctx, "/usr/bin/cat", "/etc/haproxy/haproxy.cfg")
	if e != nil {
		return e
	}
	if _, e = haproxyReality(hap, sni); e != nil {
		return e
	}
	token, e := randomHex(8)
	if e != nil {
		return e
	}
	st.Token = token
	st.Mode = "reality-confirm"
	st.Tag = sni
	st.Outbound = host
	st.Page = port
	st.File = hap
	st.Until = time.Now().Add(5 * time.Minute)
	sent, e := b.API.Send(ctx, m.Chat.ID, fmt.Sprintf("预览：统一入口 SNI → %s\nhandshake → %s:%d\n同步 HAProxy REALITY ACL 与新分享链接；保留 UUID、密钥、short ID、出站及流量。\nDNS、TLS1.3、证书、h2 检查通过；应用后用现有用户实际探测 REALITY。", sni, host, port), tg.K([]tg.Button{tg.B("确认并安全应用", "unified:confirm:"+token), tg.B("返回", "unified:menu")}), false)
	if e == nil {
		st.MessageID = sent.ID
	}
	return e
}
func (b *Bot) realityConfirm(ctx context.Context, m tg.Message, d Doc, cb string, st *pending) error {
	if st.Mode != "reality-confirm" || cb != "unified:confirm:"+st.Token || st.Token == "" || st.ChatID != m.Chat.ID || st.MessageID != m.ID || time.Now().After(st.Until) || st.Version != configVersion(d) {
		return errors.New("确认已失效，请重新检查域名")
	}
	sni, host, port, old, version := st.Tag, st.Outbound, st.Page, st.File, st.Version
	*st = pending{}
	if e := b.Config.checkReality(ctx, sni, host, port); e != nil {
		return e
	}
	candidate, e := haproxyReality(old, sni)
	if e != nil {
		return e
	}
	users := UnifiedUsers(d)
	if len(users) == 0 || b.Config.Probe == nil {
		return errors.New("需要现有 UUID 和 REALITY 实际连接探测器")
	}
	probeKey := ""
	blocked := Blocked(d)
	for _, u := range users {
		if !blocked[u.Key] {
			probeKey = u.Key
			break
		}
	}
	if probeKey == "" {
		return errors.New("请先启用至少一个 UUID，以便验证实际 REALITY 连接")
	}
	for _, raw := range list(d["inbounds"]) {
		in := obj(raw)
		if isUnified(in) {
			t := nested(in, "tls")
			t["server_name"] = sni
			h := nested(nested(t, "reality"), "handshake")
			h["server"] = host
			h["server_port"] = port
		}
	}
	switched := false
	e = b.Config.applyUnifiedVerified(ctx, d, version, func() error {
		if _, err := b.Config.Run(ctx, "/usr/bin/python3", "-c", hapInstallScript, old, candidate); err != nil {
			return err
		}
		switched = true
		_, err := b.Config.Probe(ctx, d, probeKey)
		return err
	})
	if e != nil && switched {
		recovery, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if _, r := b.Config.Run(recovery, "/usr/bin/python3", "-c", hapInstallScript, candidate, old); r != nil {
			return fmt.Errorf("%v; HAProxy 回滚失败：%v", e, r)
		}
	}
	if e != nil {
		return e
	}
	return b.API.Edit(ctx, m.Chat.ID, m.ID, "✅ REALITY 与 HAProxy 已检查并应用；新分享链接已使用 SNI "+sni+"。", tg.K([]tg.Button{tg.B("分享链接", "unified:share"), tg.B("返回", "unified:menu")}), false)
}
