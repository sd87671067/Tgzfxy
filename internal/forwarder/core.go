package forwarder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/text/cases"
	"io"
	tg "local/bots/internal/telegram"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Button struct {
	Text, URL string
	Data      []byte
}
type Message struct {
	ID                           int
	Chat, Sender                 int64
	Text                         string
	Buttons                      []Button
	Links                        []string
	Media                        any
	Photo, WebPage, Service, Out bool
	Reply                        int
	Raw                          any
}
type Transport interface {
	Ready(context.Context) (bool, error)
	Resolve(context.Context, string) (int64, bool, error)
	History(context.Context, int64, int, bool) ([]Message, error)
	Message(context.Context, int64, int) (*Message, error)
	Forward(context.Context, int64, Message) ([]int, error)
	Send(context.Context, int64, string) ([]int, error)
	Download(context.Context, Message) (io.ReadCloser, string, error)
	Click(context.Context, Message, Button) (string, string, error)
	SendCode(context.Context, string) (string, error)
	SignIn(context.Context, string, string, string) error
	Password(context.Context, string) error
}
type Rule struct {
	ID, Owner, Source, Target              int64
	SourceLabel, TargetLabel, Mode, Filter string
	Words                                  []string
	Cursor                                 int
	Enabled                                bool
	RetryMinutes, RetryMax                 int
	Notify, ButtonLinks                    bool
	FailureWords, BlackWords, WhiteWords   []string
}
type deliveryKey struct {
	Rule int64
	Mid  int
}
type receiptKey struct {
	Target   int64
	Outgoing int
}
type Receipt struct {
	Key     deliveryKey
	Attempt int
}
type Delivery struct {
	Key         deliveryKey
	Text        string
	Attempts    int
	State       string
	Due, SentAt time.Time
}
type Engine struct {
	DB         *sql.DB
	Transport  Transport
	Bot        *tg.Client
	BotID      int64
	BotName    string
	Mu         sync.Mutex
	Queue      map[deliveryKey]*Delivery
	Receipts   map[receiptKey]Receipt
	Status     string
	States     map[int64]*State
	LoginOwner int64
	LoginUntil time.Time
}

const columns = "id,owner,source,target,source_label,target_label,mode,filter,words,cursor,enabled,retry_minutes,retry_max,success_notify,button_links,failure_words,black_words,white_words"

func Open(path string, t Transport, bot *tg.Client) (*Engine, error) {
	db, e := sql.Open("sqlite3", "file:"+url.PathEscape(path)+"?_journal_mode=WAL&_busy_timeout=5000")
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if e = db.Ping(); e != nil {
		db.Close()
		return nil, e
	}
	_, e = db.Exec(`CREATE TABLE IF NOT EXISTS rules(id INTEGER PRIMARY KEY AUTOINCREMENT, owner INTEGER,
source INTEGER,target INTEGER,source_label TEXT,target_label TEXT,mode TEXT DEFAULT 'poll',filter TEXT DEFAULT 'black',words TEXT DEFAULT '[]',cursor INTEGER DEFAULT 0,enabled INTEGER DEFAULT 1,retry_minutes INTEGER DEFAULT 5,retry_max INTEGER DEFAULT 6,failure_words TEXT DEFAULT '["分享生成 STRM 文件失败"]',black_words TEXT DEFAULT '[]',white_words TEXT DEFAULT '[]',success_notify INTEGER DEFAULT 0,button_links INTEGER DEFAULT 0);
CREATE TABLE IF NOT EXISTS logs(id INTEGER PRIMARY KEY,rule INTEGER,ts INTEGER,message TEXT);
CREATE TABLE IF NOT EXISTS sent(rule INTEGER,mid INTEGER,PRIMARY KEY(rule,mid));`)
	if e != nil {
		db.Close()
		return nil, e
	}
	return &Engine{DB: db, Transport: t, Bot: bot, Queue: map[deliveryKey]*Delivery{}, Receipts: map[receiptKey]Receipt{}, States: map[int64]*State{}, Status: "连接中"}, nil
}
func scan(row interface{ Scan(...any) error }) (Rule, error) {
	var r Rule
	var words, failure, black, white string
	e := row.Scan(&r.ID, &r.Owner, &r.Source, &r.Target, &r.SourceLabel, &r.TargetLabel, &r.Mode, &r.Filter, &words, &r.Cursor, &r.Enabled, &r.RetryMinutes, &r.RetryMax, &r.Notify, &r.ButtonLinks, &failure, &black, &white)
	if e != nil {
		return r, e
	}
	values := []string{words, failure, black, white}
	pointers := []*[]string{&r.Words, &r.FailureWords, &r.BlackWords, &r.WhiteWords}
	for i, text := range values {
		if e = json.Unmarshal([]byte(text), pointers[i]); e != nil {
			return r, e
		}
	}
	return r, nil
}
func (e *Engine) Rules(ctx context.Context) ([]Rule, error) {
	rows, err := e.DB.QueryContext(ctx, "SELECT "+columns+" FROM rules ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (e *Engine) Rule(ctx context.Context, id, owner int64) (Rule, error) {
	r, err := scan(e.DB.QueryRowContext(ctx, "SELECT "+columns+" FROM rules WHERE id=?", id))
	if err != nil || r.Owner != owner {
		return r, errors.New("规则不存在或无权操作")
	}
	return r, nil
}
func (e *Engine) log(ctx context.Context, rid int64, text string) {
	r := []rune(text)
	if len(r) > 500 {
		r = r[:500]
	}
	e.DB.ExecContext(ctx, "INSERT INTO logs(rule,ts,message) VALUES(?,?,?)", rid, time.Now().Unix(), string(r))
	e.DB.ExecContext(ctx, "DELETE FROM logs WHERE id NOT IN (SELECT id FROM logs ORDER BY id DESC LIMIT 1000)")
}
func unique(items []string) []string {
	out := []string{}
	for _, v := range items {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
func folded(s string) string {
	return cases.Fold().String(s)
}
func Accepted(text, mode string, words []string) bool {
	text = folded(text)
	hit := false
	for _, w := range words {
		if w != "" && strings.Contains(text, folded(w)) {
			hit = true
			break
		}
	}
	if mode == "white" {
		return hit
	}
	return !hit
}

var linkRE = regexp.MustCompile(`(?i)(?:https?://|tg://|www\.)[^\s<>"'，。！？]+`)
var expandButtonRE = regexp.MustCompile(`(?i)查看|展开|全文|内容|详情|阅读|read|more|view`)

func Links(text string) []string {
	out := linkRE.FindAllString(text, -1)
	for i, v := range out {
		out[i] = strings.TrimRight(v, ".,;!?)）]")
	}
	return unique(out)
}
func messageLinks(m Message) []string {
	out := append(Links(m.Text), m.Links...)
	for _, b := range m.Buttons {
		if b.URL != "" {
			out = append(out, b.URL)
		}
	}
	return unique(out)
}
func splitText(text string, n int) []string {
	r := []rune(text)
	var out []string
	for len(r) > 0 {
		end := min(n, len(r))
		out = append(out, string(r[:end]))
		r = r[end:]
	}
	return out
}
func (e *Engine) remove(k deliveryKey) {
	delete(e.Queue, k)
	for key, r := range e.Receipts {
		if r.Key == k {
			delete(e.Receipts, key)
		}
	}
}
func (e *Engine) prune() {
	now := time.Now()
	var items []*Delivery
	for k, d := range e.Queue {
		if now.Sub(d.SentAt) > 24*time.Hour {
			e.remove(k)
		} else {
			items = append(items, d)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SentAt.After(items[j].SentAt) })
	for i := 500; i < len(items); i++ {
		e.remove(items[i].Key)
	}
}
func (e *Engine) track(r Rule, mid int, text string, out []int) {
	if len(Links(text)) == 0 || len([]rune(text)) > 16000 || r.RetryMax == 0 {
		return
	}
	k := deliveryKey{r.ID, mid}
	d := e.Queue[k]
	if d == nil {
		d = &Delivery{Key: k, Text: text, State: "waiting", SentAt: time.Now()}
		e.Queue[k] = d
	}
	for _, id := range out {
		e.Receipts[receiptKey{r.Target, id}] = Receipt{k, d.Attempts}
	}
	e.prune()
}
func (e *Engine) notify(ctx context.Context, r Rule, mid int, retry bool) {
	if !r.Notify {
		return
	}
	word := "转发"
	if retry {
		word = "重试转发"
	}
	_, err := e.Bot.Send(ctx, r.Owner, fmt.Sprintf("✅ %s成功 · ID%d\n%s → %s\n源消息 ID：%d", word, r.ID, r.SourceLabel, r.TargetLabel, mid), nil, false)
	if err != nil {
		e.log(ctx, r.ID, "成功通知发送失败")
	}
}
func (e *Engine) expand(ctx context.Context, m Message, depth int, visited map[[2]int64]bool) (string, error) {
	key := [2]int64{m.Chat, int64(m.ID)}
	if depth >= 3 || visited[key] {
		return "", nil
	}
	visited[key] = true
	texts := []string{m.Text}
	for _, b := range m.Buttons {
		if b.URL != "" {
			u, err := url.Parse(b.URL)
			if err != nil {
				return "", err
			}
			p := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(p) > 0 && p[0] == "s" {
				p = p[1:]
			}
			var peer string
			var mid int
			if u.Hostname() == "t.me" || u.Hostname() == "telegram.me" {
				if len(p) == 2 {
					peer = "@" + p[0]
					fmt.Sscan(p[1], &mid)
				} else if len(p) == 3 && p[0] == "c" {
					peer = "-100" + p[1]
					fmt.Sscan(p[2], &mid)
				}
			}
			if peer != "" && mid > 0 {
				id, _, err := e.Transport.Resolve(ctx, peer)
				if err != nil {
					return "", err
				}
				linked, err := e.Transport.Message(ctx, id, mid)
				if err != nil {
					return "", err
				}
				if linked != nil {
					s, err := e.expand(ctx, *linked, depth+1, visited)
					if err != nil {
						return "", err
					}
					texts = append(texts, s)
				}
			} else {
				texts = append(texts, "[按钮链接] "+b.URL)
			}
		} else if len(b.Data) > 0 && expandButtonRE.MatchString(b.Text) {
			c, cancel := context.WithTimeout(ctx, 15*time.Second)
			text, link, err := e.Transport.Click(c, m, b)
			cancel()
			if err != nil {
				return "", err
			}
			texts = append(texts, text)
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
			fresh, err := e.Transport.Message(ctx, m.Chat, m.ID)
			if err != nil {
				return "", err
			}
			if fresh != nil && fresh.Text != m.Text {
				delete(visited, key)
				s, err := e.expand(ctx, *fresh, depth+1, visited)
				if err != nil {
					return "", err
				}
				texts = append(texts, s)
			}
			if link != "" {
				texts = append(texts, "[按钮返回链接] "+link)
			}
		}
	}
	return strings.Join(unique(texts), "\n\n"), nil
}
func (e *Engine) deliver(ctx context.Context, r Rule, m Message) error {
	if m.Service {
		return nil
	}
	text := m.Text
	var err error
	if r.ButtonLinks {
		text, err = e.expand(ctx, m, 0, map[[2]int64]bool{})
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(text) == "" && m.Media == nil {
		return nil
	}
	if !Accepted(text, r.Filter, r.Words) {
		e.log(ctx, r.ID, fmt.Sprintf("消息 %d：关键词过滤", m.ID))
		return nil
	}
	var exists int
	err = e.DB.QueryRowContext(ctx, "SELECT 1 FROM sent WHERE rule=? AND mid=?", r.ID, m.ID).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if r.Target == e.BotID {
		if m.Media != nil && !m.WebPage {
			file, name, err := e.Transport.Download(ctx, m)
			if err != nil {
				return err
			}
			method := "sendDocument"
			field := "document"
			if m.Photo {
				method = "sendPhoto"
				field = "photo"
			}
			err = e.Bot.Upload(ctx, r.Owner, method, field, name, file)
			file.Close()
			if err != nil {
				return err
			}
		}
		for _, chunk := range splitText(text, 4000) {
			if _, err = e.Bot.Send(ctx, r.Owner, chunk, nil, false); err != nil {
				return err
			}
		}
	} else {
		out, err := e.Transport.Forward(ctx, r.Target, m)
		if err != nil {
			return err
		}
		links := strings.Join(unique(append(messageLinks(m), Links(text)...)), "\n")
		e.track(r, m.ID, links, out)
		if text != m.Text {
			for _, chunk := range splitText(text, 4000) {
				out, err = e.Transport.Send(ctx, r.Target, chunk)
				if err != nil {
					return err
				}
				e.track(r, m.ID, links, out)
			}
		}
	}
	if _, err = e.DB.ExecContext(ctx, "INSERT OR IGNORE INTO sent VALUES(?,?)", r.ID, m.ID); err != nil {
		return err
	}
	e.log(ctx, r.ID, fmt.Sprintf("消息 %d：转发成功", m.ID))
	e.notify(ctx, r, m.ID, false)
	return nil
}
func normalize(text string) string {
	return folded(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text))
}
func failureMatches(text string, words []string) bool {
	text = normalize(text)
	for _, w := range words {
		w = normalize(w)
		if w != "" && strings.Contains(text, w) {
			return true
		}
	}
	return false
}
func (e *Engine) failure(ctx context.Context, m Message, rules []Rule) {
	e.prune()
	if m.Out || m.Sender != m.Chat {
		return
	}
	links := messageLinks(m)
	if len(links) == 0 || len([]rune(strings.Join(links, "\n"))) > 16000 {
		return
	}
	byID := map[int64]Rule{}
	hit := false
	for _, r := range rules {
		byID[r.ID] = r
		if r.Target == m.Chat && failureMatches(m.Text, r.FailureWords) {
			hit = true
		}
	}
	if !hit {
		return
	}
	var candidates []*Delivery
	if m.Reply > 0 {
		if receipt, ok := e.Receipts[receiptKey{m.Chat, m.Reply}]; ok {
			if d := e.Queue[receipt.Key]; d != nil && d.Attempts == receipt.Attempt && (d.State == "waiting" || d.State == "queued") {
				candidates = append(candidates, d)
			}
		}
	} else {
		for _, d := range e.Queue {
			if byID[d.Key.Rule].Target == m.Chat && d.State == "waiting" {
				candidates = append(candidates, d)
			}
		}
		if len(candidates) > 1 {
			candidates = slices.DeleteFunc(candidates, func(d *Delivery) bool {
				for _, link := range Links(d.Text) {
					if slices.Contains(links, link) {
						return false
					}
				}
				return true
			})
		}
	}
	if len(candidates) != 1 {
		e.log(ctx, 0, "失败回复无法唯一关联，请检查目标机器人是否引用原消息")
		return
	}
	d := candidates[0]
	r := byID[d.Key.Rule]
	if !r.Enabled || d.State != "waiting" || !failureMatches(m.Text, r.FailureWords) {
		return
	}
	if d.Attempts >= r.RetryMax {
		e.remove(d.Key)
		return
	}
	d.State = "queued"
	d.Due = time.Now().Add(time.Duration(r.RetryMinutes) * time.Minute)
	d.Text = strings.Join(links, "\n")
	e.log(ctx, r.ID, fmt.Sprintf("消息 %d：目标回复命中失败关键词，加入重试队列", d.Key.Mid))
}
func (e *Engine) retry(ctx context.Context, rules []Rule) {
	e.prune()
	byID := map[int64]Rule{}
	for _, r := range rules {
		byID[r.ID] = r
	}
	for k, d := range e.Queue {
		r, ok := byID[k.Rule]
		if !ok || !r.Enabled || d.State != "queued" || d.Due.After(time.Now()) {
			continue
		}
		if d.Attempts >= r.RetryMax {
			e.remove(k)
			continue
		}
		d.Attempts++
		d.SentAt = time.Now()
		d.Due = d.SentAt.Add(time.Duration(r.RetryMinutes) * time.Minute)
		for key, v := range e.Receipts {
			if v.Key == k {
				delete(e.Receipts, key)
			}
		}
		var err error
		for _, part := range splitText(d.Text, 4000) {
			var out []int
			out, err = e.Transport.Send(ctx, r.Target, part)
			if err != nil {
				break
			}
			e.track(r, k.Mid, d.Text, out)
		}
		if err == nil {
			d.State = "waiting"
			e.notify(ctx, r, k.Mid, true)
			e.log(ctx, r.ID, fmt.Sprintf("消息 %d：第 %d 次重试已发送", k.Mid, d.Attempts))
		} else {
			if d.Attempts >= r.RetryMax {
				e.remove(k)
			}
			var fw interface{ RetryAfter() time.Duration }
			if errors.As(err, &fw) {
				d.Due = time.Now().Add(max(fw.RetryAfter(), time.Duration(r.RetryMinutes)*time.Minute))
			}
			e.log(ctx, r.ID, "重试发送失败")
		}
	}
}
func (e *Engine) drain(ctx context.Context, r Rule) error {
	messages, err := e.Transport.History(ctx, r.Source, r.Cursor, false)
	if err != nil {
		return err
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].ID < messages[j].ID })
	for _, m := range messages {
		if m.ID <= r.Cursor {
			continue
		}
		if err = e.deliver(ctx, r, m); err != nil {
			return err
		}
		if _, err = e.DB.ExecContext(ctx, "UPDATE rules SET cursor=? WHERE id=?", m.ID, r.ID); err != nil {
			return err
		}
		if _, err = e.DB.ExecContext(ctx, "DELETE FROM sent WHERE rule=? AND mid<=?", r.ID, m.ID); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) Tick(ctx context.Context) error {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	ready, err := e.Transport.Ready(ctx)
	if err != nil {
		return err
	}
	if !ready {
		e.Status = "尚未登录，请点击转发助手登录"
		return nil
	}
	e.Status = "用户会话已连接"
	rules, err := e.Rules(ctx)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.Enabled {
			if err = e.drain(ctx, r); err != nil {
				e.log(ctx, r.ID, "转发失败，将重试")
				return err
			}
		}
	}
	e.retry(ctx, rules)
	return nil
}
func (e *Engine) Event(ctx context.Context, m Message) error {
	e.Mu.Lock()
	defer e.Mu.Unlock()
	rules, err := e.Rules(ctx)
	if err != nil {
		return err
	}
	e.failure(ctx, m, rules)
	for _, r := range rules {
		if r.Enabled && r.Mode == "live" && r.Source == m.Chat {
			if err = e.drain(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}
