package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
)

// DefaultTelegramAPI is the Bot API base URL.
const DefaultTelegramAPI = "https://api.telegram.org"

// telegramLimit is the maximum message length (UTF-16 units; runes are a safe proxy here).
const telegramLimit = 4000

type telegram struct {
	cfg    config.NotificationConfig
	api    string
	client *http.Client
}

func newTelegram(c config.NotificationConfig) *telegram {
	api := strings.TrimRight(c.APIURL, "/")
	if api == "" {
		api = DefaultTelegramAPI
	}
	return &telegram{cfg: c, api: api, client: &http.Client{Timeout: 15 * time.Second}}
}

// Send posts the batch as one message (or a few, when it is long).
func (t *telegram) Send(ctx context.Context, batch []Notice) error {
	silent := t.cfg.Silent
	for _, n := range batch {
		if n.Level != LevelInfo {
			silent = false // failures always ring
		}
	}
	for _, msg := range telegramMessages(batch) {
		if err := t.sendMessage(ctx, msg, silent); err != nil {
			return err
		}
	}
	return nil
}

// telegramMessages renders notices as HTML messages within the size limit.
func telegramMessages(batch []Notice) []string {
	var parts []string
	for _, n := range batch {
		title := html.EscapeString(n.Title)
		if n.URL != "" {
			title = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(n.URL), title)
		}
		p := n.icon() + " <b>" + title + "</b>"
		if n.Text != "" {
			p += "\n" + html.EscapeString(n.Text)
		}
		parts = append(parts, p)
	}
	var out []string
	cur := ""
	if len(batch) > 1 {
		cur = fmt.Sprintf("<i>qbt-replicator · %d updates</i>", len(batch))
	}
	for _, p := range parts {
		if len([]rune(p)) > telegramLimit {
			p = string([]rune(p)[:telegramLimit-1]) + "…"
		}
		if cur != "" && len([]rune(cur))+2+len([]rune(p)) > telegramLimit {
			out = append(out, cur)
			cur = ""
		}
		if cur != "" {
			cur += "\n\n"
		}
		cur += p
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

type telegramResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (t *telegram) sendMessage(ctx context.Context, text string, silent bool) error {
	body := map[string]any{
		"chat_id":              t.cfg.ChatID,
		"text":                 text,
		"parse_mode":           "HTML",
		"disable_notification": silent,
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if t.cfg.ThreadID != 0 {
		body["message_thread_id"] = t.cfg.ThreadID
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api+"/bot"+t.cfg.BotToken+"/sendMessage", bytes.NewReader(b))
	if err != nil {
		return t.scrub(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return t.scrub(stripURL(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var tr telegramResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return fmt.Errorf("telegram: HTTP %d: unexpected response", resp.StatusCode)
	}
	if tr.OK {
		return nil
	}
	err = fmt.Errorf("telegram: %s", explainTelegram(tr))
	if tr.ErrorCode == http.StatusTooManyRequests && tr.Parameters.RetryAfter > 0 {
		return &RetryAfterError{After: time.Duration(tr.Parameters.RetryAfter) * time.Second, Err: err}
	}
	return err
}

// explainTelegram adds a hint to the common setup mistakes.
func explainTelegram(tr telegramResponse) string {
	d := tr.Description
	switch {
	case tr.ErrorCode == http.StatusUnauthorized || tr.ErrorCode == http.StatusNotFound:
		return d + " (check the bot token from @BotFather)"
	case strings.Contains(d, "chat not found"):
		return d + " (check chat_id; send /start to the bot or add it to the group first)"
	case strings.Contains(d, "bot was blocked"):
		return d + " (unblock the bot in Telegram)"
	case strings.Contains(d, "thread not found"):
		return d + " (check thread_id: the topic must exist in the group)"
	}
	return d
}

// scrub removes the bot token from errors (it is part of the request URL).
func (t *telegram) scrub(err error) error {
	if t.cfg.BotToken == "" {
		return err
	}
	return fmt.Errorf("telegram: %s", strings.ReplaceAll(err.Error(), t.cfg.BotToken, "<bot_token>"))
}
