// Package qbt is a small qBittorrent WebUI API v2 client supporting both the
// 4.x and 5.x API dialects.
package qbt

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors returned by the client.
var (
	ErrUnauthorized = errors.New("qbittorrent: login failed (bad credentials)")
	ErrBanned       = errors.New("qbittorrent: client IP banned after too many failed logins")
	ErrNotFound     = errors.New("qbittorrent: torrent not found")
	ErrAddFailed    = errors.New("qbittorrent: torrent add failed")
	ErrNoMetadata   = errors.New("qbittorrent: torrent metadata not available")
	// ErrAPIKeyRejected means the WebUI refused the API key (wrong key, or a
	// qBittorrent version without API key support).
	ErrAPIKeyRejected = errors.New("qbittorrent: API key rejected (check the key; API keys need qBittorrent 5.2+)")
)

// Options configures a Client.
type Options struct {
	Name     string
	BaseURL  string
	Username string
	Password string
	// APIKey, when set, authenticates every request with "Authorization: Bearer"
	// (qBittorrent 5.2+) instead of a username/password session.
	APIKey             string
	InsecureSkipVerify bool
	Timeout            time.Duration
	// ExportFallbackDir is a mounted BT_backup directory used when /torrents/export is unavailable.
	ExportFallbackDir string
	// HTTPClient overrides the transport (tests).
	HTTPClient *http.Client
}

// Client talks to a single qBittorrent instance. It is safe for concurrent use.
type Client struct {
	opts Options
	base *url.URL
	http *http.Client

	mu          sync.Mutex
	loggedIn    bool
	apiVersion  Version
	appVersion  string
	nextLoginAt time.Time
	loginDelay  time.Duration

	privMu   sync.Mutex
	privates map[string]bool
}

// New builds a client. It does not contact the server.
func New(opts Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(opts.BaseURL, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("parsing url: %w", err)
	}
	if opts.Timeout == 0 {
		opts.Timeout = 15 * time.Second
	}
	hc := opts.HTTPClient
	if hc == nil {
		jar, _ := cookiejar.New(nil)
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if opts.InsecureSkipVerify {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in by user config
		}
		hc = &http.Client{Jar: jar, Transport: tr, Timeout: opts.Timeout}
	} else if hc.Jar == nil {
		jar, _ := cookiejar.New(nil)
		cp := *hc
		cp.Jar = jar
		hc = &cp
	}
	return &Client{opts: opts, base: u, http: hc, privates: map[string]bool{}}, nil
}

// Name returns the configured instance name.
func (c *Client) Name() string { return c.opts.Name }

// BaseURL returns the configured base URL.
func (c *Client) BaseURL() string { return c.base.String() }

// APIVersion returns the WebAPI version detected at login (zero before login).
func (c *Client) APIVersion() Version {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiVersion
}

// AppVersion returns the qBittorrent version detected at login.
func (c *Client) AppVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.appVersion
}

// v5 reports whether the instance uses the 5.x dialect (start/stop, stopped).
func (c *Client) v5() bool { return !c.APIVersion().Less(Version{2, 11, 0}) }

// Login authenticates and detects versions. Failed attempts back off exponentially
// so the client never trips qBittorrent's IP ban.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	if wait := time.Until(c.nextLoginAt); wait > 0 {
		c.mu.Unlock()
		return fmt.Errorf("login backoff active for %s: %w", wait.Round(time.Second), ErrUnauthorized)
	}
	c.mu.Unlock()

	if err := c.login(ctx); err != nil {
		c.mu.Lock()
		if c.loginDelay == 0 {
			c.loginDelay = 5 * time.Second
		} else if c.loginDelay < 10*time.Minute {
			c.loginDelay *= 2
		}
		c.nextLoginAt = time.Now().Add(c.loginDelay)
		c.loggedIn = false
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	c.loginDelay = 0
	c.nextLoginAt = time.Time{}
	c.loggedIn = true
	c.mu.Unlock()

	ver, err := c.getText(ctx, "api/v2/app/webapiVersion", nil)
	if err != nil {
		return fmt.Errorf("detecting webapi version: %w", err)
	}
	app, err := c.getText(ctx, "api/v2/app/version", nil)
	if err != nil {
		return fmt.Errorf("detecting app version: %w", err)
	}
	c.mu.Lock()
	c.apiVersion = ParseVersion(ver)
	c.appVersion = strings.TrimSpace(app)
	c.mu.Unlock()
	return nil
}

func (c *Client) login(ctx context.Context) error {
	if c.opts.APIKey != "" {
		return nil // stateless: the key is sent with every request
	}
	if c.opts.Username == "" && c.opts.Password == "" {
		return nil // auth bypassed (whitelisted subnet)
	}
	form := url.Values{"username": {c.opts.Username}, "password": {c.opts.Password}}
	resp, err := c.send(ctx, http.MethodPost, "api/v2/auth/login", func() (io.Reader, string, error) {
		return strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil
	})
	if err != nil {
		return err
	}
	defer drain(resp)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	// 4.x answers 200 "Ok."/"Fails."; 5.x answers 204 on success and 401 on bad credentials.
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return ErrBanned
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("login: unexpected status %d", resp.StatusCode)
	case strings.TrimSpace(string(body)) == "Fails.":
		return ErrUnauthorized
	}
	return nil
}

func (c *Client) ensureLogin(ctx context.Context) error {
	c.mu.Lock()
	ok := c.loggedIn
	c.mu.Unlock()
	if ok {
		return nil
	}
	return c.Login(ctx)
}

type bodyFunc func() (io.Reader, string, error)

func (c *Client) send(ctx context.Context, method, endpoint string, body bodyFunc) (*http.Response, error) {
	ref, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	u := c.base.ResolveReference(ref)
	var (
		r  io.Reader
		ct string
	)
	if body != nil {
		if r, ct, err = body(); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, err
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	// qBittorrent validates Referer/Origin against its own host (CSRF protection).
	origin := c.base.Scheme + "://" + c.base.Host
	req.Header.Set("Referer", c.base.String())
	req.Header.Set("Origin", origin)
	if c.opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	return resp, nil
}

// do performs an authenticated call, re-logging in once on 403.
func (c *Client) do(ctx context.Context, method, endpoint string, body bodyFunc) (*http.Response, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden && c.opts.APIKey != "" {
		drain(resp)
		return nil, ErrAPIKeyRejected
	}
	if resp.StatusCode == http.StatusForbidden {
		drain(resp)
		c.mu.Lock()
		c.loggedIn = false
		c.mu.Unlock()
		if err := c.Login(ctx); err != nil {
			return nil, err
		}
		resp, err = c.send(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func formBody(v url.Values) bodyFunc {
	return func() (io.Reader, string, error) {
		return strings.NewReader(v.Encode()), "application/x-www-form-urlencoded", nil
	}
}

func (c *Client) get(ctx context.Context, endpoint string, q url.Values) (*http.Response, error) {
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, endpoint, nil)
}

func (c *Client) getText(ctx context.Context, endpoint string, q url.Values) (string, error) {
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	resp, err := c.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusForbidden && c.opts.APIKey != "" {
		return "", ErrAPIKeyRejected
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", endpoint, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

func (c *Client) getJSON(ctx context.Context, endpoint string, q url.Values, out any) error {
	resp, err := c.get(ctx, endpoint, q)
	if err != nil {
		return err
	}
	defer drain(resp)
	if err := statusErr(resp, endpoint); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) post(ctx context.Context, endpoint string, form url.Values) error {
	resp, err := c.do(ctx, http.MethodPost, endpoint, formBody(form))
	if err != nil {
		return err
	}
	defer drain(resp)
	return statusErr(resp, endpoint)
}

func statusErr(resp *http.Response, endpoint string) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s: %w", endpoint, ErrNotFound)
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// Torrents lists torrents. hashes may be empty for all.
func (c *Client) Torrents(ctx context.Context, hashes ...string) ([]Torrent, error) {
	q := url.Values{}
	if len(hashes) > 0 {
		q.Set("hashes", strings.Join(hashes, "|"))
	}
	var out []Torrent
	if err := c.getJSON(ctx, "api/v2/torrents/info", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Torrent returns one torrent by hash or ErrNotFound.
func (c *Client) Torrent(ctx context.Context, hash string) (Torrent, error) {
	ts, err := c.Torrents(ctx, hash)
	if err != nil {
		return Torrent{}, err
	}
	for _, t := range ts {
		if strings.EqualFold(t.Hash, hash) {
			return t, nil
		}
	}
	return Torrent{}, ErrNotFound
}

// Files lists a torrent's files.
func (c *Client) Files(ctx context.Context, hash string) ([]File, error) {
	var out []File
	if err := c.getJSON(ctx, "api/v2/torrents/files", url.Values{"hash": {hash}}, &out); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Index == nil {
			idx := i
			out[i].Index = &idx
		}
	}
	return out, nil
}

// IsPrivate reports whether a torrent is private, using the info field when
// available and otherwise torrents/properties. Results are cached (immutable).
func (c *Client) IsPrivate(ctx context.Context, t Torrent) (bool, error) {
	if t.Private != nil {
		return *t.Private, nil
	}
	c.privMu.Lock()
	v, ok := c.privates[t.Hash]
	c.privMu.Unlock()
	if ok {
		return v, nil
	}
	var props struct {
		IsPrivate *bool `json:"is_private"`
	}
	if err := c.getJSON(ctx, "api/v2/torrents/properties", url.Values{"hash": {t.Hash}}, &props); err != nil {
		return false, err
	}
	if props.IsPrivate == nil {
		// Very old qBittorrent: fall back to tracker messages.
		var trackers []struct {
			Msg string `json:"msg"`
		}
		if err := c.getJSON(ctx, "api/v2/torrents/trackers", url.Values{"hash": {t.Hash}}, &trackers); err != nil {
			return false, err
		}
		p := false
		for _, tr := range trackers {
			if strings.Contains(strings.ToLower(tr.Msg), "private") {
				p = true
			}
		}
		props.IsPrivate = &p
	}
	c.privMu.Lock()
	c.privates[t.Hash] = *props.IsPrivate
	c.privMu.Unlock()
	return *props.IsPrivate, nil
}

// Export returns the .torrent file bytes. Falls back to ExportFallbackDir.
func (c *Client) Export(ctx context.Context, hash string) ([]byte, error) {
	resp, err := c.get(ctx, "api/v2/torrents/export", url.Values{"hash": {hash}})
	if err == nil {
		defer drain(resp)
		switch resp.StatusCode {
		case http.StatusOK:
			return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		case http.StatusConflict:
			err = fmt.Errorf("torrents/export: %w", ErrNoMetadata)
		default:
			err = statusErr(resp, "torrents/export")
		}
	}
	if c.opts.ExportFallbackDir != "" {
		b, ferr := os.ReadFile(filepath.Join(c.opts.ExportFallbackDir, strings.ToLower(hash)+".torrent"))
		if ferr == nil {
			return b, nil
		}
		return nil, errors.Join(err, ferr)
	}
	return nil, err
}

// AddOptions configures AddTorrent.
type AddOptions struct {
	SavePath     string
	Category     string
	Tags         []string
	Stopped      bool
	SkipChecking bool
	// RatioLimit/SeedingTimeLimit are only sent when non-nil.
	RatioLimit       *float64
	SeedingTimeLimit *int64
	// DownloadLimit in bytes/s (0 = not set / unlimited).
	DownloadLimit int64
}

// AddTorrent uploads a .torrent file. Content layout is always "Original" and
// Automatic Torrent Management is disabled so replicas never move files.
func (c *Client) AddTorrent(ctx context.Context, torrent []byte, name string, o AddOptions) error {
	v5 := c.v5()
	build := func() (io.Reader, string, error) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fw, err := w.CreateFormFile("torrents", name+".torrent")
		if err != nil {
			return nil, "", err
		}
		if _, err := fw.Write(torrent); err != nil {
			return nil, "", err
		}
		fields := map[string]string{
			"savepath":      o.SavePath,
			"autoTMM":       "false",
			"contentLayout": "Original",
			"skip_checking": strconv.FormatBool(o.SkipChecking),
		}
		if v5 {
			fields["stopped"] = strconv.FormatBool(o.Stopped)
		} else {
			fields["paused"] = strconv.FormatBool(o.Stopped)
		}
		if o.Category != "" {
			fields["category"] = o.Category
		}
		if len(o.Tags) > 0 {
			fields["tags"] = strings.Join(o.Tags, ",")
		}
		if o.DownloadLimit > 0 {
			fields["dlLimit"] = strconv.FormatInt(o.DownloadLimit, 10)
		}
		if o.RatioLimit != nil {
			fields["ratioLimit"] = strconv.FormatFloat(*o.RatioLimit, 'f', -1, 64)
		}
		if o.SeedingTimeLimit != nil {
			fields["seedingTimeLimit"] = strconv.FormatInt(*o.SeedingTimeLimit, 10)
		}
		for k, val := range fields {
			if err := w.WriteField(k, val); err != nil {
				return nil, "", err
			}
		}
		if err := w.Close(); err != nil {
			return nil, "", err
		}
		return &buf, w.FormDataContentType(), nil
	}
	resp, err := c.do(ctx, http.MethodPost, "api/v2/torrents/add", build)
	if err != nil {
		return err
	}
	defer drain(resp)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := strings.TrimSpace(string(body))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// 409 Conflict on 5.x means the torrent already exists.
		return fmt.Errorf("%w: status %d: %s", ErrAddFailed, resp.StatusCode, text)
	}
	if text == "Fails." {
		return ErrAddFailed
	}
	if strings.HasPrefix(text, "{") {
		// WebAPI >= 2.12 returns {"success_count":1,"failure_count":0,...}.
		var res struct {
			FailureCount int `json:"failure_count"`
			SuccessCount int `json:"success_count"`
		}
		if err := json.Unmarshal(body, &res); err == nil && res.FailureCount > 0 && res.SuccessCount == 0 {
			return fmt.Errorf("%w: %s", ErrAddFailed, text)
		}
	}
	return nil
}

// Remove deletes torrents from the client. Files are NEVER deleted: storage is
// shared between instances, so deleting data would destroy it for everyone.
func (c *Client) Remove(ctx context.Context, hashes ...string) error {
	return c.post(ctx, "api/v2/torrents/delete", url.Values{
		"hashes":      {strings.Join(hashes, "|")},
		"deleteFiles": {"false"},
	})
}

// Start resumes torrents (resume on 4.x, start on 5.x).
func (c *Client) Start(ctx context.Context, hashes ...string) error {
	ep := "api/v2/torrents/resume"
	if c.v5() {
		ep = "api/v2/torrents/start"
	}
	return c.post(ctx, ep, url.Values{"hashes": {strings.Join(hashes, "|")}})
}

// Stop pauses torrents (pause on 4.x, stop on 5.x).
func (c *Client) Stop(ctx context.Context, hashes ...string) error {
	ep := "api/v2/torrents/pause"
	if c.v5() {
		ep = "api/v2/torrents/stop"
	}
	return c.post(ctx, ep, url.Values{"hashes": {strings.Join(hashes, "|")}})
}

// AddTags adds tags to torrents (tags are created if missing).
func (c *Client) AddTags(ctx context.Context, hashes []string, tags ...string) error {
	return c.post(ctx, "api/v2/torrents/addTags", url.Values{
		"hashes": {strings.Join(hashes, "|")}, "tags": {strings.Join(tags, ",")},
	})
}

// RemoveTags removes tags from torrents.
func (c *Client) RemoveTags(ctx context.Context, hashes []string, tags ...string) error {
	return c.post(ctx, "api/v2/torrents/removeTags", url.Values{
		"hashes": {strings.Join(hashes, "|")}, "tags": {strings.Join(tags, ",")},
	})
}

// Recheck forces a hash recheck.
func (c *Client) Recheck(ctx context.Context, hashes ...string) error {
	return c.post(ctx, "api/v2/torrents/recheck", url.Values{"hashes": {strings.Join(hashes, "|")}})
}

// SetFilePriority sets the priority of the given file indexes.
func (c *Client) SetFilePriority(ctx context.Context, hash string, indexes []int, prio int) error {
	ids := make([]string, len(indexes))
	for i, x := range indexes {
		ids[i] = strconv.Itoa(x)
	}
	return c.post(ctx, "api/v2/torrents/filePrio", url.Values{
		"hash": {hash}, "id": {strings.Join(ids, "|")}, "priority": {strconv.Itoa(prio)},
	})
}

// RenameFile renames a file inside a torrent.
func (c *Client) RenameFile(ctx context.Context, hash, oldPath, newPath string) error {
	return c.post(ctx, "api/v2/torrents/renameFile", url.Values{
		"hash": {hash}, "oldPath": {oldPath}, "newPath": {newPath},
	})
}

// RenameFolder renames a folder inside a torrent.
func (c *Client) RenameFolder(ctx context.Context, hash, oldPath, newPath string) error {
	return c.post(ctx, "api/v2/torrents/renameFolder", url.Values{
		"hash": {hash}, "oldPath": {oldPath}, "newPath": {newPath},
	})
}

// SetShareLimits sets ratio (-2 global, -1 unlimited) and seeding time limits (minutes).
func (c *Client) SetShareLimits(ctx context.Context, hash string, ratio float64, seedingTime, inactiveSeedingTime int64) error {
	return c.post(ctx, "api/v2/torrents/setShareLimits", url.Values{
		"hashes":                   {hash},
		"ratioLimit":               {strconv.FormatFloat(ratio, 'f', -1, 64)},
		"seedingTimeLimit":         {strconv.FormatInt(seedingTime, 10)},
		"inactiveSeedingTimeLimit": {strconv.FormatInt(inactiveSeedingTime, 10)},
	})
}

// Categories lists categories by name.
func (c *Client) Categories(ctx context.Context) (map[string]Category, error) {
	out := map[string]Category{}
	if err := c.getJSON(ctx, "api/v2/torrents/categories", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateCategory creates a category (savePath may be empty).
func (c *Client) CreateCategory(ctx context.Context, name, savePath string) error {
	return c.post(ctx, "api/v2/torrents/createCategory", url.Values{"category": {name}, "savePath": {savePath}})
}

// EnsureCategory creates the category if it does not exist yet.
func (c *Client) EnsureCategory(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	cats, err := c.Categories(ctx)
	if err != nil {
		return err
	}
	if _, ok := cats[name]; ok {
		return nil
	}
	return c.CreateCategory(ctx, name, "")
}

// TransferInfo returns global transfer statistics.
func (c *Client) TransferInfo(ctx context.Context) (TransferInfo, error) {
	var out TransferInfo
	err := c.getJSON(ctx, "api/v2/transfer/info", nil, &out)
	return out, err
}

// ListenPort returns the configured incoming port.
func (c *Client) ListenPort(ctx context.Context) (int, error) {
	var prefs struct {
		ListenPort int `json:"listen_port"`
	}
	err := c.getJSON(ctx, "api/v2/app/preferences", nil, &prefs)
	return prefs.ListenPort, err
}
