package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientAuthPathsAndErrors(t *testing.T) {
	var syncs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sub/api/v1/sync" && syncs.Add(1)%2 == 1 { // every first try is rate limited
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}
		switch {
		case r.URL.Path == "/sub/api/v1/plan" && r.Header.Get("Authorization") == "Bearer qbtr_k":
			_, _ = w.Write([]byte(`{"actions":[{"type":"add","hash":"h"}]}`))
		case r.URL.Path == "/sub/api/v1/plan":
			if u, p, ok := r.BasicAuth(); ok && u == "admin" && p == "pw" {
				_, _ = w.Write([]byte(`{"actions":[]}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"API key required"}`))
		case r.URL.Path == "/sub/api/v1/sync":
			_, _ = w.Write([]byte(`{"dry_run":true,"failed":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	c, err := New(srv.URL+"/sub/", "qbtr_k")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := c.Plan(ctx); err != nil || len(p.Actions) != 1 {
		t.Fatalf("plan with key: %+v %v", p, err)
	}
	start := time.Now()
	if rep, err := c.Sync(ctx, true); err != nil || !rep.DryRun || time.Since(start) < time.Second {
		t.Fatalf("429 should be retried after Retry-After: %+v %v", rep, err)
	}

	withLogin, _ := New(strings.Replace(srv.URL, "http://", "http://admin:pw@", 1)+"/sub", "")
	if _, err := withLogin.Plan(ctx); err != nil {
		t.Fatalf("basic auth from the URL: %v", err)
	}
	if strings.Contains(withLogin.Host(), "pw") {
		t.Fatal("Host() leaks the password")
	}
	anon, _ := New(srv.URL+"/sub", "")
	_, err = anon.Plan(ctx)
	if !IsStatus(err, http.StatusUnauthorized) || !strings.Contains(err.Error(), "--api-key") {
		t.Fatalf("unauthorized: %v", err)
	}
	c.maxRetry = 0 // do not wait: report the limit
	if _, err := c.Sync(ctx, true); !IsStatus(err, http.StatusTooManyRequests) || !strings.Contains(err.Error(), "retry in 1s") {
		t.Fatalf("rate limited: %v", err)
	}
	if _, err := New("ftp://x", ""); err == nil {
		t.Fatal("ftp host accepted")
	}
	if c, err := New("replicator.lan:8080", ""); err != nil || c.Host() != "http://replicator.lan:8080/" {
		t.Fatalf("scheme default: %v %v", c, err)
	}
}
