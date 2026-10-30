package sbbot

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Maintenance(ctx context.Context, dir string) error {
	cutoff := time.Now().Add(-24 * time.Hour)
	for file, table := range map[string]string{"sbbot.db": "opslog", "forwarder.db": "logs"} {
		path := filepath.Join(dir, file)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		db, err := sql.Open("sqlite3", path+"?_busy_timeout=30000")
		if err != nil {
			return err
		}
		_, err = db.ExecContext(ctx, "DELETE FROM "+table+" WHERE ts < ?", cutoff.Unix())
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
