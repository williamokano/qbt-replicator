package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/williamokano/qbt-replicator/internal/app"
)

func bearer(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }

func TestAPIKeyAuth(t *testing.T) {
	h, svc, _ := setupFile(t, editable)
	if rec := do(h, http.MethodGet, "/api/v1/status", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("open API without keys: %d", rec.Code)
	}

	// Created in the UI; shown once in the page.
	form := url.Values{"name": {"ha"}, "scope": {"read"}}
	rec := do(h, http.MethodPost, "/config/api-keys", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	read := regexp.MustCompile(`qbtr_[0-9a-f]{48}`).FindString(rec.Body.String())
	if rec.Code != http.StatusOK || read == "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %q", rec.Code, read)
	}
	if body := do(h, http.MethodGet, "/config/api-keys", nil, nil).Body.String(); strings.Contains(body, read) || !strings.Contains(body, "ha") {
		t.Fatal("key list must not show the secret")
	}
	full, err := svc.CreateAPIKey(context.Background(), "ci", app.ScopeFull)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		method, path string
		hdr          map[string]string
		want         int
	}{
		{http.MethodGet, "/api/v1/status", nil, http.StatusUnauthorized}, // keys exist now
		{http.MethodGet, "/api/v1/status", bearer(read), http.StatusOK},
		{http.MethodGet, "/api/v1/status", map[string]string{"X-Api-Key": read}, http.StatusOK},
		{http.MethodGet, "/metrics", bearer(read), http.StatusOK},
		{http.MethodPost, "/api/v1/sync", bearer(read), http.StatusForbidden},
		{http.MethodPost, "/api/v1/sync", bearer(full.Secret), http.StatusOK},
		{http.MethodGet, "/config", bearer(full.Secret), http.StatusUnauthorized}, // UI: login only
		{http.MethodPost, "/config/api-keys", bearer(full.Secret), http.StatusUnauthorized},
		{http.MethodGet, "/", nil, http.StatusOK}, // UI without login stays open
	}
	for _, c := range cases {
		if rec := do(h, c.method, c.path, nil, c.hdr); rec.Code != c.want {
			t.Errorf("%s %s %v: %d, want %d: %s", c.method, c.path, c.hdr, rec.Code, c.want, rec.Body.String())
		}
	}

	// Secrets stay masked for keys.
	if body := do(h, http.MethodGet, "/api/v1/config/raw", nil, bearer(full.Secret)).Body.String(); strings.Contains(body, "top#secret") {
		t.Fatal("password exposed to an API key")
	}

	// Revoked in the UI.
	keys, _ := svc.APIKeys(context.Background())
	rec = do(h, http.MethodPost, "/config/api-keys/"+itoa(keys[0].ID)+"/revoke", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/status", nil, bearer(read)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d", rec.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestAPIKeyWithLogin(t *testing.T) {
	h, svc, _ := setupFile(t, editable+"server: {auth: {username: admin, password: pw}}\n")
	k, _ := svc.CreateAPIKey(context.Background(), "ci", app.ScopeFull)
	if rec := do(h, http.MethodGet, "/api/v1/status", nil, bearer(k.Secret)); rec.Code != http.StatusOK {
		t.Fatalf("key without login: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/status", nil, map[string]string{"Authorization": "Basic YWRtaW46cHc="}); rec.Code != http.StatusOK {
		t.Fatalf("login still works for the API: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/status", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d", rec.Code)
	}

	// A full key may save the config but not change the login.
	raw := do(h, http.MethodGet, "/api/v1/config/raw", nil, bearer(k.Secret)).Body.String()
	changed := strings.Replace(raw, "http://b:8080", "http://b2:8080", 1)
	if rec := do(h, http.MethodPut, "/api/v1/config/raw", strings.NewReader(changed), bearer(k.Secret)); rec.Code != http.StatusOK {
		t.Fatalf("save with key: %d %s", rec.Code, rec.Body.String())
	}
	login := strings.Replace(changed, "username: admin", "username: mallory", 1)
	if rec := do(h, http.MethodPut, "/api/v1/config/raw", strings.NewReader(login), bearer(k.Secret)); rec.Code != http.StatusForbidden {
		t.Fatalf("login change with key: %d %s", rec.Code, rec.Body.String())
	}
	if svc.Config().Server.Auth.Username != "admin" {
		t.Fatal("login changed")
	}
}

func TestAPIKeyRateLimits(t *testing.T) {
	h, svc, _ := setupFile(t, editable+"server: {api: {rate_limit: {read: 0.001, read_burst: 2, write: 0.001, write_burst: 1}}}\n")
	k, _ := svc.CreateAPIKey(context.Background(), "ci", app.ScopeFull)
	other, _ := svc.CreateAPIKey(context.Background(), "other", app.ScopeRead)
	for i, want := range []int{200, 200, 429} {
		if rec := do(h, http.MethodGet, "/api/v1/status", nil, bearer(k.Secret)); rec.Code != want {
			t.Fatalf("read %d: %d, want %d", i, rec.Code, want)
		}
	}
	rec := do(h, http.MethodGet, "/api/v1/status", nil, bearer(k.Secret))
	if rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "rate limit") {
		t.Fatalf("429 without Retry-After: %v", rec.Header())
	}
	if rec := do(h, http.MethodPost, "/api/v1/sync", nil, bearer(k.Secret)); rec.Code != 200 {
		t.Fatalf("writes have their own bucket: %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/v1/sync", nil, bearer(k.Secret)); rec.Code != 429 {
		t.Fatalf("second write: %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/status", nil, bearer(other.Secret)); rec.Code != 200 {
		t.Fatalf("buckets are per key: %d", rec.Code)
	}

	// Invalid keys: 10 tries per address, then 429 even for a valid key.
	for i := range failBurst {
		if rec := do(h, http.MethodGet, "/api/v1/status", nil, bearer("qbtr_wrong")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad key %d: %d", i, rec.Code)
		}
	}
	rec = do(h, http.MethodGet, "/api/v1/status", nil, bearer(other.Secret))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("after guessing: %d", rec.Code)
	}
}

func TestConfigFormRateLimit(t *testing.T) {
	h, svc, _ := setupFile(t, editable)
	if page := do(h, http.MethodGet, "/config", nil, nil).Body.String(); !strings.Contains(page, `name="rl_read" type="number" min="0" step="any" value="10"`) {
		t.Fatal("rate limit fields missing")
	}
	v := baseForm()
	v.Set("rl_read", "2.5")
	v.Set("rl_read_burst", "4")
	v.Set("rl_write", "0")
	v.Set("rl_write_burst", "1")
	if rec := formPost(h, v); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if rl := svc.Config().Server.API.RateLimit; rl.Read != 2.5 || rl.ReadBurst != 4 || rl.Write != 0 {
		t.Fatalf("rate limit = %+v", rl)
	}
}
