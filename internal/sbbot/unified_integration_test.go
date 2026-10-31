package sbbot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func markerSOCKS(t *testing.T, marker string) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); l.Close() })
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(20 * time.Second))
				h := make([]byte, 2)
				if _, e := io.ReadFull(c, h); e != nil {
					return
				}
				methods := make([]byte, int(h[1]))
				if _, e = io.ReadFull(c, methods); e != nil {
					return
				}
				c.Write([]byte{5, 0})
				req := make([]byte, 4)
				if _, e = io.ReadFull(c, req); e != nil {
					return
				}
				n := 0
				switch req[3] {
				case 1:
					n = 4
				case 4:
					n = 16
				case 3:
					b := make([]byte, 1)
					if _, e = io.ReadFull(c, b); e != nil {
						return
					}
					n = int(b[0])
				default:
					return
				}
				if _, e = io.CopyN(io.Discard, c, int64(n+2)); e != nil {
					return
				}
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
				r, e := http.ReadRequest(bufio.NewReader(c))
				if e != nil {
					return
				}
				if r.URL.Path == "/hold" {
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: 9999999\r\n\r\n%s", marker)
					<-done
				} else {
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(marker), marker)
				}
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}
func TestUnifiedRealSingBoxRealityVisionRoutingAccountingRollback(t *testing.T) {
	binary := os.Getenv("SING_BOX_BINARY")
	if binary == "" {
		t.Skip("set SING_BOX_BINARY to run isolated real REALITY integration")
	}
	b, _ := testBot(t)
	d := unifiedDoc(t)
	in := obj(list(d["inbounds"])[3])
	port := freePort(t)
	in["listen_port"] = port
	in["users"] = []any{}
	d["inbounds"] = []any{in}
	a, z := markerSOCKS(t, "exit-A"), markerSOCKS(t, "exit-B")
	d["outbounds"] = []any{Doc{"type": "direct", "tag": "direct"}, Doc{"type": "socks", "tag": "exit-a", "server": "127.0.0.1", "server_port": a}, Doc{"type": "socks", "tag": "exit-b", "server": "127.0.0.1", "server_port": z}}
	nested(d, "route")["rules"] = []any{Doc{"inbound": []string{"sbbot-unified-main"}, "action": "reject"}}
	var users []UnifiedUser
	for _, x := range []struct{ n, o string }{{"one", "exit-a"}, {"two", "exit-b"}, {"https", "direct"}, {"unbound", "direct"}} {
		u, e := AddUnifiedUser(d, "sbbot-unified-main", x.n, "", x.o)
		if e != nil {
			t.Fatal(e)
		}
		users = append(users, u)
	}
	var rules []any
	for _, raw := range list(obj(d["route"])["rules"]) {
		if !ruleForUser(obj(raw), users[3]) {
			rules = append(rules, raw)
		}
	}
	nested(d, "route")["rules"] = rules
	apiPort := freePort(t)
	d["experimental"] = Doc{"clash_api": Doc{"external_controller": fmt.Sprintf("127.0.0.1:%d", apiPort)}}
	d["log"] = Doc{"level": "error"}
	installDoc(t, b, d)
	var server *exec.Cmd
	var logs bytes.Buffer
	stop := func() {
		if server != nil && server.Process != nil {
			server.Process.Kill()
			server.Wait()
			server = nil
		}
	}
	start := func() error {
		logs.Reset()
		server = exec.Command(binary, "run", "-c", b.Config.Path)
		server.Stdout = &logs
		server.Stderr = &logs
		if e := server.Start(); e != nil {
			return e
		}
		for i := 0; i < 80; i++ {
			c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
			if e == nil {
				c.Close()
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return fmt.Errorf("isolated server readiness failed")
	}
	t.Cleanup(stop)
	b.Config.AutoRestart = true
	b.Config.Run = func(ctx context.Context, prog string, args ...string) (string, error) {
		if strings.HasSuffix(prog, "sing-box") {
			out, e := exec.CommandContext(ctx, binary, args...).CombinedOutput()
			return string(out), e
		}
		if args[0] == "restart" {
			stop()
			return "", start()
		}
		if server == nil {
			return "", fmt.Errorf("stopped")
		}
		return "", nil
	}
	if _, e := b.Config.Run(context.Background(), binary, "check", "-c", b.Config.Path); e != nil {
		t.Fatal(e)
	}
	if e := start(); e != nil {
		t.Fatal(e)
	}
	// The client uses synthetic UUIDs and the server's fresh synthetic REALITY key.
	client := Doc{"log": Doc{"level": "error"}}
	var ci, co, cr []any
	var socks []int
	pk, e := PublicKey(str(obj(obj(in["tls"])["reality"])["private_key"]))
	if e != nil {
		t.Fatal(e)
	}
	unknown, e := NewUUID()
	if e != nil {
		t.Fatal(e)
	}
	users = append(users, UnifiedUser{Name: "unknown", UUID: unknown})
	for _, u := range users {
		p := freePort(t)
		socks = append(socks, p)
		ci = append(ci, Doc{"type": "mixed", "tag": u.Name, "listen": "127.0.0.1", "listen_port": p})
		co = append(co, Doc{"type": "vless", "tag": u.Name, "server": "127.0.0.1", "server_port": port, "uuid": u.UUID, "flow": "xtls-rprx-vision", "tls": Doc{"enabled": true, "server_name": "www.samsung.com", "utls": Doc{"enabled": true, "fingerprint": "chrome"}, "reality": Doc{"enabled": true, "public_key": pk, "short_id": "aabbccdd"}}})
		cr = append(cr, Doc{"inbound": []string{u.Name}, "outbound": u.Name})
	}
	client["inbounds"] = ci
	client["outbounds"] = co
	client["route"] = Doc{"rules": cr}
	raw, _ := json.Marshal(client)
	cp := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(cp, raw, 0600)
	cmd := exec.Command(binary, "run", "-c", cp)
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	time.Sleep(300 * time.Millisecond)
	curl := func(i int, url string) (string, error) {
		out, e := exec.Command("curl", "--silent", "--show-error", "--fail", "--max-time", "12", "--proxy", fmt.Sprintf("socks5h://127.0.0.1:%d", socks[i]), url).Output()
		return string(out), e
	}
	for i, want := range []string{"exit-A", "exit-B"} {
		out, e := curl(i, "http://probe.invalid/")
		if e != nil || out != want {
			t.Fatalf("route %d body=%q err=%v", i, out, e)
		}
	}
	if out, e := curl(2, "https://api.ipify.org"); e != nil || net.ParseIP(strings.TrimSpace(out)) == nil {
		t.Fatalf("real Vision HTTPS failed: %q %v", out, e)
	}
	for _, i := range []int{3, 4} {
		if out, e := curl(i, "http://probe.invalid/"); e == nil || strings.Contains(out, "exit-") {
			t.Fatalf("unauthorized/unbound %d accepted", i)
		}
	}
	holdCtx, cancel := context.WithCancel(context.Background())
	hold := exec.CommandContext(holdCtx, "curl", "--silent", "--max-time", "15", "--proxy", fmt.Sprintf("socks5h://127.0.0.1:%d", socks[0]), "http://probe.invalid/hold")
	if e = hold.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cancel(); hold.Wait() }()
	found := false
	for i := 0; i < 50 && !found; i++ {
		payload, e := JSONGet(context.Background(), http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/connections", apiPort), nil)
		if e == nil {
			for _, raw := range list(payload["connections"]) {
				if ConnectionAccounting(obj(raw), d) == users[0].Key {
					found = true
					t.Log("actual Clash chains attribute traffic to authenticated user")
				}
			}
			if found {
				if e = b.Store.Sample(context.Background(), payload, d); e != nil {
					t.Fatal(e)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !found {
		t.Fatal("real Clash JSON did not identify user via selector chain")
	}
	cancel()
	hold.Wait()
	// Two users share the single direct outbound. Real Clash matched rules must
	// distinguish them even though their chains are identical and lack usernames.
	if e = BindUser(d, users[3].Key, "direct"); e != nil {
		t.Fatal(e)
	}
	if e = b.Config.ApplyUnified(context.Background(), d); e != nil {
		t.Fatal(e)
	}
	directServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("d", 4096)))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer directServer.Close()
	directCtx, stopDirect := context.WithCancel(context.Background())
	var directCommands []*exec.Cmd
	for _, i := range []int{2, 3} {
		cmd := exec.CommandContext(directCtx, "curl", "--silent", "--max-time", "15", "--noproxy", "", "--proxy", fmt.Sprintf("socks5h://127.0.0.1:%d", socks[i]), directServer.URL)
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		directCommands = append(directCommands, cmd)
	}
	defer func() {
		stopDirect()
		for _, cmd := range directCommands {
			cmd.Wait()
		}
	}()
	foundDirect := map[string]bool{}
	for n := 0; n < 50 && len(foundDirect) < 2; n++ {
		payload, err := JSONGet(context.Background(), http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/connections", apiPort), nil)
		if err == nil {
			for _, raw := range list(payload["connections"]) {
				c := obj(raw)
				key := ConnectionAccounting(c, d)
				if key == users[2].Key || key == users[3].Key {
					foundDirect[key] = true
					t.Logf("real direct accounting: %s chains=%v rule=%s", key, c["chains"], c["rule"])
				}
			}
			if err = b.Store.Sample(context.Background(), payload, d); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(foundDirect) != 2 {
		t.Fatal("shared direct users lost independent accounting", foundDirect)
	}
	for _, i := range []int{2, 3} {
		n, err := b.Store.Usage(context.Background(), users[i].Key, time.Unix(0, 0))
		if err != nil || n < 4096 {
			t.Fatal("direct traffic missing", users[i].Name, n, err)
		}
	}
	stopDirect()
	// Real candidate check, real isolated restart and scoped pause.
	if e = BindUser(d, users[0].Key, "exit-b"); e != nil {
		t.Fatal(e)
	}
	if e = b.Config.ApplyUnified(context.Background(), d); e != nil {
		t.Fatal(e)
	}
	if out, e := curl(0, "http://probe.invalid/"); e != nil || out != "exit-B" {
		t.Fatalf("rebind: %q %v", out, e)
	}
	SetBlockedDoc(d, map[string]bool{users[0].Key: true})
	if e = b.Config.ApplyUnified(context.Background(), d); e != nil {
		t.Fatal(e)
	}
	if _, e = curl(0, "http://probe.invalid/"); e == nil {
		t.Fatal("paused user connected")
	}
	if out, e := curl(1, "http://probe.invalid/"); e != nil || out != "exit-B" {
		t.Fatal("pause leaked to other user", e)
	}
	// Exercise the new lifetime cycle against actual REALITY connections.
	SetBlockedDoc(d, map[string]bool{})
	if e = b.Config.ApplyUnified(context.Background(), d); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if e = b.Store.SetCycle(ctx, users[0].Key, 1024, 7, func() error { return b.enforce(ctx, d) }); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Store.DB.Exec("INSERT INTO samples VALUES(?,?,?)", time.Now().Unix(), users[0].Key, 1024); e != nil {
		t.Fatal(e)
	}
	if e = b.enforce(ctx, d); e != nil {
		t.Fatal(e)
	}
	if _, e = curl(0, "http://probe.invalid/"); e == nil {
		t.Fatal("cycle exhausted user connected")
	}
	if out, e := curl(1, "http://probe.invalid/"); e != nil || out != "exit-B" {
		t.Fatal("cycle block affected other UUID", e)
	}
	if e = b.Store.SetCycle(ctx, users[0].Key, 4096, 7, func() error { return b.enforce(ctx, d) }); e != nil {
		t.Fatal(e)
	}
	if out, e := curl(0, "http://probe.invalid/"); e != nil || out != "exit-B" {
		t.Fatal("renewed cycle not usable", e)
	}
	cycle, e := b.Store.Cycle(ctx, users[0].Key)
	if e != nil {
		t.Fatal(e)
	}
	cycle.Expires = time.Now().Unix() - 1
	if e = b.Store.putCycle(ctx, users[0].Key, cycle); e != nil {
		t.Fatal(e)
	}
	if e = b.enforce(ctx, d); e != nil {
		t.Fatal(e)
	}
	if _, e = curl(0, "http://probe.invalid/"); e == nil {
		t.Fatal("cycle expired user connected")
	}
	if out, e := curl(1, "http://probe.invalid/"); e != nil || out != "exit-B" {
		t.Fatal("cycle expiry affected other UUID", e)
	}
	// Fail health after a valid real restart, then restore and start the original.
	before, _ := os.ReadFile(b.Config.Path)
	run := b.Config.Run
	b.Config.Run = func(ctx context.Context, p string, args ...string) (string, error) {
		if args[0] == "is-active" {
			return "", fmt.Errorf("injected health failure")
		}
		return run(ctx, p, args...)
	}
	SetBlockedDoc(d, map[string]bool{})
	if e = b.Config.ApplyUnified(context.Background(), d); e == nil {
		t.Fatal("injected failure accepted")
	}
	after, _ := os.ReadFile(b.Config.Path)
	if !bytes.Equal(before, after) {
		t.Fatal("rollback did not restore original config")
	}
	if out, e := curl(1, "http://probe.invalid/"); e != nil || out != "exit-B" {
		t.Fatal("rollback service not recovered", e)
	}
}

func TestUnifiedProductionMappingCandidateReadOnly(t *testing.T) {
	path := os.Getenv("SBBOT_MAPPING_FIXTURE")
	binary := os.Getenv("SING_BOX_BINARY")
	if path == "" || binary == "" {
		t.Skip("optional private migration fixture required")
	}
	d, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	before := configVersion(d)
	plan, err := PlanMigration(d)
	if err != nil {
		t.Fatal(err)
	}
	template := ""
	for _, raw := range list(d["inbounds"]) {
		if str(obj(raw)["type"]) == "vless" {
			template = str(obj(raw)["tag"])
			break
		}
	}
	staged, err := StageUnified(d, template, "sbbot-unified-rehearsal", "127.0.0.1", freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range plan {
		if _, err = AddUnifiedUser(staged, "sbbot-unified-rehearsal", u.ProposedName, u.ProposedUUID, u.Outbound); err != nil {
			t.Fatal(err)
		}
	}
	if err = ValidateUnified(staged); err != nil {
		t.Fatal(err)
	}
	if configVersion(d) != before {
		t.Fatal("source config modified")
	}
	oldIns := list(d["inbounds"])
	candidateIns := list(staged["inbounds"])
	if configVersion(Doc{"inbounds": oldIns}) != configVersion(Doc{"inbounds": candidateIns[:len(oldIns)]}) {
		t.Fatal("legacy listeners changed")
	}
	candidate := filepath.Join(t.TempDir(), "private-candidate.json")
	raw, _ := json.Marshal(staged)
	if err = os.WriteFile(candidate, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = exec.Command(binary, "check", "-c", candidate).CombinedOutput(); err != nil {
		t.Fatal("candidate validation failed")
	}
	t.Logf("validated %d user migration mappings; all legacy listeners preserved; candidate never started", len(plan))
}
