package sbbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type Store struct {
	DB           *sql.DB
	DefaultLimit int64
}

func OpenStore(path string, limit int64) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+url.PathEscape(path)+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS samples(ts INTEGER NOT NULL,tag TEXT NOT NULL,bytes INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS connections(id TEXT PRIMARY KEY,tag TEXT NOT NULL,total INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS limits(tag TEXT PRIMARY KEY,bytes INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS reports(day TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS opslog(id INTEGER PRIMARY KEY AUTOINCREMENT,ts INTEGER NOT NULL,actor TEXT,action TEXT,detail TEXT);
CREATE TABLE IF NOT EXISTS proto_expiry(tag TEXT PRIMARY KEY,expires_at INTEGER);
CREATE TABLE IF NOT EXISTS user_legacy(user_key TEXT PRIMARY KEY,legacy_tag TEXT NOT NULL UNIQUE);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db, limit}, nil
}
func (s *Store) Limit(ctx context.Context, tag string) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, "SELECT bytes FROM limits WHERE tag=?", tag).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return s.DefaultLimit, nil
	}
	return n, err
}
func (s *Store) SetLimit(ctx context.Context, tag string, n int64) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO limits(tag,bytes) VALUES(?,?) ON CONFLICT(tag) DO UPDATE SET bytes=excluded.bytes", tag, n)
	return err
}
func (s *Store) Expiry(ctx context.Context, tag string) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, "SELECT expires_at FROM proto_expiry WHERE tag=?", tag).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
func (s *Store) SetExpiry(ctx context.Context, tag string, days int) error {
	if days == 0 {
		_, err := s.DB.ExecContext(ctx, "DELETE FROM proto_expiry WHERE tag=?", tag)
		return err
	}
	_, err := s.DB.ExecContext(ctx, "INSERT INTO proto_expiry(tag,expires_at) VALUES(?,?) ON CONFLICT(tag) DO UPDATE SET expires_at=excluded.expires_at", tag, time.Now().AddDate(0, 0, days).Unix())
	return err
}
func (s *Store) Manual(ctx context.Context) (map[string]bool, error) {
	var text string
	err := s.DB.QueryRowContext(ctx, "SELECT bytes FROM limits WHERE tag=?", ManualBlock).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	out := map[string]bool{}
	for _, t := range strings.Split(text, ",") {
		if t != "" {
			out[t] = true
		}
	}
	return out, err
}
func (s *Store) SetManual(ctx context.Context, tags map[string]bool) error {
	var keys []string
	for t := range tags {
		keys = append(keys, t)
	}
	slices.Sort(keys)
	_, err := s.DB.ExecContext(ctx, "INSERT INTO limits(tag,bytes) VALUES(?,?) ON CONFLICT(tag) DO UPDATE SET bytes=excluded.bytes", ManualBlock, strings.Join(keys, ","))
	return err
}
func (s *Store) Usage(ctx context.Context, tag string, start time.Time) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(bytes),0) FROM samples WHERE (tag=? OR tag IN (SELECT legacy_tag FROM user_legacy WHERE user_key=?)) AND ts>=?", tag, tag, start.Unix()).Scan(&n)
	return n, err
}
func (s *Store) UsageAll(ctx context.Context, start time.Time) (map[string]int64, error) {
	return s.UsageRange(ctx, start, time.Time{})
}
func (s *Store) UsageRange(ctx context.Context, start, end time.Time) (map[string]int64, error) {
	links, err := s.LegacyLinks(ctx)
	if err != nil {
		return nil, err
	}
	query := "SELECT tag,COALESCE(SUM(bytes),0) FROM samples WHERE ts>=?"
	args := []any{start.Unix()}
	if !end.IsZero() {
		query += " AND ts<?"
		args = append(args, end.Unix())
	}
	query += " GROUP BY tag ORDER BY 2 DESC"
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var tag string
		var n int64
		if err = rows.Scan(&tag, &n); err != nil {
			return nil, err
		}
		out[tag] = n
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Aggregate aliases after a single samples scan; old records remain available
	// individually, and reports hide adopted legacy rows to avoid double totals.
	for key, legacy := range links {
		out[key] += out[legacy]
	}
	return out, nil
}
func (s *Store) Log(ctx context.Context, actor, action, detail string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO opslog(ts,actor,action,detail) VALUES(?,?,?,?)", time.Now().Unix(), actor, action, detail)
	return err
}
func Day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
func Month(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()) }
func Human(n int64) string {
	v := float64(n)
	for _, u := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if v < 1024 || u == "TiB" {
			return fmt.Sprintf("%.2f %s", v, u)
		}
		v /= 1024
	}
	return ""
}
func (s *Store) Sample(ctx context.Context, payload Doc, d Doc) error {
	valid := map[string]bool{}
	for _, v := range accountingInbounds(d) {
		valid[str(obj(v)["tag"])] = true
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// One bounded transaction avoids one fsync per live connection.
	if _, err = tx.ExecContext(ctx, "CREATE TEMP TABLE IF NOT EXISTS current_connections(id TEXT PRIMARY KEY); DELETE FROM current_connections;"); err != nil {
		return err
	}
	for _, v := range list(payload["connections"]) {
		c := obj(v)
		tag := ConnectionAccounting(c, d)
		id := str(c["id"])
		if id == "" || !valid[tag] {
			continue
		}
		up, ok := c["uploadTotal"]
		if !ok {
			up = c["upload"]
		}
		down, ok := c["downloadTotal"]
		if !ok {
			down = c["download"]
		}
		total := integer(up) + integer(down)
		var oldTag string
		var oldTotal int64
		err = tx.QueryRowContext(ctx, "SELECT tag,total FROM connections WHERE id=?", id).Scan(&oldTag, &oldTotal)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		first := errors.Is(err, sql.ErrNoRows) || tag != oldTag || total < oldTotal
		if err == nil && tag == oldTag && total > oldTotal {
			if _, err = tx.ExecContext(ctx, "INSERT INTO samples VALUES(?,?,?)", time.Now().Unix(), oldTag, total-oldTotal); err != nil {
				return err
			}
		}
		if strings.HasPrefix(tag, userPrefix) && first && total > 0 {
			if _, err = tx.ExecContext(ctx, "INSERT INTO samples VALUES(?,?,?)", time.Now().Unix(), tag, total); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO connections(id,tag,total) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET tag=excluded.tag,total=excluded.total", id, tag, total); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO current_connections VALUES(?)", id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM connections WHERE id NOT IN (SELECT id FROM current_connections)"); err != nil {
		return err
	}
	return tx.Commit()
}
func JSONGet(ctx context.Context, client *http.Client, address string, headers map[string]string) (Doc, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, errors.New("invalid API endpoint")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("API transport failure")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API HTTP %d", resp.StatusCode)
	}
	var d Doc
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 32<<20))
	decoder.UseNumber()
	err = decoder.Decode(&d)
	return d, err
}

// AttachLegacy is an explicit opt-in operation after migration review. It keeps
// old records and limits intact, and counts both old and new clients together.
func (s *Store) AttachLegacy(ctx context.Context, key, legacy string) error {
	if !strings.HasPrefix(key, userPrefix) || legacy == "" || strings.HasPrefix(legacy, userPrefix) {
		return errors.New("历史映射无效")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO user_legacy(user_key,legacy_tag) VALUES(?,?)", key, legacy); err != nil {
		return errors.New("用户或旧入口已有关联，未覆盖")
	}
	for _, table := range []struct{ t, c string }{{"limits", "bytes"}, {"proto_expiry", "expires_at"}} {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.t+" WHERE tag=?", key).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return errors.New("新用户已有额度或到期设置，未覆盖")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO "+table.t+"(tag,"+table.c+") SELECT ?,"+table.c+" FROM "+table.t+" WHERE tag=?", key, legacy); err != nil {
			return err
		}
	}
	// Manual pause is inherited without changing the original old-client pause.
	var manual string
	err = tx.QueryRowContext(ctx, "SELECT bytes FROM limits WHERE tag=?", ManualBlock).Scan(&manual)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if slices.Contains(strings.Split(manual, ","), legacy) {
		if _, err = tx.ExecContext(ctx, "UPDATE limits SET bytes=? WHERE tag=?", strings.Trim(manual+","+key, ","), ManualBlock); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateUserPolicy(ctx context.Context, key, table string, value int64, apply func() error) error {
	column := "bytes"
	if table == "proto_expiry" {
		column = "expires_at"
	} else if table != "limits" {
		return errors.New("invalid policy table")
	}
	var old int64
	err := s.DB.QueryRowContext(ctx, "SELECT "+column+" FROM "+table+" WHERE tag=?", key).Scan(&old)
	existed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = s.DB.ExecContext(ctx, "INSERT INTO "+table+"(tag,"+column+") VALUES(?,?) ON CONFLICT(tag) DO UPDATE SET "+column+"=excluded."+column, key, value); err != nil {
		return err
	}
	if err = apply(); err != nil {
		recovery, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var rollback error
		if existed {
			_, rollback = s.DB.ExecContext(recovery, "UPDATE "+table+" SET "+column+"=? WHERE tag=?", old, key)
		} else {
			_, rollback = s.DB.ExecContext(recovery, "DELETE FROM "+table+" WHERE tag=?", key)
		}
		if rollback != nil {
			return fmt.Errorf("%v; policy rollback failed: %v", err, rollback)
		}
		return err
	}
	return nil
}
func (s *Store) AdoptLegacy(ctx context.Context, key, legacy string, apply func() error) error {
	manual, err := s.Manual(ctx)
	if err != nil {
		return err
	}
	if err = s.AttachLegacy(ctx, key, legacy); err != nil {
		return err
	}
	if err = apply(); err == nil {
		return nil
	}
	recovery, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, e := s.DB.BeginTx(recovery, nil)
	if e != nil {
		return fmt.Errorf("%v; history rollback failed: %v", err, e)
	}
	defer tx.Rollback()
	for _, q := range []string{"DELETE FROM user_legacy WHERE user_key=?", "DELETE FROM limits WHERE tag=?", "DELETE FROM proto_expiry WHERE tag=?"} {
		if _, e = tx.ExecContext(recovery, q, key); e != nil {
			return fmt.Errorf("%v; history rollback failed: %v", err, e)
		}
	}
	var keys []string
	for key := range manual {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if _, e = tx.ExecContext(recovery, "INSERT INTO limits(tag,bytes) VALUES(?,?) ON CONFLICT(tag) DO UPDATE SET bytes=excluded.bytes", ManualBlock, strings.Join(keys, ",")); e != nil {
		return fmt.Errorf("%v; history rollback failed: %v", err, e)
	}
	if e = tx.Commit(); e != nil {
		return fmt.Errorf("%v; history rollback failed: %v", err, e)
	}
	return err
}
func (s *Store) LegacyLinks(ctx context.Context) (map[string]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT user_key,legacy_tag FROM user_legacy")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, legacy string
		if err = rows.Scan(&key, &legacy); err != nil {
			return nil, err
		}
		out[key] = legacy
	}
	return out, rows.Err()
}
func (b *Bot) reportingInbounds(ctx context.Context, d Doc) ([]any, error) {
	links, err := b.Store.LegacyLinks(ctx)
	if err != nil {
		return nil, err
	}
	hidden := map[string]bool{}
	for key, legacy := range links {
		if _, ok := findUser(d, key); ok {
			hidden[legacy] = true
		}
	}
	var out []any
	for _, raw := range accountingInbounds(d) {
		if !hidden[str(obj(raw)["tag"])] {
			out = append(out, raw)
		}
	}
	return out, nil
}
