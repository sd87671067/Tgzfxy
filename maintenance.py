"""Daily bounded cleanup: preserve rules, sessions, cursors and billing data."""
import sqlite3
import time
from pathlib import Path
p=Path('/data/forwarder.db')
if p.exists():
    db=sqlite3.connect(p,timeout=30)
    db.execute('DELETE FROM logs WHERE ts < ?', (int(time.time())-86400,))
    db.commit()
    db.execute('PRAGMA wal_checkpoint(TRUNCATE)')
    db.execute('VACUUM')
    db.close()
p=Path('/data/sbbot.db')
if p.exists():
    db=sqlite3.connect(p,timeout=30)
    db.execute('DELETE FROM opslog WHERE ts < ?', (int(time.time())-86400,))
    db.commit()
    db.execute('PRAGMA wal_checkpoint(PASSIVE)')
    db.close()
# Only disposable cache directories owned by this feature, never session files.
for root in [Path('/data/forwarder-cache'),Path('/app/__pycache__')]:
    if root.exists():
        for f in root.rglob('*'):
            if f.is_file() and not f.is_symlink() and f.stat().st_mtime < time.time()-86400:
                f.unlink()
print('sbbot maintenance completed')
