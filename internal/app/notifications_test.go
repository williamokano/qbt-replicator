package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/notify"
	"github.com/williamokano/qbt-replicator/internal/qbt"
	"github.com/williamokano/qbt-replicator/internal/qbt/fake"
)

// A replication and a failure reach the webhook; queued notices are delivered
// on Close even within the digest window (one-shot CLI commands).
func TestNotificationsEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var p notify.WebhookPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		texts = append(texts, p.Text)
		mu.Unlock()
	}))
	defer hook.Close()
	a, b, c := fake.New(true, "", ""), fake.New(true, "", ""), fake.New(true, "", "")
	defer a.Close()
	defer b.Close()
	a.Seed(qbt.Torrent{Hash: "h1", Name: "Linux ISO", SavePath: "/data", Size: 1}, nil, false)
	c.Close() // target c is unreachable: no add to it, an instance_down notice instead

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
server: {public_url: "http://ui.lan"}
instances:
  - {name: a, url: %q}
  - {name: b, url: %q}
  - {name: c, url: %q}
rules:
  - {name: r, sources: [a], settle_delay: 0s}
notifications:
  - {name: hook, type: webhook, url: %q, digest: 1h, daily_summary: "00:00"}
`, a.URL, b.URL, c.URL, hook.URL)))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	svc.Engine().Executor().AppearTimeout = time.Second
	svc.SyncOnce(context.Background(), false)
	if s := svc.summary(context.Background()); !strings.Contains(s.Text, "1 replicas added") || !strings.Contains(s.Text, "c: unreachable") {
		t.Errorf("summary = %q", s.Text)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 1 {
		t.Fatalf("webhook calls = %d", len(texts))
	}
	for _, want := range []string{"c is unreachable", "Replicated Linux ISO", "a → b"} {
		if !strings.Contains(texts[0], want) {
			t.Errorf("message lacks %q:\n%s", want, texts[0])
		}
	}
}
