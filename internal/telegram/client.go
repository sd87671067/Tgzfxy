package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type Message struct {
	ID   int64  `json:"message_id"`
	From User   `json:"from"`
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
}
type Callback struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}
type Update struct {
	ID       int64     `json:"update_id"`
	Message  *Message  `json:"message"`
	Callback *Callback `json:"callback_query"`
}
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
}
type Keyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

func B(text, data string) Button   { return Button{Text: text, Data: data} }
func K(rows ...[]Button) *Keyboard { return &Keyboard{Rows: rows} }

type APIError struct {
	Code        int
	RetryAfter  int
	Description string
}

func (e *APIError) Error() string { return fmt.Sprintf("Telegram API error %d", e.Code) }

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(token string) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 4
	return &Client{BaseURL: "https://api.telegram.org/bot" + token + "/", HTTP: &http.Client{Timeout: 65 * time.Second, Transport: tr}}
}
func (c *Client) Call(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+method, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Telegram endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Telegram transport failure")
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Params      struct {
			Retry int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&envelope); err != nil {
		return errors.New("invalid Telegram response")
	}
	if !envelope.OK {
		return &APIError{envelope.Code, envelope.Params.Retry, envelope.Description}
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}
func (c *Client) Send(ctx context.Context, chat int64, text string, k *Keyboard, html bool) (Message, error) {
	p := map[string]any{"chat_id": chat, "text": text}
	if k != nil {
		p["reply_markup"] = k
	}
	if html {
		p["parse_mode"] = "HTML"
	}
	var m Message
	err := c.Call(ctx, "sendMessage", p, &m)
	return m, err
}
func (c *Client) Edit(ctx context.Context, chat, mid int64, text string, k *Keyboard, html bool) error {
	p := map[string]any{"chat_id": chat, "message_id": mid, "text": text}
	if k != nil {
		p["reply_markup"] = k
	}
	if html {
		p["parse_mode"] = "HTML"
	}
	err := c.Call(ctx, "editMessageText", p, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && strings.Contains(apiErr.Description, "message is not modified") {
		return nil
	}
	return err
}
func (c *Client) Answer(ctx context.Context, id, text string) error {
	return c.Call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text, "show_alert": text != ""}, nil)
}

// Poll advances the offset only after the application has handled an update.
// Transient polling failures back off; shutdown cancels the active HTTP request.
func (c *Client) Poll(ctx context.Context, handle func(context.Context, Update) error, report func(error)) error {
	var offset int64
	delay := time.Second
	for ctx.Err() == nil {
		var updates []Update
		err := c.Call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "callback_query"}}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			report(err)
			var ae *APIError
			if errors.As(err, &ae) && ae.RetryAfter > 0 {
				delay = time.Duration(ae.RetryAfter) * time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			delay = min(delay*2, 30*time.Second)
			continue
		}
		delay = time.Second
		for _, u := range updates {
			if err = handle(ctx, u); err != nil {
				report(err)
			}
			offset = u.ID + 1
		}
	}
	return ctx.Err()
}
