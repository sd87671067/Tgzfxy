package sbbot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Bounded, immutable read snapshots. Hold the map reference to avoid pointer reuse.
var reads = struct {
	sync.Mutex
	items []readSnapshot
}{}

type readSnapshot struct {
	doc     Doc
	raw     []byte
	version string
	ptr     string
}

func remember(d Doc, raw []byte) {
	reads.Lock()
	defer reads.Unlock()
	reads.items = append(reads.items, readSnapshot{d, append([]byte(nil), raw...), configVersion(d), fmt.Sprintf("%p", d)})
	if len(reads.items) > 512 {
		reads.items = reads.items[len(reads.items)-512:]
	}
}
func baseline(d Doc, version string) ([]byte, error) {
	reads.Lock()
	defer reads.Unlock()
	for i := len(reads.items) - 1; i >= 0; i-- {
		s := reads.items[i]
		if version != "" && s.version == version || version == "" && s.ptr == fmt.Sprintf("%p", d) {
			return append([]byte(nil), s.raw...), nil
		}
	}
	return nil, errors.New("编辑快照已过期，请刷新")
}
func snapshot(version string) (Doc, error) {
	raw, e := baseline(nil, version)
	if e != nil {
		return nil, e
	}
	var d Doc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	e = dec.Decode(&d)
	if e == nil {
		remember(d, raw)
	}
	return d, e
}
func (c *ConfigManager) BridgeCall(ctx context.Context, path string, request any, result any) error {
	raw, e := json.Marshal(request)
	if e != nil {
		return e
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.ControlSocket)
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 180 * time.Second}
	req, e := http.NewRequestWithContext(ctx, "POST", "http://control"+path, bytes.NewReader(raw))
	if e != nil {
		return e
	}
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("配置控制服务不可用；保存未确认，请刷新核实")
	}
	defer resp.Body.Close()
	body, e := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if e != nil {
		return e
	}
	if resp.StatusCode != 200 {
		var v struct{ Error string }
		json.Unmarshal(body, &v)
		return fmt.Errorf("控制服务 HTTP %d: %s", resp.StatusCode, v.Error)
	}
	if result != nil {
		return json.Unmarshal(body, result)
	}
	return nil
}
func (c *ConfigManager) bridgeApply(ctx context.Context, d Doc, version, sni, tag, user, ip string) error {
	base, e := baseline(d, version)
	if e != nil {
		return e
	}
	candidate, e := json.Marshal(d)
	if e != nil {
		return e
	}
	h := sha256.Sum256(base)
	request := map[string]any{"base_bytes": base, "candidate": json.RawMessage(candidate), "hash": fmt.Sprintf("%x", h), "reality_sni": sni, "tag": tag, "auth_id": user, "expected_ip": ip}
	if e = c.BridgeCall(ctx, "/sbbot-apply", request, nil); e != nil {
		return e
	}
	// Re-read the authority after successful activation, including merged peer edits.
	fresh, e := Load(c.Path)
	if e != nil {
		return e
	}
	for k := range d {
		delete(d, k)
	}
	for k, v := range fresh {
		d[k] = v
	}
	raw, e := baseline(fresh, "")
	if e != nil {
		return e
	}
	remember(d, raw)
	return nil
}
func (c *ConfigManager) bridgeProbe(ctx context.Context, d Doc, target string) (string, error) {
	tag, user := target, ""
	if u, ok := findUser(d, target); ok {
		tag, user = u.Inbound, u.Name
	}
	var result struct {
		IP string `json:"ip"`
	}
	e := c.BridgeCall(ctx, "/sbbot-probe", map[string]string{"tag": tag, "auth_id": user}, &result)
	return result.IP, e
}
