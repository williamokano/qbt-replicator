package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/qbt"
	"github.com/williamokano/qbt-replicator/internal/qbt/fake"
)

func setup(t *testing.T, extra string) (http.Handler, *fake.Server, *fake.Server) {
	t.Helper()
	a, b := fake.New(true, "", ""), fake.New(true, "", "")
	t.Cleanup(func() { a.Close(); b.Close() })
	a.Seed(qbt.Torrent{Hash: "h1", Name: "Linux ISO", SavePath: "/data", Size: 42}, nil, false)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
%s
instances:
  - {name: a, url: %q}
  - {name: b, url: %q}
rules:
  - {name: r, sources: [a], settle_delay: 0s}
`, extra, a.URL, b.URL)))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := app.New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	svc.Engine().Executor().AppearTimeout = 1e9
	return New(svc, logger), a, b
}

func do(h http.Handler, method, target string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPagesRender(t *testing.T) {
	h, _, _ := setup(t, "")
	for _, p := range []string{"/", "/torrents", "/plan", "/events", "/config", "/healthz", "/readyz", "/metrics", "/static/style.css"} {
		rec := do(h, http.MethodGet, p, nil, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	rec := do(h, http.MethodGet, "/torrents", nil, nil)
	if !strings.Contains(rec.Body.String(), "Linux ISO") {
		t.Error("torrent not listed")
	}
	if rec := do(h, http.MethodGet, "/metrics", nil, nil); !strings.Contains(rec.Body.String(), "qbtr_instance_up") {
		t.Error("metrics missing")
	}
}

func TestAPIFlow(t *testing.T) {
	h, _, b := setup(t, "")
	rec := do(h, http.MethodGet, "/api/v1/plan", nil, nil)
	var plan struct {
		Actions []struct{ Type, Target string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil || len(plan.Actions) != 1 || plan.Actions[0].Target != "b" {
		t.Fatalf("plan: %s %v", rec.Body.String(), err)
	}
	rec = do(h, http.MethodPost, "/api/v1/sync", strings.NewReader(`{"dry_run":true}`), nil)
	if rec.Code != 200 || len(b.Hashes()) != 0 {
		t.Fatalf("dry-run sync: %d %v", rec.Code, b.Hashes())
	}
	rec = do(h, http.MethodPost, "/api/v1/replicate", strings.NewReader(`{"hash":"h1"}`), nil)
	if rec.Code != 200 || len(b.Hashes()) != 1 {
		t.Fatalf("replicate: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodPost, "/api/v1/unreplicate", strings.NewReader(`{"hash":"h1","on":["a"]}`), nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unreplicate of source should be refused: %d", rec.Code)
	}
	rec = do(h, http.MethodPost, "/api/v1/unreplicate", strings.NewReader(`{"hash":"h1"}`), nil)
	if rec.Code != 200 || len(b.Hashes()) != 0 {
		t.Fatalf("unreplicate: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/api/v1/events?limit=5", nil, nil)
	if !strings.Contains(rec.Body.String(), "remove") {
		t.Errorf("events: %s", rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/api/v1/config", nil, nil)
	if rec.Code != 200 {
		t.Errorf("config: %d", rec.Code)
	}
}

func TestFormReplicateRedirects(t *testing.T) {
	h, _, b := setup(t, "")
	form := url.Values{"hash": {"h1"}, "return": {"/torrents"}}
	rec := do(h, http.MethodPost, "/actions/replicate", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://example.com"})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/torrents?msg=") {
		t.Fatalf("redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if len(b.Hashes()) != 1 {
		t.Error("not replicated")
	}
}

func TestCrossOriginRejected(t *testing.T) {
	h, _, _ := setup(t, "")
	rec := do(h, http.MethodPost, "/api/v1/sync", nil, map[string]string{"Origin": "http://evil.test"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestBasicAuthAndBasePath(t *testing.T) {
	h, _, _ := setup(t, "server: {base_path: /qbtr, auth: {username: u, password: p}}")
	if rec := do(h, http.MethodGet, "/qbtr/healthz", nil, nil); rec.Code != 200 {
		t.Errorf("healthz must bypass auth: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/qbtr/", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/qbtr/api/v1/status", nil)
	req.SetBasicAuth("u", "p")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("authorized: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/qbtr", nil, nil); rec.Code != http.StatusMovedPermanently {
		t.Errorf("base redirect: %d", rec.Code)
	}
}
