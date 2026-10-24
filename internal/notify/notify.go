// Package notify delivers notifications (Telegram, webhooks) through a
// database outbox, so messages survive restarts and failed deliveries are
// retried with backoff.
package notify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
)

// Kinds beyond the subscribable config.NotificationEvents.
const (
	KindDailySummary = "daily_summary"
	KindTest         = "test"
)

// Levels.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Notice is one notification.
type Notice struct {
	Kind     string    `json:"kind"`
	Level    string    `json:"level"`
	Time     time.Time `json:"time"`
	Title    string    `json:"title"`              // one line, plain text
	Text     string    `json:"text,omitempty"`     // details, plain text
	Instance string    `json:"instance,omitempty"` // comma-separated when several targets
	Source   string    `json:"source,omitempty"`
	Hash     string    `json:"hash,omitempty"`
	Name     string    `json:"name,omitempty"`
	Action   string    `json:"action,omitempty"`
	Error    string    `json:"error,omitempty"`
	URL      string    `json:"url,omitempty"` // link to the web UI (server.public_url)
}

// dedupeKey identifies repeats of the same failure.
func (n Notice) dedupeKey() string {
	return strings.Join([]string{n.Kind, n.Action, n.Instance, n.Hash}, "|")
}

func (n Notice) icon() string {
	switch n.Kind {
	case config.EventReplicaAdded:
		return "✅"
	case config.EventReplicaRemoved:
		return "🗑️"
	case config.EventActionFailed:
		return "❌"
	case config.EventInstanceDown:
		return "🔴"
	case config.EventInstanceUp:
		return "🟢"
	case KindDailySummary:
		return "📊"
	default:
		return "🔔"
	}
}

// Sender delivers a batch of notices to one destination.
type Sender interface {
	Send(ctx context.Context, batch []Notice) error
}

// NewSender builds the sender for a notifier configuration.
func NewSender(c config.NotificationConfig) (Sender, error) {
	switch c.Type {
	case config.NotifyTelegram:
		return newTelegram(c), nil
	case config.NotifyWebhook:
		return newWebhook(c), nil
	default:
		return nil, fmt.Errorf("unknown notification type %q", c.Type)
	}
}

// PlainText renders a batch as plain text (webhooks, logs).
func PlainText(batch []Notice) string {
	var b strings.Builder
	for i, n := range batch {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(n.icon() + " " + n.Title)
		if n.Text != "" {
			b.WriteString("\n" + n.Text)
		}
	}
	return b.String()
}

// RetryAfterError asks the dispatcher to wait before the next attempt (HTTP 429).
type RetryAfterError struct {
	After time.Duration
	Err   error
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }
