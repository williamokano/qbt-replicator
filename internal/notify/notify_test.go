package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/store"
)

// recorder is a fake Telegram Bot API / webhook endpoint.
type recorder struct {
	mu     sync.Mutex
	bodies []map[string]any
	paths  []string
	auth   []string
	status int    // HTTP status to answer (0 = 200)
	reply  string // response body (Telegram JSON)
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(req.Body).Decode(&body)
	r.mu.Lock()
	r.bodies, r.paths, r.auth = append(r.bodies, body), append(r.paths, req.URL.Path), append(r.auth, req.Header.Get("Authorization"))
	status, reply := r.status, r.reply
	r.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if reply == "" {
		reply = `{"ok":true}`
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reply)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func newRecorder(t *testing.T) (*recorder, string) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return rec, srv.URL
}

func setup(t *testing.T, cfg *config.Config) (*Dispatcher, *store.Store, *time.Time) {
	t.Helper()
	st, err := store.Open(context.Background(), "memory")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := NewDispatcher(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.Local)
	d.Now = func() time.Time { return now }
	if err := d.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return d, st, &now
}

func added(name string) Notice {
	return Notice{Kind: config.EventReplicaAdded, Level: LevelInfo, Title: "Replicated " + name, Text: "a → b", Hash: "h-" + name, Name: name, Action: "add", Instance: "b"}
}

func TestTelegramDigestAndFormat(t *testing.T) {
	rec, url := newRecorder(t)
	cfg := &config.Config{Server: config.ServerConfig{PublicURL: "https://qbtr.example.org/"}, Notifications: []config.NotificationConfig{{
		Name: "tg", Type: config.NotifyTelegram, Enabled: true, Digest: time.Minute, Silent: true,
		BotToken: "123:abc", ChatID: "-100", ThreadID: 7, APIURL: url,
	}}}
	d, _, now := setup(t, cfg)
	ctx := context.Background()
	d.Notify(added("Ubuntu <ISO>"))
	d.Notify(added("Debian"))
	d.Flush(ctx)
	if rec.count() != 0 {
		t.Fatal("sent before the digest window")
	}
	*now = now.Add(time.Minute)
	d.Flush(ctx)
	if rec.count() != 1 {
		t.Fatalf("digest should send one message, sent %d", rec.count())
	}
	b := rec.bodies[0]
	text, _ := b["text"].(string)
	if rec.paths[0] != "/bot123:abc/sendMessage" || b["chat_id"] != "-100" || b["message_thread_id"] != float64(7) ||
		b["parse_mode"] != "HTML" || b["disable_notification"] != true {
		t.Fatalf("request = %s %v", rec.paths[0], b)
	}
	for _, want := range []string{"2 updates", "Ubuntu &lt;ISO&gt;", `<a href="https://qbtr.example.org/history?hash=h-Debian">`} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	d.Flush(ctx)
	if rec.count() != 1 {
		t.Fatal("sent twice")
	}
	// Failures ring even when silent.
	d.Notify(Notice{Kind: config.EventActionFailed, Level: LevelError, Title: "x failed", Action: "add", Instance: "b", Hash: "h"})
	*now = now.Add(time.Minute)
	d.Flush(ctx)
	if rec.count() != 2 || rec.bodies[1]["disable_notification"] != false {
		t.Fatalf("failure should not be silent: %v", rec.bodies)
	}
}

func TestWebhookEventsFilterAndToken(t *testing.T) {
	rec, url := newRecorder(t)
	cfg := &config.Config{Notifications: []config.NotificationConfig{
		{Name: "hook", Type: config.NotifyWebhook, Enabled: true, URL: url + "/in", Token: "s3cret", Events: []string{config.EventInstanceDown}},
		{Name: "off", Type: config.NotifyWebhook, Enabled: false, URL: url + "/off"},
	}}
	d, st, _ := setup(t, cfg)
	ctx := context.Background()
	d.Notify(added("Ubuntu"))
	d.Notify(Notice{Kind: config.EventInstanceDown, Level: LevelWarn, Title: "qbt-2 is unreachable", Instance: "qbt-2"})
	d.Flush(ctx)
	if rec.count() != 1 || rec.paths[0] != "/in" || rec.auth[0] != "Bearer s3cret" {
		t.Fatalf("sent %d: %v %v", rec.count(), rec.paths, rec.auth)
	}
	b := rec.bodies[0]
	notices, _ := b["notices"].([]any)
	if b["source"] != "qbt-replicator" || len(notices) != 1 || !strings.Contains(b["text"].(string), "qbt-2 is unreachable") {
		t.Fatalf("payload = %v", b)
	}
	if c, _ := st.OutboxCounts(ctx, "off"); len(c) != 0 {
		t.Fatalf("disabled notifier got items: %v", c)
	}
}

func TestRetryBackoffAndGiveUp(t *testing.T) {
	rec, url := newRecorder(t)
	rec.status = http.StatusBadGateway
	cfg := &config.Config{Notifications: []config.NotificationConfig{{Name: "hook", Type: config.NotifyWebhook, Enabled: true, URL: url}}}
	d, st, now := setup(t, cfg)
	ctx := context.Background()
	d.Notify(added("Ubuntu"))
	d.Flush(ctx)
	d.Flush(ctx) // not due yet
	if rec.count() != 1 {
		t.Fatalf("attempts = %d, want 1", rec.count())
	}
	*now = now.Add(30 * time.Second)
	d.Flush(ctx)
	if rec.count() != 2 {
		t.Fatalf("retry not attempted after 30s")
	}
	for range maxAttempts {
		*now = now.Add(time.Hour)
		d.Flush(ctx)
	}
	if rec.count() != maxAttempts {
		t.Fatalf("attempts = %d, want %d", rec.count(), maxAttempts)
	}
	if c, _ := st.OutboxCounts(ctx, "hook"); c[store.OutboxFailed] != 1 {
		t.Fatalf("counts = %v", c)
	}
	// 429 with retry_after is honored.
	rec.status, rec.reply = http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7200","parameters":{"retry_after":7200}}`
	tg := newTelegram(config.NotificationConfig{BotToken: "1:x", ChatID: "1", APIURL: url})
	err := tg.Send(ctx, []Notice{added("x")})
	var ra *RetryAfterError
	if !errors.As(err, &ra) || ra.After != 2*time.Hour {
		t.Fatalf("err = %v", err)
	}
}

func TestDedupeFailures(t *testing.T) {
	rec, url := newRecorder(t)
	cfg := &config.Config{Notifications: []config.NotificationConfig{{Name: "hook", Type: config.NotifyWebhook, Enabled: true, URL: url}}}
	d, _, now := setup(t, cfg)
	ctx := context.Background()
	fail := Notice{Kind: config.EventActionFailed, Level: LevelError, Title: "failed", Action: "add", Instance: "b", Hash: "h1", Name: "Ubuntu"}
	send := func() int {
		d.Notify(fail)
		d.Flush(ctx)
		return rec.count()
	}
	send()
	if send() != 1 {
		t.Fatal("repeated failure not deduplicated")
	}
	*now = now.Add(dedupeWindow)
	if send() != 2 {
		t.Fatal("failure not repeated after the dedupe window")
	}
	ok := added("Ubuntu")
	ok.Hash, ok.Instance = "h1", "a,b"
	d.Notify(ok)
	d.Flush(ctx)
	if send() != 4 {
		t.Fatal("failure after a success should be reported again")
	}
}

func TestDailySummary(t *testing.T) {
	rec, url := newRecorder(t)
	cfg := &config.Config{Notifications: []config.NotificationConfig{{Name: "hook", Type: config.NotifyWebhook, Enabled: true, URL: url, DailySummary: "09:30", Events: []string{config.EventInstanceDown}}}}
	d, _, now := setup(t, cfg)
	d.Summary = func(context.Context) Notice { return Notice{Level: LevelInfo, Title: "Daily summary", Text: "5 added"} }
	ctx := context.Background()
	tick := func() { d.dailySummaries(ctx); d.Flush(ctx) }
	tick() // 09:00: too early
	*now = now.Add(31 * time.Minute)
	tick()
	tick()
	if rec.count() != 1 || !strings.Contains(rec.bodies[0]["text"].(string), "5 added") {
		t.Fatalf("summaries sent = %d", rec.count())
	}
	*now = now.Add(24 * time.Hour)
	tick()
	if rec.count() != 2 {
		t.Fatal("no summary the next day")
	}
	*now = now.Add(24*time.Hour + 2*time.Hour) // service was down at 09:30: skipped
	tick()
	if rec.count() != 2 {
		t.Fatal("late summary should be skipped")
	}
}

func TestErrorsHideSecrets(t *testing.T) {
	ctx := context.Background()
	tg := newTelegram(config.NotificationConfig{BotToken: "123:SECRET", ChatID: "1", APIURL: "http://127.0.0.1:1"})
	if err := tg.Send(ctx, []Notice{added("x")}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
	wh := newWebhook(config.NotificationConfig{URL: "http://127.0.0.1:1/hooks/SECRET"})
	if err := wh.Send(ctx, []Notice{added("x")}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
	rec, url := newRecorder(t)
	rec.status, rec.reply = http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
	err := newTelegram(config.NotificationConfig{BotToken: "1:x", ChatID: "1", APIURL: url}).Send(ctx, []Notice{added("x")})
	if err == nil || !strings.Contains(err.Error(), "send /start") {
		t.Fatalf("err = %v", err)
	}
}

func TestLongBatchesAreSplit(t *testing.T) {
	var batch []Notice
	for range 200 {
		batch = append(batch, added(strings.Repeat("n", 60)))
	}
	msgs := telegramMessages(batch)
	if len(msgs) < 2 {
		t.Fatalf("messages = %d", len(msgs))
	}
	for _, m := range msgs {
		if len([]rune(m)) > telegramLimit {
			t.Fatalf("message too long: %d", len([]rune(m)))
		}
	}
}

func TestConfigureDropsRemovedNotifiers(t *testing.T) {
	_, url := newRecorder(t)
	cfg := &config.Config{Notifications: []config.NotificationConfig{{Name: "hook", Type: config.NotifyWebhook, Enabled: true, URL: url, Digest: time.Hour}}}
	d, st, _ := setup(t, cfg)
	ctx := context.Background()
	d.Notify(added("x"))
	if err := d.Configure(ctx, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.OutboxCounts(ctx, "hook"); c[store.OutboxFailed] != 1 {
		t.Fatalf("counts = %v", c)
	}
}
