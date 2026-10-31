package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"local/bots/internal/forwarder"
	"local/bots/internal/mtproto"
	"local/bots/internal/sbbot"
	tg "local/bots/internal/telegram"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
)

func env(k, d string) string {
	v, ok := os.LookupEnv(k)
	if ok {
		return strings.TrimSpace(v)
	}
	return d
}
func yes(k string, d bool) bool {
	v := strings.ToLower(env(k, strconv.FormatBool(d)))
	return v == "true" || v == "1" || v == "yes" || v == "on"
}
func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	check := flag.Bool("check", false, "validate configuration and database without network or mutation")
	probe := flag.Bool("probe", false, "check Telegram authentication without polling or sending messages")
	maintenance := flag.Bool("maintenance", false, "remove expired operation logs")
	flag.Parse()
	dir := env("DATA_DIR", "/data")
	path := env("CONFIG_PATH", "/config/config.json")
	if *maintenance {
		if err := sbbot.Maintenance(context.Background(), dir); err != nil {
			return err
		}
		fmt.Println("sbbot maintenance completed")
		return nil
	}
	token := env("TELEGRAM_BOT_TOKEN", "")
	admins := map[int64]bool{}
	for _, v := range strings.Split(env("TELEGRAM_ADMIN_IDS", ""), ",") {
		if strings.TrimSpace(v) == "" {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || id <= 0 {
			return errors.New("TELEGRAM_ADMIN_IDS 必须为正整数列表")
		}
		admins[id] = true
	}
	if token == "" || len(admins) == 0 {
		return errors.New("请设置 TELEGRAM_BOT_TOKEN 和 TELEGRAM_ADMIN_IDS")
	}
	tz, err := time.LoadLocation(env("TIMEZONE", "Asia/Shanghai"))
	if err != nil {
		return errors.New("TIMEZONE 无效")
	}
	if *check {
		if _, err = sbbot.Load(path); err != nil {
			return err
		}
		for _, name := range []string{"sbbot.db", "forwarder.db"} {
			db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, name)+"?mode=ro")
			if err != nil {
				return err
			}
			var result int
			err = db.QueryRow("PRAGMA schema_version").Scan(&result)
			db.Close()
			if err != nil {
				return err
			}
			if result <= 0 {
				return errors.New("database schema is empty")
			}
		}
		fmt.Println("sbbot configuration and databases OK")
		return nil
	}
	if *probe {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		var me tg.User
		if err := tg.New(token).Call(ctx, "getMe", map[string]any{}, &me); err != nil {
			return err
		}
		id, _ := strconv.Atoi(env("FORWARD_API_ID", ""))
		if id > 0 && env("FORWARD_API_HASH", "") != "" {
			client, err := mtproto.New(ctx, id, env("FORWARD_API_HASH", ""), dir)
			if err != nil {
				return err
			}
			if err := client.Probe(ctx); err != nil {
				return err
			}
		}
		fmt.Println("Telegram bot and configured user session authentication OK")
		return nil
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	limit, err := strconv.ParseFloat(env("DEFAULT_MONTHLY_LIMIT_GB", "500"), 64)
	if err != nil || limit < 0 || math.IsNaN(limit) || math.IsInf(limit, 0) || limit >= float64(math.MaxInt64)/(1<<30) {
		return errors.New("DEFAULT_MONTHLY_LIMIT_GB 无效")
	}
	store, err := sbbot.OpenStore(filepath.Join(dir, "sbbot.db"), int64(limit*(1<<30)))
	if err != nil {
		return err
	}
	defer store.DB.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	api := tg.New(token)
	features := map[string]bool{"bwh": env("BWH_API_KEY", "") != "" && env("BWH_VEID", "") != ""}
	for feature, program := range map[string]string{"singbox": "/usr/local/bin/sing-box", "ufw": "/usr/sbin/ufw"} {
		_, e := sbbot.HostRun(ctx, "/usr/bin/test", "-x", program)
		features[feature] = e == nil
	}
	publicPort, err := strconv.Atoi(env("UNIFIED_PUBLIC_PORT", "0"))
	if err != nil || publicPort < 0 || publicPort > 65535 {
		return errors.New("UNIFIED_PUBLIC_PORT 必须为 0-65535，0 表示使用监听端口")
	}
	config := &sbbot.ConfigManager{Path: path, UnifiedPublicPort: publicPort, Domain: env("SHARE_DOMAIN", ""), Cert: env("ANYTLS_CERT_PATH", ""), Key: env("ANYTLS_KEY_PATH", ""), SNI: env("REALITY_SNI", "itunes.apple.com"), AutoRestart: yes("AUTO_RESTART_SING_BOX", true), Run: sbbot.HostRun}
	config.Probe = config.ProbeTraffic
	host, err := sbbot.ResolveShareHost(ctx, env("SHARE_HOST", ""))
	if err != nil {
		return err
	}
	bot := &sbbot.Bot{API: api, Store: store, Config: config, Admins: admins, Features: features, TZ: tz, HTTP: &http.Client{Timeout: 60 * time.Second}, Host: host, APIURL: strings.TrimRight(env("SINGBOX_API_URL", "http://127.0.0.1:9090"), "/"), APISecret: env("SINGBOX_API_SECRET", ""), BWHURL: "https://api.64clouds.com/v1", BWHKey: env("BWH_API_KEY", ""), BWHVEID: env("BWH_VEID", "")}
	bot.InitStates()
	if yes("AUTO_CONFIGURE_CLASH_API", true) {
		if err = bot.EnsureAPI(ctx); err != nil {
			return err
		}
	}
	var me tg.User
	if err = api.Call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return err
	}
	var fw *forwarder.Engine
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		if fw != nil {
			fw.DB.Close()
		}
	}()
	appID, _ := strconv.Atoi(env("FORWARD_API_ID", ""))
	if appID > 0 && env("FORWARD_API_HASH", "") != "" {
		client, err := mtproto.New(ctx, appID, env("FORWARD_API_HASH", ""), dir)
		if err != nil {
			return err
		}
		fw, err = forwarder.Open(filepath.Join(dir, "forwarder.db"), client, api)
		if err != nil {
			return err
		}
		fw.BotID = me.ID
		fw.BotName = me.Username
		bot.Forward = fw
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fw.Serve(ctx, client, client.Events); err != nil && ctx.Err() == nil {
				log.Print("forwarder stopped: ", err)
			}
		}()
	}
	commands := []map[string]string{}
	for _, v := range [][2]string{{"panel", "打开操作面板"}, {"bind", "添加转发规则"}, {"switch", "切换规则设置"}, {"cs", "设置转发重试"}, {"cancel", "取消操作"}, {"today", "今日流量"}, {"month", "本月流量"}, {"status", "流量状态"}, {"share", "生成分享链接"}, {"alllimit", "设置用户流量与天数"}, {"addout", "添加出站"}, {"addvless", "添加VLESS入站"}, {"addhy2", "添加Hysteria2入站"}, {"addanytls", "添加AnyTLS入站"}, {"add", "快速添加协议"}, {"protocols", "查看入站"}, {"setexpiry", "设置有效期"}, {"bwh", "搬瓦工管理"}, {"help", "帮助"}} {
		if bot.CommandAvailable(v[0]) {
			commands = append(commands, map[string]string{"command": v[0], "description": v[1]})
		}
	}
	if err = api.Call(ctx, "setMyCommands", map[string]any{"commands": commands}, nil); err != nil {
		log.Print("setMyCommands failed")
	}
	if err = api.Call(ctx, "setChatMenuButton", map[string]any{"menu_button": map[string]string{"type": "commands"}}, nil); err != nil {
		log.Print("setChatMenuButton failed")
	}
	interval, _ := strconv.Atoi(env("POLL_SECONDS", "60"))
	interval = max(15, interval)
	hour, _ := strconv.Atoi(env("DAILY_REPORT_HOUR", "9"))
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for {
			if e := bot.Collect(ctx); e != nil && ctx.Err() == nil {
				log.Print("collector failed; will retry")
			}
			if e := bot.Daily(ctx, hour); e != nil && ctx.Err() == nil {
				log.Print("daily report failed; will retry")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		timer := time.NewTimer(10 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			maintenanceCtx, done := context.WithTimeout(ctx, 30*time.Second)
			if e := sbbot.Maintenance(maintenanceCtx, dir); e != nil && ctx.Err() == nil {
				log.Print("maintenance deferred: ", e)
			}
			done()
			timer.Reset(6 * time.Hour)
		}
	}()
	log.Print("sbbot Go started; polling Telegram")
	return api.PollQueued(ctx, bot.Prepare, bot.Handle, func(err error) {
		if ctx.Err() == nil {
			log.Printf("bot request failed: %v", err)
		}
	})
}
