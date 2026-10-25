// Package client is a typed client for the qbt-replicator JSON API, used by
// the CLI in remote mode (--host) and by the MCP stdio bridge.
package client

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
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/notify"
	"github.com/williamokano/qbt-replicator/internal/replicator"
	"github.com/williamokano/qbt-replicator/internal/store"
	"github.com/williamokano/qbt-replicator/internal/version"
)

// Client talks to a running qbt-replicator.
type Client struct {
	base     *url.URL // ends with "/"
	apiKey   string
	user     *url.Userinfo // basic auth from the host URL (user:pass@host)
	http     *http.Client
	maxRetry time.Duration // longest Retry-After waited for automatically
}

// New creates a client for host (e.g. https://replicator.lan or
// http://localhost:8080/replicator). Credentials in the URL are used as the
// web login; apiKey (qbtr_…) takes precedence.
func New(host, apiKey string) (*Client, error) {
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("host %q: expected a URL like https://replicator.lan", host)
	}
	user := u.User
	u.User = nil
	u.Path = strings.TrimRight(u.Path, "/") + "/"
	return &Client{base: u, apiKey: apiKey, user: user, http: &http.Client{Timeout: 5 * time.Minute}, maxRetry: 5 * time.Second}, nil
}

// Host returns the server URL without credentials.
func (c *Client) Host() string { return c.base.String() }

// Error is a non-2xx API answer.
type Error struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "unauthorized: " + e.Message + " (pass --api-key or set QBTR_API_KEY)"
	case http.StatusForbidden:
		return "forbidden: " + e.Message
	case http.StatusTooManyRequests:
		return fmt.Sprintf("rate limited: %s (retry in %s)", e.Message, e.RetryAfter)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// do sends a request and decodes the JSON answer into out (if non-nil).
// A 429 with a short Retry-After is retried once.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "qbt-replicator-cli/"+version.Version)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		switch {
		case c.apiKey != "":
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		case c.user != nil:
			pw, _ := c.user.Password()
			req.SetBasicAuth(c.user.Username(), pw)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("cannot reach %s: %w", c.Host(), err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode/100 == 2 {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("unexpected answer from %s: %w", u.Path, err)
			}
			return nil
		}
		apiErr := &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			apiErr.Message = e.Error
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			apiErr.RetryAfter = time.Duration(s) * time.Second
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 && apiErr.RetryAfter > 0 && apiErr.RetryAfter <= c.maxRetry {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(apiErr.RetryAfter):
			}
			continue
		}
		return apiErr
	}
}

// IsStatus reports whether err is an API error with this HTTP status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// Status returns instance health and the last run (fresh asks every instance now).
func (c *Client) Status(ctx context.Context, fresh bool) (app.Status, error) {
	var st app.Status
	q := url.Values{}
	if fresh {
		q.Set("fresh", "1")
	}
	err := c.do(ctx, http.MethodGet, "api/v1/status", q, nil, &st)
	return st, err
}

// Plan previews the next pass.
func (c *Client) Plan(ctx context.Context) (*replicator.Plan, error) {
	var p replicator.Plan
	err := c.do(ctx, http.MethodGet, "api/v1/plan", nil, nil, &p)
	return &p, err
}

// Sync runs one pass on the server.
func (c *Client) Sync(ctx context.Context, dryRun bool) (*replicator.RunReport, error) {
	var rep replicator.RunReport
	err := c.do(ctx, http.MethodPost, "api/v1/sync", nil, map[string]bool{"dry_run": dryRun}, &rep)
	return &rep, err
}

// Torrents returns the torrent × instance matrix.
func (c *Client) Torrents(ctx context.Context, f app.TorrentFilter) ([]app.TorrentRow, error) {
	q := url.Values{}
	for k, v := range map[string]string{"instance": f.Instance, "search": f.Search, "category": f.Category} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.Completed {
		q.Set("completed", "true")
	}
	if f.Fresh {
		q.Set("fresh", "1")
	}
	var rows []app.TorrentRow
	err := c.do(ctx, http.MethodGet, "api/v1/torrents", q, nil, &rows)
	return rows, err
}

// Config returns the configuration with secrets masked.
func (c *Client) Config(ctx context.Context) (config.Config, error) {
	var cfg config.Config
	err := c.do(ctx, http.MethodGet, "api/v1/config", nil, nil, &cfg)
	return cfg, err
}

// TestInstance tests a configured instance by name.
func (c *Client) TestInstance(ctx context.Context, name string) (app.InstanceTest, error) {
	var res app.InstanceTest
	err := c.do(ctx, http.MethodPost, "api/v1/instances/test", nil, map[string]string{"name": name}, &res)
	return res, err
}

// Replicate replicates a torrent manually.
func (c *Client) Replicate(ctx context.Context, req replicator.ReplicateRequest) ([]replicator.Result, error) {
	var res []replicator.Result
	err := c.do(ctx, http.MethodPost, "api/v1/replicate", nil, map[string]any{
		"hash": req.Hash, "from": req.From, "to": req.To, "force": req.Force, "dry_run": req.DryRun}, &res)
	return res, err
}

// Unreplicate removes replicas (files kept).
func (c *Client) Unreplicate(ctx context.Context, req replicator.UnreplicateRequest) ([]replicator.Result, error) {
	var res []replicator.Result
	err := c.do(ctx, http.MethodPost, "api/v1/unreplicate", nil, map[string]any{
		"hash": req.Hash, "on": req.On, "exclude": req.Exclude, "dry_run": req.DryRun}, &res)
	return res, err
}

// Include lets rules replicate an excluded torrent again.
func (c *Client) Include(ctx context.Context, hash string, dryRun bool) ([]replicator.Result, error) {
	var res []replicator.Result
	err := c.do(ctx, http.MethodPost, "api/v1/include", nil, map[string]any{"hash": hash, "dry_run": dryRun}, &res)
	return res, err
}

func pageQuery(limit int, before int64) url.Values {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if before > 0 {
		q.Set("before", strconv.FormatInt(before, 10))
	}
	return q
}

// Events returns the activity log, newest first.
func (c *Client) Events(ctx context.Context, f store.EventFilter) ([]events.Event, error) {
	q := pageQuery(f.Limit, f.BeforeID)
	for k, v := range map[string]string{"hash": f.Hash, "instance": f.Instance, "level": f.Level} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var evs []events.Event
	err := c.do(ctx, http.MethodGet, "api/v1/events", q, nil, &evs)
	return evs, err
}

// History returns executed actions, newest first.
func (c *Client) History(ctx context.Context, f store.ActionFilter) ([]store.ActionRecord, error) {
	q := pageQuery(f.Limit, f.BeforeID)
	for k, v := range map[string]string{"hash": f.Hash, "search": f.Search, "instance": f.Instance, "type": f.Type, "result": f.Result} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.UTC().Format(time.RFC3339))
	}
	var acts []store.ActionRecord
	err := c.do(ctx, http.MethodGet, "api/v1/history", q, nil, &acts)
	return acts, err
}

// Notifications lists notifiers with delivery counts.
func (c *Client) Notifications(ctx context.Context) ([]notify.Status, error) {
	var st []notify.Status
	err := c.do(ctx, http.MethodGet, "api/v1/notifications", nil, nil, &st)
	return st, err
}

// TestNotification sends a test message with a configured notifier.
func (c *Client) TestNotification(ctx context.Context, name string) error {
	var res struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := c.do(ctx, http.MethodPost, "api/v1/notifications/test", nil, map[string]string{"name": name}, &res); err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Message)
	}
	return nil
}

// Identity describes the credentials the server sees.
type Identity struct {
	Auth  string `json:"auth"` // api_key, login or none
	Name  string `json:"name,omitempty"`
	Scope string `json:"scope"` // read or full
}

// WhoAmI returns how the server authenticated this client.
func (c *Client) WhoAmI(ctx context.Context) (Identity, error) {
	var id Identity
	err := c.do(ctx, http.MethodGet, "api/v1/whoami", nil, nil, &id)
	return id, err
}
