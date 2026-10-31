package sbbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	tg "local/bots/internal/telegram"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type UserCycle struct{ Baseline, Allowance, Expires, Started int64 }

func resolveUnified(d Doc, arg string) (string, error) {
	if _, ok := findUser(d, arg); ok {
		return arg, nil
	}
	key := ""
	for _, u := range UnifiedUsers(d) {
		if u.Name == arg {
			if key != "" {
				return "", errors.New("用户名称重复，请使用完整用户标识")
			}
			key = u.Key
		}
	}
	if key == "" {
		return "", errors.New("统一用户不存在")
	}
	return key, nil
}
func parseAllLimit(args []string) (int64, int64, error) {
	if len(args) < 1 || len(args) > 3 {
		return 0, 0, errors.New("用法：/alllimit tag [流量MB] [时间day]")
	}
	mb, days := int64(500*1024), int64(30)
	var err error
	if len(args) > 1 {
		mb, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return 0, 0, errors.New("流量必须为正整数 MB")
		}
	}
	if len(args) > 2 {
		days, err = strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return 0, 0, errors.New("时间必须为正整数天")
		}
	}
	if mb <= 0 || mb > math.MaxInt64/(1024*1024) || days <= 0 || days > 36500 {
		return 0, 0, errors.New("流量或天数超出范围（最多36500天）")
	}
	return mb * 1024 * 1024, days, nil
}
func (s *Store) Cycle(ctx context.Context, key string) (*UserCycle, error) {
	c := &UserCycle{}
	err := s.DB.QueryRowContext(ctx, "SELECT baseline,allowance,expires_at,started_at FROM user_cycles WHERE tag=?", key).Scan(&c.Baseline, &c.Allowance, &c.Expires, &c.Started)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}
func (s *Store) putCycle(ctx context.Context, key string, c *UserCycle) error {
	if c == nil {
		_, e := s.DB.ExecContext(ctx, "DELETE FROM user_cycles WHERE tag=?", key)
		return e
	}
	_, e := s.DB.ExecContext(ctx, `INSERT INTO user_cycles VALUES(?,?,?,?,?) ON CONFLICT(tag) DO UPDATE SET baseline=excluded.baseline,allowance=excluded.allowance,expires_at=excluded.expires_at,started_at=excluded.started_at`, key, c.Baseline, c.Allowance, c.Expires, c.Started)
	return e
}

// Bot.Mu serializes collection and policy mutation. Persisted desired policy is
// re-enforced by collection after restart; runtime apply failures restore it.
func (s *Store) SetCycle(ctx context.Context, key string, amount, days int64, apply func() error) error {
	old, e := s.Cycle(ctx, key)
	if e != nil {
		return e
	}
	total, e := s.Usage(ctx, key, time.Unix(0, 0))
	if e != nil {
		return e
	}
	now := time.Now().Unix()
	if amount <= 0 || days <= 0 || days > 36500 || total > math.MaxInt64-amount {
		return errors.New("额度溢出或参数无效")
	}
	// Snapshot SQLite before changing the persisted policy.
	if e = s.backupPolicy(ctx, key, old); e != nil {
		return e
	}
	next := &UserCycle{total, amount, now + days*86400, now}
	if e = s.putCycle(ctx, key, next); e != nil {
		return e
	}
	if e = apply(); e != nil {
		recovery, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if r := s.putCycle(recovery, key, old); r != nil {
			return fmt.Errorf("%v; 周期回滚失败：%v", e, r)
		}
		return e
	}
	return nil
}

// Persist only the policy being changed. Never VACUUM/copy traffic history on save.
func (s *Store) backupPolicy(ctx context.Context, key string, previous *UserCycle) error {
	var path string
	if err := s.DB.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		return err
	}
	if path == "" {
		return nil
	}
	raw, err := json.Marshal(struct {
		Key      string
		Previous *UserCycle
		SavedAt  int64
	}{key, previous, time.Now().Unix()})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".policy-recovery-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path+".policy-recovery.json"); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (b *Bot) allLimit(ctx context.Context, m tg.Message, d Doc, args []string, st *pending) error {
	amount, days, e := parseAllLimit(args)
	if e != nil {
		return e
	}
	key, e := resolveUnified(d, args[0])
	if e != nil {
		return e
	}
	if !b.Config.AutoRestart || b.Config.Run == nil {
		return errors.New("未启用配置检查与重载")
	}
	if e = b.Store.SetCycle(ctx, key, amount, days, func() error { return b.enforce(ctx, d) }); e != nil {
		return e
	}
	u, _ := findUser(d, key)
	if e = b.send(ctx, m.Chat.ID, fmt.Sprintf("✅ %s：新增可用 %s，%d天，从现在起计算。历史累计保留，以当前累计为基线；原月额度、有效期和手动暂停仍独立生效。", u.Name, Human(amount), days), nil); e != nil {
		return e
	}
	return b.userPage(ctx, m, d, st, st.Page, false)
}
