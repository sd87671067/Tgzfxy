package sbbot

import (
	"context"
	"database/sql"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Maintenance(ctx context.Context, dir string) error {
	cutoff := time.Now().Add(-24 * time.Hour)
	for file, table := range map[string]string{"sbbot.db": "opslog", "forwarder.db": "logs"} {
		path := filepath.Join(dir, file)
		if info, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		} else if !info.Mode().IsRegular() {
			continue
		}
		db, err := sql.Open("sqlite3", "file:"+url.PathEscape(path)+"?_busy_timeout=1000")
		if err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		// Bound write lock duration; the next maintenance run continues any backlog.
		for batch := 0; batch < 10; batch++ {
			var result sql.Result
			result, err = db.ExecContext(ctx, "DELETE FROM "+table+" WHERE id IN (SELECT id FROM "+table+" WHERE ts < ? LIMIT 500)", cutoff.Unix())
			if err != nil {
				break
			}
			n, e := result.RowsAffected()
			if e != nil {
				err = e
				break
			}
			if n < 500 {
				break
			}
		}
		if err == nil {
			_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
		}
		db.Close()
		if err != nil {
			return err
		}
	}
	for _, root := range []string{filepath.Join(dir, "forwarder-cache"), filepath.Join(dir, "logs")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if filepath.Base(root) == "logs" && !strings.HasSuffix(d.Name(), ".log.gz") && !strings.Contains(d.Name(), ".log.") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
				return os.Remove(path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
