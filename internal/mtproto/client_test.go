package mtproto

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTelethonSessionConversion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "legacy.session")
	dest := filepath.Join(dir, "go.json")
	db, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 256)
	for i := range key {
		key[i] = byte(i)
	}
	_, err = db.Exec("CREATE TABLE sessions(dc_id INTEGER,server_address TEXT,port INTEGER,auth_key BLOB); INSERT INTO sessions VALUES(2,'149.154.167.51',443,?)", key)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = ConvertSession(ctx, source, dest); err != nil {
		t.Fatal(err)
	}
	data, err := (&session.Loader{Storage: atomicStorage{dest}}).Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha1.Sum(key)
	if data.DC != 2 || data.Addr != "149.154.167.51:443" || !reflect.DeepEqual(data.AuthKey, key) || !reflect.DeepEqual(data.AuthKeyID, hash[12:]) {
		t.Fatal("session conversion mismatch")
	}
	after, _ := os.ReadFile(source)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("legacy session modified")
	}
	info, _ := os.Stat(dest)
	if info.Mode().Perm() != 0600 {
		t.Fatal("session permissions")
	}
	if err = ConvertSession(ctx, source, dest); err != nil {
		t.Fatal(err)
	}
}
func TestMessageConversion(t *testing.T) {
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, FromID: &tg.PeerUser{UserID: 222}, Message: "hello", Entities: []tg.MessageEntityClass{&tg.MessageEntityTextURL{Offset: 0, Length: 5, URL: "https://example.org"}}, ReplyTo: &tg.MessageReplyHeader{ReplyToMsgID: 3}, ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "link", URL: "https://example.com"}, &tg.KeyboardButtonCallback{Text: "查看", Data: []byte("payload")}}}}}}
	m, ok := convert(msg)
	if !ok || m.Chat != -1000000000123 || m.Sender != 222 || m.Reply != 3 || len(m.Links) != 1 || len(m.Buttons) != 2 {
		t.Fatalf("message conversion mismatch: %#v", m)
	}
}
