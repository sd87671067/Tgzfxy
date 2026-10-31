package sbbot

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ResolveShareHost preserves an explicit address or domain. Automatic discovery
// uses HTTPS public IPv4 services, never hostname -I or a private interface.
func ResolveShareHost(ctx context.Context, explicit string) (string, error) {
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return resolveShareHost(ctx, explicit, client, []string{"https://api4.ipify.org", "https://ipv4.icanhazip.com"})
}

func resolveShareHost(ctx context.Context, explicit string, client *http.Client, endpoints []string) (string, error) {
	if host := strings.TrimSpace(explicit); host != "" {
		return host, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 9*time.Second)
	defer cancel()
	for _, endpoint := range endpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 65))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || len(raw) > 64 {
			continue
		}
		host := strings.TrimSpace(string(raw))
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			continue
		}
		// Reject shared, documentation, benchmark and reserved IPv4 ranges too.
		reserved := []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"}
		valid := true
		for _, cidr := range reserved {
			_, block, _ := net.ParseCIDR(cidr)
			if block.Contains(ip) {
				valid = false
				break
			}
		}
		if valid {
			return ip.To4().String(), nil
		}
	}
	return "", errors.New("无法获取公网 IPv4，请显式设置 SHARE_HOST")
}
