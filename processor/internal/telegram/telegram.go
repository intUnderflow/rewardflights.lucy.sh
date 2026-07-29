// Package telegram is a minimal, stdlib-only Telegram Bot API client: long
// polling in (commands), sendMessage out (alerts). The bot long-polls from
// the watcher process, so there is no webhook, no inbound port, and no new
// tunnel — the host stays out of every serving path.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"time"
)

// SendTimeout bounds one sendMessage round trip; poll requests add the long
// poll window on top.
const (
	SendTimeout = 15 * time.Second
	pollSecs    = 50
)

// Client talks to one bot. Base is overridable for tests.
type Client struct {
	Token string
	Base  string // default https://api.telegram.org
	HTTP  *http.Client
}

func New(token string) *Client {
	return &Client{Token: token, Base: "https://api.telegram.org",
		HTTP: &http.Client{Timeout: SendTimeout + pollSecs*time.Second}}
}

// Update is the subset of Telegram's Update we consume.
type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
	} `json:"message"`
}

type apiResp struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) call(ctx context.Context, method string, body any, out any) (int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/bot%s/%s", c.Base, c.Token, method), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	var r apiResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return res.StatusCode, fmt.Errorf("telegram %s: bad response (%d)", method, res.StatusCode)
	}
	if !r.OK {
		if r.Parameters != nil && r.Parameters.RetryAfter > 0 {
			return r.ErrorCode, fmt.Errorf("telegram %s: rate limited, retry in %ds", method, r.Parameters.RetryAfter)
		}
		return r.ErrorCode, fmt.Errorf("telegram %s: %s (%d)", method, r.Description, r.ErrorCode)
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return res.StatusCode, err
		}
	}
	return res.StatusCode, nil
}

// GetMe returns the bot's username (for building t.me deep links).
func (c *Client) GetMe(ctx context.Context) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	if _, err := c.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return "", err
	}
	if me.Username == "" {
		return "", fmt.Errorf("telegram getMe: no username")
	}
	return me.Username, nil
}

// Send delivers one alert to one chat: bold title, plain body, one URL
// button. gone=true means the chat is unreachable for good (the user blocked
// the bot or deleted the chat) — the caller drops the subscription, exactly
// like a push 410.
func (c *Client) Send(ctx context.Context, chatID int64, title, bodyText, url, buttonLabel string) (gone bool, err error) {
	msg := map[string]any{
		"chat_id":    chatID,
		"text":       "<b>" + html.EscapeString(title) + "</b>\n" + html.EscapeString(bodyText),
		"parse_mode": "HTML",
		// The alert IS the promise; a link preview of the site under every
		// message is noise on top of it.
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if url != "" {
		msg["reply_markup"] = map[string]any{
			"inline_keyboard": [][]map[string]string{{{"text": buttonLabel, "url": url}}},
		}
	}
	code, err := c.call(ctx, "sendMessage", msg, nil)
	if err != nil && (code == 403 || code == 400) {
		// 403: blocked / kicked. 400 "chat not found": never reachable.
		return true, err
	}
	return false, err
}

// Reply sends a plain-text response to a command (no button, no HTML).
func (c *Client) Reply(ctx context.Context, chatID int64, text string) error {
	_, err := c.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID, "text": text,
		"link_preview_options": map[string]any{"is_disabled": true},
	}, nil)
	return err
}

// Poll long-polls getUpdates and hands each message to handle. It returns
// when ctx ends. Errors back off rather than spin: the bot must survive
// Telegram outages and network blips unattended.
func (c *Client) Poll(ctx context.Context, logf func(string, ...any), handle func(chatID int64, text string)) {
	var offset int64
	for ctx.Err() == nil {
		var updates []Update
		_, err := c.call(ctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": pollSecs,
			"allowed_updates": []string{"message"},
		}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logf("WARN telegram-poll: %v", err)
			select {
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			handle(u.Message.Chat.ID, u.Message.Text)
		}
	}
}

// ChatLabel renders a chat id for logs without pretending it's a name.
func ChatLabel(chatID int64) string { return "tg:" + strconv.FormatInt(chatID, 10) }
