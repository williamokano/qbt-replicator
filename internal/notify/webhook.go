package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/version"
)

type webhook struct {
	cfg    config.NotificationConfig
	client *http.Client
}

func newWebhook(c config.NotificationConfig) *webhook {
	return &webhook{cfg: c, client: &http.Client{Timeout: 15 * time.Second}}
}

// WebhookPayload is the JSON body POSTed to webhooks.
type WebhookPayload struct {
	Source  string   `json:"source"` // always "qbt-replicator"
	Version string   `json:"version"`
	Text    string   `json:"text"` // the batch as plain text, ready to display
	Notices []Notice `json:"notices"`
}

// Send POSTs the batch as JSON. Any 2xx status is success.
func (w *webhook) Send(ctx context.Context, batch []Notice) error {
	b, err := json.Marshal(WebhookPayload{Source: "qbt-replicator", Version: version.Version, Text: PlainText(batch), Notices: batch})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "qbt-replicator/"+version.Version)
	if w.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+w.cfg.Token)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", stripURL(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	err = fmt.Errorf("webhook: HTTP %d %s", resp.StatusCode, bytes.TrimSpace(body))
	if resp.StatusCode == http.StatusTooManyRequests {
		if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s > 0 {
			return &RetryAfterError{After: time.Duration(s) * time.Second, Err: err}
		}
	}
	return err
}

// stripURL drops the request URL from client errors: webhook URLs often carry
// secrets in their path.
func stripURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}
