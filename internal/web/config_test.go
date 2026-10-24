package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	page := do(h, http.MethodGet, "/config/yaml", nil, nil).Body.String()
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

func formPost(h http.Handler, v url.Values) *httptest.ResponseRecorder {
	return do(h, http.MethodPost, "/config/form", strings.NewReader(v.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

// baseForm mirrors what the form page submits for the "editable" config.
func baseForm() url.Values {
	return url.Values{
		"daemon_enabled": {"on"}, "daemon_interval": {"2s"}, "daemon_concurrency": {"2"},
		"daemon_orphan_grace_ticks": {"2"}, "daemon_orphan_grace_period": {"1m"},
		"daemon_replica_tag": {"qbtr-replica"}, "daemon_exclude_tag": {"qbtr-exclude"}, "log_level": {"info"},
		"inst-0-orig": {"a"}, "inst-0-name": {"a"}, "inst-0-url": {"http://a:8080"}, "inst-0-password": {""},
		"inst-1-orig": {"b"}, "inst-1-name": {"b"}, "inst-1-url": {"http://b2:8080"}, "inst-1-password": {"${B_PW}"},
		"rule-0-name": {"r"}, "rule-0-sources": {"a"}, "rule-0-targets": {"*"}, "rule-0-settle_delay": {"30s"},
		"rule-0-name_regex": {`(?i)S\d{2}E\d{2}`}, "rule-0-categories": {"tv, anime"},
		"rule-0-skip_checking": {"on"}, "rule-0-start": {"on"}, "rule-0-remove_orphans": {"on"},
		"rule-0-follow_relocation": {"on"}, "rule-0-add_category": {"inherit"},
	}
}

func TestConfigFormSaves(t *testing.T) {
	t.Setenv("B_PW", "from-env")
	t.Setenv("QBTR_DAEMON_DRY_RUN", "true") // env override must not leak into the file
	h, svc, path := setupFile(t, editable)
	page := do(h, http.MethodGet, "/config", nil, nil).Body.String()
	if strings.Contains(page, "top#secret") || !strings.Contains(page, `value="http://a:8080"`) || !strings.Contains(page, "QBTR_DAEMON_DRY_RUN") {
		t.Fatal("form page wrong: must show values, hide passwords and list env overrides")
	}
	rec := formPost(h, baseForm())
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("form save: %d %s", rec.Code, rec.Body.String())
	}
	cfg := svc.Config()
	if cfg.Daemon.Interval.String() != "2s" || cfg.Instances[1].URL != "http://b2:8080" ||
		cfg.Instances[0].Password != "top#secret" || cfg.Instances[1].Password != "from-env" {
		t.Fatalf("not applied: %+v %+v", cfg.Daemon, cfg.Instances)
	}
	r := cfg.Rules[0]
	if r.Filter.NameRE == nil || len(r.Filter.Categories) != 2 || r.SettleDelay.String() != "30s" || r.Filter.IncludePrivate {
		t.Fatalf("rule not applied: %+v", r)
	}
	disk, _ := os.ReadFile(path)
	d := string(disk)
	if !strings.Contains(d, "${B_PW}") || strings.Contains(d, "from-env") || strings.Contains(d, "dry_run: true") || !strings.Contains(d, "top#secret") {
		t.Fatalf("file written wrongly:\n%s", d)
	}
}

func TestConfigFormRejectsInvalid(t *testing.T) {
	h, svc, path := setupFile(t, editable)
	before, _ := os.ReadFile(path)
	v := baseForm()
	v.Set("daemon_interval", "soon")
	v.Set("rule-0-name_regex", "(")
	rec := formPost(h, v)
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "Not saved") || !strings.Contains(body, "not a duration") {
		t.Fatalf("expected validation errors, got %d", rec.Code)
	}
	if !strings.Contains(body, `value="http://b2:8080"`) {
		t.Error("rejected form should keep the submitted values")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || svc.Config().Daemon.Interval.String() != "5s" {
		t.Error("invalid form must not be written or applied")
	}
	// Valid durations but a bad regex is caught by config validation.
	v.Set("daemon_interval", "2s")
	if rec := formPost(h, v); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "name_regex") {
		t.Fatalf("regex error not reported: %d", rec.Code)
	}
}

func TestAdvancedEditorAssets(t *testing.T) {
	h, _, _ := setupFile(t, editable)
	body := do(h, http.MethodGet, "/config/yaml", nil, nil).Body.String()
	if !strings.Contains(body, "codemirror.min.js") || !strings.Contains(body, "yamleditor.js") {
		t.Fatal("advanced editor does not load CodeMirror")
	}
	for _, p := range []string{"/static/vendor/codemirror/codemirror.min.js", "/static/vendor/codemirror/yaml.min.js", "/static/yamleditor.js", "/static/configform.js"} {
		if rec := do(h, http.MethodGet, p, nil, nil); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{2 * time.Minute: "2m", time.Hour: "1h", 90 * time.Second: "1m30s", 5 * time.Second: "5s", 0: "0s", 2 * time.Hour: "2h"} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
}
