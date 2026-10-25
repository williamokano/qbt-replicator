package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/qbt"
	"github.com/williamokano/qbt-replicator/internal/qbt/fake"
	"github.com/williamokano/qbt-replicator/internal/web"
)

// remoteServer runs a real qbt-replicator web server over two fake instances
// and returns its URL plus a full and a read-only key.
func remoteServer(t *testing.T) (url, full, read string, b *fake.Server) {
	t.Helper()
	a, b := fake.New(true, "u", "top-secret"), fake.New(false, "", "")
	t.Cleanup(func() { a.Close(); b.Close() })
	a.Seed(qbt.Torrent{Hash: "h1", Name: "Linux ISO", SavePath: "/data", Size: 42}, nil, false)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	doc := fmt.Sprintf(`instances:
  - {name: a, url: %q, username: u, password: "top-secret"}
  - {name: b, url: %q}
rules:
  - {name: r, sources: [a], settle_delay: 0s}
notifications:
  - {name: tg, type: telegram, bot_token: "123:SECRET", chat_id: "1", enabled: false}
`, a.URL, b.URL)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := app.New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.Engine().Executor().AppearTimeout = time.Second
	srv := httptest.NewServer(web.New(svc, logger))
	t.Cleanup(srv.Close)
	fk, _ := svc.CreateAPIKey(context.Background(), "cli", app.ScopeFull)
	rk, _ := svc.CreateAPIKey(context.Background(), "ro", app.ScopeRead)
	return srv.URL, fk.Secret, rk.Secret, b
}

func TestRemoteCLI(t *testing.T) {
	url, full, read, b := remoteServer(t)
	clientDir := t.TempDir() // the "other machine": no config, no database
	missing := filepath.Join(clientDir, "config.yaml")
	remote := func(key string, args ...string) (string, error) {
		return run(t, append([]string{"-c", missing, "--host", url, "--api-key", key}, args...)...)
	}

	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"status"}, []string{"a", "ok", "total: up"}},
		{[]string{"plan"}, []string{"add", "Linux ISO"}},
		{[]string{"instances", "list"}, []string{"a", "b"}},
		{[]string{"instances", "test", "a"}, []string{"a OK"}},
		{[]string{"instances", "check"}, []string{"a", "b"}},
		{[]string{"torrents", "list", "--search", "linux"}, []string{"Linux ISO", "source/"}},
		{[]string{"sync", "--dry-run"}, []string{"dry-run", "(dry run)"}},
		{[]string{"replicate", "h1"}, []string{"add", "ok"}},
		{[]string{"history", "linux", "--since", "1h"}, []string{"a -> b", "Linux ISO"}},
		{[]string{"events", "--hash", "h1"}, []string{"add"}},
		{[]string{"notify", "list"}, []string{"tg", "telegram", "false"}},
		{[]string{"config", "show"}, []string{"********"}},
		{[]string{"version"}, []string{"server " + url, "qbt-replicator"}},
	} {
		out, err := remote(full, c.args...)
		if err != nil {
			t.Errorf("%v: %v\n%s", c.args, err, out)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: output lacks %q:\n%s", c.args, w, out)
			}
		}
		for _, secret := range []string{"top-secret", "123:SECRET"} {
			if strings.Contains(out, secret) {
				t.Errorf("%v leaked %q", c.args, secret)
			}
		}
	}
	if _, ok := b.Get("h1"); !ok {
		t.Fatal("remote replicate did not replicate")
	}
	if out, err := remote(full, "-o", "json", "history", "--type", "remove"); err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("json history: %v %s", err, out)
	}

	// Read-only keys read but cannot change anything.
	if _, err := remote(read, "status"); err != nil {
		t.Errorf("read key status: %v", err)
	}
	if _, err := remote(read, "unreplicate", "h1"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("read key unreplicate: %v", err)
	}
	// Wrong or missing key.
	if _, err := remote("", "status"); err == nil || !strings.Contains(err.Error(), "--api-key") {
		t.Errorf("no key: %v", err)
	}
	// Commands that need local files refuse remote mode.
	for _, args := range [][]string{{"serve"}, {"apikey", "list"}, {"config", "validate"}, {"config", "init"}} {
		if _, err := remote(full, args...); err == nil || !strings.Contains(err.Error(), "drop --host") {
			t.Errorf("%v in remote mode: %v", args, err)
		}
	}
	// Environment variables work too.
	t.Setenv("QBTR_HOST", url)
	t.Setenv("QBTR_API_KEY", read)
	if out, err := run(t, "-c", missing, "status"); err != nil || !strings.Contains(out, "total: up") {
		t.Errorf("env: %v %s", err, out)
	}
	if entries, _ := os.ReadDir(clientDir); len(entries) != 0 {
		t.Errorf("remote mode wrote local files: %v", entries)
	}
}
