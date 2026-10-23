package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/config"
)

func setupFile(t *testing.T, content string) (http.Handler, *app.Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
	return New(svc, logger), svc, path
}

const editable = `instances:
  - name: a
    url: http://a:8080
    password: "top#secret"
  - name: b
    url: http://b:8080
rules:
  - name: r
    sources: [a]
`

func TestConfigEditorMasksAndSaves(t *testing.T) {
	h, svc, path := setupFile(t, editable)
	page := do(h, http.MethodGet, "/config", nil, nil).Body.String()
	if strings.Contains(page, "top#secret") || !strings.Contains(page, config.Mask) {
		t.Fatal("editor must mask literal passwords")
	}
	raw := do(h, http.MethodGet, "/api/v1/config/raw", nil, nil).Body.String()
	edited := strings.Replace(raw, "http://b:8080", "http://b2:8080", 1)

	rec := do(h, http.MethodPost, "/config", strings.NewReader(url.Values{"content": {edited}}.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	cfg := svc.Config()
	if cfg.Instances[1].URL != "http://b2:8080" || cfg.Instances[0].Password != "top#secret" {
		t.Errorf("not applied or password lost: %+v", cfg.Instances)
	}
	disk, _ := os.ReadFile(path)
	if !strings.Contains(string(disk), "top#secret") || !strings.Contains(string(disk), "b2:8080") {
		t.Errorf("file not written correctly:\n%s", disk)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Error("backup missing")
	}
}

func TestConfigEditorRejectsInvalid(t *testing.T) {
	h, svc, path := setupFile(t, editable)
	before, _ := os.ReadFile(path)
	rec := do(h, http.MethodPost, "/config", strings.NewReader(url.Values{"content": {"instances: [{name: a, url: nope}]"}}.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Not saved") {
		t.Fatalf("expected validation error page, got %d", rec.Code)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || svc.Config().Instances[1].URL != "http://b:8080" {
		t.Error("invalid config must not be written or applied")
	}
	// API variant.
	rec = do(h, http.MethodPut, "/api/v1/config/raw", strings.NewReader("{{{"), nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("api invalid: %d", rec.Code)
	}
}

func TestAuthChangeAppliesImmediately(t *testing.T) {
	h, _, _ := setupFile(t, editable)
	withAuth := "server:\n  auth: {username: u, password: p}\n" + editable
	rec := do(h, http.MethodPut, "/api/v1/config/raw", strings.NewReader(withAuth), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodGet, "/", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("auth not enforced after save: %d", rec.Code)
	}
}

func TestSetupModeBanner(t *testing.T) {
	h, _, _ := setupFile(t, config.Starter)
	body := do(h, http.MethodGet, "/", nil, nil).Body.String()
	if !strings.Contains(body, "No qBittorrent instances configured") {
		t.Error("setup banner missing")
	}
}
