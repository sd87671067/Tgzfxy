package sbbot

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
)

// The helper runs in the host namespace, using a private, short-lived sing-box
// client. No credentials are passed in argv or returned in diagnostics.
const trafficProbe = `
import json,socket,subprocess,tempfile,time,os,sys,ipaddress
p=sys.argv[1]
d=json.load(open(p))
s=socket.socket();s.bind(('127.0.0.1',0));port=s.getsockname()[1];s.close()
d['inbounds']=[{'type':'socks','tag':'probe','listen':'127.0.0.1','listen_port':port}]
f=tempfile.NamedTemporaryFile(mode='w',dir=os.path.dirname(p),prefix='.traffic-',suffix='.json',delete=False)
proc=None
try:
 json.dump(d,f);f.close()
 subprocess.run(['/usr/local/bin/sing-box','check','-c',f.name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=True)
 proc=subprocess.Popen(['/usr/local/bin/sing-box','run','-c',f.name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 for _ in range(50):
  if proc.poll() is not None: raise RuntimeError('probe client stopped')
  try:
   c=socket.create_connection(('127.0.0.1',port),.1);c.close();break
  except OSError:time.sleep(.05)
 else:raise RuntimeError('probe client unavailable')
 for url in ['https://api.ipify.org','https://ifconfig.me/ip']:
  r=subprocess.run(['/usr/bin/curl','--fail','--silent','--max-time','12','--socks5-hostname','127.0.0.1:'+str(port),url],capture_output=True,text=True)
  if r.returncode==0:
   try:print(ipaddress.ip_address(r.stdout.strip()));break
   except ValueError:pass
 else:raise RuntimeError('exit IP verification failed')
finally:
 if proc is not None:
  proc.terminate()
  try:proc.wait(timeout=3)
  except subprocess.TimeoutExpired:proc.kill();proc.wait()
 os.unlink(f.name)
`

// ProbeTraffic accepts either a real outbound tag or a unified user key. User
// probes traverse the public SNI listener when UnifiedPublicPort is configured.
func (c *ConfigManager) ProbeTraffic(ctx context.Context, d Doc, target string) (string, error) {
	p := Doc{"log": Doc{"level": "error"}, "outbounds": d["outbounds"], "dns": d["dns"], "route": Doc{"final": target, "default_domain_resolver": obj(d["route"])["default_domain_resolver"]}}
	if _, ok := findUser(d, target); ok {
		links, err := ShareLinks(d, []string{target}, "127.0.0.1", "", c.UnifiedPublicPort)
		if c.UnifiedPublicPort == 0 {
			links, err = ShareLinks(d, []string{target}, "127.0.0.1", "")
		}
		if err != nil || len(links) != 1 {
			return "", errors.New("无法生成用户探测配置")
		}
		out, err := ParseOutbound(links[0])
		if err != nil {
			return "", err
		}
		out["tag"] = "verify-user"
		p["outbounds"] = []any{out}
		p["route"] = Doc{"final": "verify-user"}
		delete(p, "dns")
	} else if Kind(d, target) != "outbound" || target == ManagedBlock || managedUserOutbound(target) {
		return "", errors.New("无效探测目标")
	}
	f, err := os.CreateTemp(filepath.Dir(c.Path), ".sbbot-probe-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(p)
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	path := f.Name()
	if filepath.Dir(c.Path) == "/config" {
		path = filepath.Join("/etc/sing-box", filepath.Base(path))
	}
	result, err := c.Run(ctx, "/usr/bin/python3", "-c", trafficProbe, path)
	if err != nil {
		return "", errors.New("出口探测失败（TCP、认证或目的站不可达），未确认健康")
	}
	if net.ParseIP(result) == nil {
		return "", errors.New("出口未返回有效 IP")
	}
	return result, nil
}
