package mtproto

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	_ "github.com/mattn/go-sqlite3"
	"io"
	"local/bots/internal/forwarder"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type atomicStorage struct{ Path string }

func (s atomicStorage) LoadSession(ctx context.Context) ([]byte, error) {
	b, err := os.ReadFile(s.Path)
	if os.IsNotExist(err) {
		return nil, session.ErrNotFound
	}
	return b, err
}
func (s atomicStorage) StoreSession(ctx context.Context, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".session-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(b); err != nil {
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
	return os.Rename(name, s.Path)
}
func ConvertSession(ctx context.Context, source, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	db, err := sql.Open("sqlite3", "file:"+url.PathEscape(source)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var dc, port int
	var address string
	var key []byte
	err = db.QueryRowContext(ctx, "SELECT dc_id,server_address,port,auth_key FROM sessions LIMIT 1").Scan(&dc, &address, &port, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(key) == 0 {
		return nil
	}
	if len(key) != 256 {
		return errors.New("invalid Telethon authorization key size")
	}
	hash := sha1.Sum(key)
	loader := session.Loader{Storage: atomicStorage{destination}}
	return loader.Save(ctx, &session.Data{DC: dc, Addr: net.JoinHostPort(address, strconv.Itoa(port)), AuthKey: key, AuthKeyID: hash[12:]})
}

type peer struct {
	ID, Hash       int64
	Kind, Username string
	Left           bool
}
type Client struct {
	starter   func(context.Context) error
	connected chan struct{}
	factory   func() *telegram.Client
	Client    *telegram.Client
	api       *tg.Client
	Connected atomic.Bool
	mu        sync.Mutex
	peers     map[int64]peer
	peerPath  string
	Events    chan forwarder.Message
	Self      atomic.Int64
}

func New(ctx context.Context, id int, hash, dir string) (*Client, error) {
	c := &Client{connected: make(chan struct{}), peers: map[int64]peer{}, peerPath: filepath.Join(dir, "forwarder-go-peers.json"), Events: make(chan forwarder.Message, 128)}
	raw, err := os.ReadFile(c.peerPath)
	if err == nil {
		if err = json.Unmarshal(raw, &c.peers); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	legacy := filepath.Join(dir, "forwarder-user.session")
	if _, err = os.Stat(legacy); err == nil {
		if err = ConvertSession(ctx, legacy, filepath.Join(dir, "forwarder-go-session.json")); err != nil {
			return nil, err
		}
		db, err := sql.Open("sqlite3", "file:"+url.PathEscape(legacy)+"?mode=ro")
		if err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, "SELECT id,hash,COALESCE(username,'') FROM entities")
		if err != nil {
			db.Close()
			return nil, err
		}
		for rows.Next() {
			var p peer
			if err = rows.Scan(&p.ID, &p.Hash, &p.Username); err != nil {
				rows.Close()
				db.Close()
				return nil, err
			}
			p.Kind = "user"
			if p.ID <= -1000000000000 {
				p.Kind = "channel"
			} else if p.ID < 0 {
				p.Kind = "chat"
			}
			if _, exists := c.peers[p.ID]; !exists {
				c.peers[p.ID] = p
			}
		}
		err = rows.Err()
		rows.Close()
		db.Close()
		if err != nil {
			return nil, err
		}
	}
	dispatcher := tg.NewUpdateDispatcher()
	emit := func(ctx context.Context, msg tg.MessageClass) error {
		m, ok := convert(msg)
		if !ok {
			return nil
		}
		select {
		case c.Events <- m:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	dispatcher.OnNewMessage(func(ctx context.Context, en tg.Entities, u *tg.UpdateNewMessage) error {
		c.entities(en.Users, en.Chats, en.Channels)
		return emit(ctx, u.Message)
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, en tg.Entities, u *tg.UpdateNewChannelMessage) error {
		c.entities(en.Users, en.Chats, en.Channels)
		return emit(ctx, u.Message)
	})
	dispatcher.OnEditMessage(func(ctx context.Context, en tg.Entities, u *tg.UpdateEditMessage) error {
		c.entities(en.Users, en.Chats, en.Channels)
		return emit(ctx, u.Message)
	})
	dispatcher.OnEditChannelMessage(func(ctx context.Context, en tg.Entities, u *tg.UpdateEditChannelMessage) error {
		c.entities(en.Users, en.Chats, en.Channels)
		return emit(ctx, u.Message)
	})
	c.factory = func() *telegram.Client {
		return telegram.NewClient(id, hash, telegram.Options{SessionStorage: atomicStorage{filepath.Join(dir, "forwarder-go-session.json")}, UpdateHandler: dispatcher})
	}
	c.Client = c.factory()
	c.api = c.Client.API()
	return c, nil
}
func (c *Client) apiClient() *tg.Client            { c.mu.Lock(); defer c.mu.Unlock(); return c.api }
func (c *Client) telegramClient() *telegram.Client { c.mu.Lock(); defer c.mu.Unlock(); return c.Client }
func (c *Client) SetStarter(start func(context.Context) error) {
	c.mu.Lock()
	c.starter = start
	c.mu.Unlock()
}
func (c *Client) Run(ctx context.Context) error {
	// gotd clients cannot Run again after cancellation. Recreate the network
	// client while retaining the same atomic session store, peer cache and events.
	c.mu.Lock()
	c.Client = c.factory()
	c.api = c.Client.API()
	client := c.Client
	c.mu.Unlock()
	return client.Run(ctx, func(ctx context.Context) error {
		c.mu.Lock()
		c.Connected.Store(true)
		close(c.connected)
		c.mu.Unlock()
		defer func() { c.mu.Lock(); c.Connected.Store(false); c.connected = make(chan struct{}); c.mu.Unlock() }()
		<-ctx.Done()
		return ctx.Err()
	})
}

// Probe verifies the migrated session without subscribing to application events.
func (c *Client) Probe(ctx context.Context) error {
	return c.Client.Run(ctx, func(ctx context.Context) error {
		status, err := c.telegramClient().Auth().Status(ctx)
		if err != nil {
			return safe(err)
		}
		if !status.Authorized {
			return errors.New("Telegram user session is not authorized")
		}
		return nil
	})
}
func (c *Client) entities(users map[int64]*tg.User, chats map[int64]*tg.Chat, channels map[int64]*tg.Channel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, u := range users {
		if !u.Min {
			c.peers[id] = peer{ID: id, Hash: u.AccessHash, Kind: "user", Username: u.Username}
		}
	}
	for id := range chats {
		c.peers[-id] = peer{ID: -id, Kind: "chat"}
	}
	for id, u := range channels {
		if !u.Min {
			c.peers[-1000000000000-id] = peer{ID: -1000000000000 - id, Hash: u.AccessHash, Kind: "channel", Username: u.Username, Left: u.Left}
		}
	}
}
func (c *Client) remember(users []tg.UserClass, chats []tg.ChatClass) {
	us := map[int64]*tg.User{}
	cs := map[int64]*tg.Chat{}
	chs := map[int64]*tg.Channel{}
	for _, v := range users {
		if u, ok := v.(*tg.User); ok {
			us[u.ID] = u
		}
	}
	for _, v := range chats {
		switch u := v.(type) {
		case *tg.Chat:
			cs[u.ID] = u
		case *tg.Channel:
			chs[u.ID] = u
		}
	}
	c.entities(us, cs, chs)
}
func (c *Client) savePeers(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(c.peers)
	if err != nil {
		return err
	}
	return (atomicStorage{c.peerPath}).StoreSession(ctx, b)
}
func peerID(p tg.PeerClass) int64 {
	switch v := p.(type) {
	case *tg.PeerUser:
		return v.UserID
	case *tg.PeerChat:
		return -v.ChatID
	case *tg.PeerChannel:
		return -1000000000000 - v.ChannelID
	}
	return 0
}
func (c *Client) input(id int64) (tg.InputPeerClass, error) {
	c.mu.Lock()
	p, ok := c.peers[id]
	c.mu.Unlock()
	if !ok {
		if id < 0 && id > -1000000000000 {
			return &tg.InputPeerChat{ChatID: -id}, nil
		}
		return nil, errors.New("聊天 ID 未缓存，请使用 @用户名或群邀请链接重新绑定")
	}
	switch p.Kind {
	case "channel":
		return &tg.InputPeerChannel{ChannelID: -1000000000000 - id, AccessHash: p.Hash}, nil
	case "chat":
		return &tg.InputPeerChat{ChatID: -id}, nil
	default:
		return &tg.InputPeerUser{UserID: id, AccessHash: p.Hash}, nil
	}
}
func (c *Client) Ready(ctx context.Context) (bool, error) {
	if !c.Connected.Load() {
		c.mu.Lock()
		start := c.starter
		ready := c.connected
		c.mu.Unlock()
		if start == nil {
			return false, errors.New("Telegram 连接尚未就绪")
		}
		if err := start(ctx); err != nil {
			return false, err
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	s, err := c.telegramClient().Auth().Status(ctx)
	if err != nil {
		return false, safe(err)
	}
	if s.User != nil {
		c.Self.Store(s.User.ID)
	}
	return s.Authorized, nil
}
func (c *Client) Resolve(ctx context.Context, value string) (int64, bool, error) {
	value = strings.TrimSpace(value)
	if id, err := strconv.ParseInt(value, 10, 64); err == nil {
		_, err = c.input(id)
		c.mu.Lock()
		p := c.peers[id]
		c.mu.Unlock()
		return id, p.Left, err
	}
	domain := ""
	if strings.HasPrefix(value, "@") {
		domain = strings.TrimPrefix(value, "@")
	} else {
		if !strings.Contains(value, "://") {
			value = "https://" + value
		}
		u, err := url.Parse(value)
		if err != nil {
			return 0, false, errors.New("Telegram 链接无效")
		}
		if u.Hostname() != "t.me" && u.Hostname() != "telegram.me" && u.Hostname() != "www.t.me" {
			return 0, false, errors.New("请使用 Telegram 链接、@用户名或数字聊天 ID")
		}
		path := strings.Trim(u.Path, "/")
		if strings.HasPrefix(path, "+") || strings.HasPrefix(path, "joinchat/") {
			hash := strings.TrimPrefix(strings.TrimPrefix(path, "+"), "joinchat/")
			invite, err := c.apiClient().MessagesCheckChatInvite(ctx, hash)
			if err != nil {
				return 0, false, safe(err)
			}
			joined, ok := invite.(*tg.ChatInviteAlready)
			if !ok {
				return 0, false, errors.New("请先使用配置的 Telegram 用户账号加入该私密群/频道")
			}
			c.remember(nil, []tg.ChatClass{joined.Chat})
			if err = c.savePeers(ctx); err != nil {
				return 0, false, err
			}
			switch chat := joined.Chat.(type) {
			case *tg.Channel:
				return -1000000000000 - chat.ID, chat.Left, nil
			case *tg.Chat:
				return -chat.ID, chat.Left, nil
			}
			return 0, false, errors.New("群类型不支持")
		}
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[0] == "c" {
			return c.Resolve(ctx, "-100"+parts[1])
		}
		if len(parts) != 1 {
			return 0, false, errors.New("规则不能使用单条消息链接")
		}
		domain = path
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(domain) {
		return 0, false, errors.New("Telegram 用户名无效")
	}
	resolved, err := c.apiClient().ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: domain})
	if err != nil {
		return 0, false, safe(err)
	}
	c.remember(resolved.Users, resolved.Chats)
	id := peerID(resolved.Peer)
	c.mu.Lock()
	p := c.peers[id]
	c.mu.Unlock()
	return id, p.Left, c.savePeers(ctx)
}
func convert(raw tg.MessageClass) (forwarder.Message, bool) {
	m := forwarder.Message{Raw: raw}
	switch v := raw.(type) {
	case *tg.Message:
		m.ID = v.ID
		m.Chat = peerID(v.PeerID)
		m.Sender = peerID(v.FromID)
		if m.Sender == 0 && !v.Out {
			m.Sender = m.Chat
		}
		m.Text = v.Message
		m.Out = v.Out
		m.Media = v.Media
		if _, ok := v.Media.(*tg.MessageMediaEmpty); ok {
			m.Media = nil
		}
		_, m.Photo = v.Media.(*tg.MessageMediaPhoto)
		_, m.WebPage = v.Media.(*tg.MessageMediaWebPage)
		if r, ok := v.ReplyTo.(*tg.MessageReplyHeader); ok {
			m.Reply = r.ReplyToMsgID
		}
		for _, en := range v.Entities {
			if link, ok := en.(*tg.MessageEntityTextURL); ok {
				m.Links = append(m.Links, link.URL)
			}
		}
		if k, ok := v.ReplyMarkup.(*tg.ReplyInlineMarkup); ok {
			for _, row := range k.Rows {
				for _, b := range row.Buttons {
					switch button := b.(type) {
					case *tg.KeyboardButtonURL:
						m.Buttons = append(m.Buttons, forwarder.Button{Text: button.Text, URL: button.URL})
					case *tg.KeyboardButtonCallback:
						m.Buttons = append(m.Buttons, forwarder.Button{Text: button.Text, Data: button.Data})
					}
				}
			}
		}
		return m, true
	case *tg.MessageService:
		m.ID = v.ID
		m.Chat = peerID(v.PeerID)
		m.Service = true
		return m, true
	}
	return m, false
}
func (c *Client) messages(result tg.MessagesMessagesClass) []forwarder.Message {
	v, ok := result.AsModified()
	if !ok {
		return nil
	}
	c.remember(v.GetUsers(), v.GetChats())
	var out []forwarder.Message
	for _, raw := range v.GetMessages() {
		if m, ok := convert(raw); ok {
			out = append(out, m)
		}
	}
	return out
}
func (c *Client) History(ctx context.Context, id int64, cursor int, latest bool) ([]forwarder.Message, error) {
	p, err := c.input(id)
	if err != nil {
		return nil, err
	}
	req := &tg.MessagesGetHistoryRequest{Peer: p, Limit: 100, OffsetID: cursor + 1, AddOffset: -100, MinID: cursor}
	if latest {
		req = &tg.MessagesGetHistoryRequest{Peer: p, Limit: 1}
	}
	result, err := c.apiClient().MessagesGetHistory(ctx, req)
	if err != nil {
		return nil, safe(err)
	}
	out := c.messages(result)
	if !latest {
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	}
	return out, nil
}
func (c *Client) Message(ctx context.Context, chat int64, id int) (*forwarder.Message, error) {
	p, err := c.input(chat)
	if err != nil {
		return nil, err
	}
	var result tg.MessagesMessagesClass
	if channel, ok := p.(*tg.InputPeerChannel); ok {
		result, err = c.apiClient().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash}, ID: []tg.InputMessageClass{&tg.InputMessageID{ID: id}}})
	} else {
		result, err = c.apiClient().MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: id}})
	}
	if err != nil {
		return nil, safe(err)
	}
	messages := c.messages(result)
	for _, m := range messages {
		if m.ID == id && m.Chat == chat {
			return &m, nil
		}
	}
	return nil, nil
}
func randomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}
func ids(result tg.UpdatesClass) []int {
	var out []int
	var updates []tg.UpdateClass
	switch v := result.(type) {
	case *tg.Updates:
		updates = v.Updates
	case *tg.UpdatesCombined:
		updates = v.Updates
	case *tg.UpdateShortSentMessage:
		return []int{v.ID}
	case *tg.UpdateShort:
		updates = []tg.UpdateClass{v.Update}
	}
	for _, v := range updates {
		switch u := v.(type) {
		case *tg.UpdateMessageID:
			out = append(out, u.ID)
		case *tg.UpdateNewMessage:
			if m, ok := convert(u.Message); ok {
				out = append(out, m.ID)
			}
		case *tg.UpdateNewChannelMessage:
			if m, ok := convert(u.Message); ok {
				out = append(out, m.ID)
			}
		}
	}
	return out
}
func (c *Client) Forward(ctx context.Context, target int64, m forwarder.Message) ([]int, error) {
	from, err := c.input(m.Chat)
	if err != nil {
		return nil, err
	}
	to, err := c.input(target)
	if err != nil {
		return nil, err
	}
	result, err := c.apiClient().MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{FromPeer: from, ToPeer: to, ID: []int{m.ID}, RandomID: []int64{randomID()}})
	if err != nil {
		return nil, safe(err)
	}
	return ids(result), nil
}
func (c *Client) Send(ctx context.Context, target int64, text string) ([]int, error) {
	p, err := c.input(target)
	if err != nil {
		return nil, err
	}
	result, err := c.apiClient().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: p, Message: text, RandomID: randomID()})
	if err != nil {
		return nil, safe(err)
	}
	return ids(result), nil
}
func (c *Client) Click(ctx context.Context, m forwarder.Message, b forwarder.Button) (string, string, error) {
	p, err := c.input(m.Chat)
	if err != nil {
		return "", "", err
	}
	result, err := c.apiClient().MessagesGetBotCallbackAnswer(ctx, &tg.MessagesGetBotCallbackAnswerRequest{Peer: p, MsgID: m.ID, Data: b.Data})
	if err != nil {
		return "", "", safe(err)
	}
	return result.Message, result.URL, nil
}

type tempFile struct{ *os.File }

func (t tempFile) Close() error { name := t.Name(); err := t.File.Close(); os.Remove(name); return err }
func (c *Client) Download(ctx context.Context, m forwarder.Message) (io.ReadCloser, string, error) {
	var loc tg.InputFileLocationClass
	name := "media"
	dc := 0
	switch v := m.Media.(type) {
	case *tg.MessageMediaPhoto:
		p, ok := v.Photo.(*tg.Photo)
		if !ok {
			return nil, "", errors.New("photo unavailable")
		}
		dc = p.DCID
		size := ""
		for _, s := range p.Sizes {
			if v, ok := s.(interface{ GetType() string }); ok {
				size = v.GetType()
			}
		}
		loc = &tg.InputPhotoFileLocation{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference, ThumbSize: size}
		name = "photo.jpg"
	case *tg.MessageMediaDocument:
		d, ok := v.Document.(*tg.Document)
		if !ok {
			return nil, "", errors.New("document unavailable")
		}
		dc = d.DCID
		loc = &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}
		for _, a := range d.Attributes {
			if f, ok := a.(*tg.DocumentAttributeFilename); ok {
				name = filepath.Base(f.FileName)
			}
		}
	default:
		return nil, "", errors.New("unsupported downloadable media")
	}
	file, err := os.CreateTemp("", "sbbot-media-*")
	if err != nil {
		return nil, "", err
	}
	out := tempFile{file}
	var rpc downloader.Client = c.apiClient()
	var closer io.Closer
	if dc != 0 {
		pool, err := c.telegramClient().DC(ctx, dc, 1)
		if err != nil {
			out.Close()
			return nil, "", safe(err)
		}
		rpc = tg.NewClient(pool)
		closer = pool
	}
	if closer != nil {
		defer closer.Close()
	}
	_, err = downloader.NewDownloader().Download(rpc, loc).Stream(ctx, file)
	if err != nil {
		out.Close()
		return nil, "", safe(err)
	}
	if _, err = file.Seek(0, 0); err != nil {
		out.Close()
		return nil, "", err
	}
	return out, name, nil
}
func (c *Client) SendCode(ctx context.Context, phone string) (string, error) {
	v, err := c.telegramClient().Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return "", safe(err)
	}
	if sent, ok := v.(*tg.AuthSentCode); ok {
		return sent.PhoneCodeHash, nil
	}
	return "", errors.New("login code not required or unsupported delivery")
}
func (c *Client) SignIn(ctx context.Context, phone, code, hash string) error {
	_, err := c.telegramClient().Auth().SignIn(ctx, phone, code, hash)
	if errors.Is(err, auth.ErrPasswordAuthNeeded) {
		return forwarder.ErrPasswordNeeded
	}
	return safe(err)
}
func (c *Client) Password(ctx context.Context, password string) error {
	_, err := c.telegramClient().Auth().Password(ctx, password)
	return safe(err)
}

type rpcError struct {
	Kind string
	Wait time.Duration
}

func (e rpcError) Error() string {
	if e.Wait > 0 {
		return fmt.Sprintf("请求过于频繁，请等待 %d 秒后重试", int(e.Wait.Seconds()))
	}
	return "Telegram 请求失败：" + e.Kind
}
func (e rpcError) RetryAfter() time.Duration { return e.Wait }
func safe(err error) error {
	if err == nil {
		return nil
	}
	if duration, ok := tgerr.AsFloodWait(err); ok {
		return rpcError{Kind: "FLOOD_WAIT", Wait: duration}
	}
	if rpc, ok := tgerr.As(err); ok {
		return rpcError{Kind: rpc.Type}
	}
	if errors.Is(err, auth.ErrPasswordInvalid) {
		return rpcError{Kind: "PASSWORD_INVALID"}
	}
	return errors.New("Telegram transport failure")
}
