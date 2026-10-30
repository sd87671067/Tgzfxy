CREATE TABLE rules(id INTEGER PRIMARY KEY AUTOINCREMENT, owner INTEGER,
          source INTEGER, target INTEGER, source_label TEXT, target_label TEXT,
          mode TEXT DEFAULT 'poll', filter TEXT DEFAULT 'black', words TEXT DEFAULT '[]',
          cursor INTEGER DEFAULT 0, enabled INTEGER DEFAULT 1, retry_minutes INTEGER DEFAULT 5, retry_max INTEGER DEFAULT 6, failure_words TEXT DEFAULT '["分享生成 STRM 文件失败"]', black_words TEXT DEFAULT '[]', white_words TEXT DEFAULT '[]', success_notify INTEGER DEFAULT 0, button_links INTEGER DEFAULT 0);
CREATE TABLE logs(id INTEGER PRIMARY KEY, rule INTEGER, ts INTEGER, message TEXT);
CREATE TABLE sent(rule INTEGER, mid INTEGER, PRIMARY KEY(rule,mid));
