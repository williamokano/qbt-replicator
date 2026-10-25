package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/williamokano/qbt-replicator/internal/app"
)

type keyTransport struct{ key string }

func (k keyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if k.key != "" {
		r.Header.Set("Authorization", "Bearer "+k.key)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func mcpSession(t *testing.T, url, key string) (*mcp.ClientSession, error) {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: keyTransport{key}}, MaxRetries: -1}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), tr, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func TestMCPOverHTTP(t *testing.T) {
	h, svc, _ := setupFile(t, editable+"server: {api: {rate_limit: {read: 0.001, read_burst: 3, write: 0.001, write_burst: 1}}}\n")
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx := context.Background()
	full, _ := svc.CreateAPIKey(ctx, "claude", app.ScopeFull)
	read, _ := svc.CreateAPIKey(ctx, "ro", app.ScopeRead)

	if _, err := mcpSession(t, srv.URL, ""); err == nil {
		t.Fatal("MCP without a key must be refused")
	}
	if _, err := mcpSession(t, srv.URL, "qbtr_wrong"); err == nil {
		t.Fatal("invalid key accepted")
	}

	ro, err := mcpSession(t, srv.URL, read.Secret)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := ro.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if slices.Contains(names, "sync") || !slices.Contains(names, "get_status") {
		t.Fatalf("read-only key tools = %v", names)
	}
	if res, err := ro.CallTool(ctx, &mcp.CallToolParams{Name: "sync"}); err == nil && !res.IsError {
		t.Fatal("read-only key ran sync")
	}

	rw, err := mcpSession(t, srv.URL, full.Secret)
	if err != nil {
		t.Fatal(err)
	}
	res, err := rw.CallTool(ctx, &mcp.CallToolParams{Name: "sync", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("sync: %+v %v", res, err)
	}
	// The write bucket holds one token: the second change is limited, reads still work.
	res, err = rw.CallTool(ctx, &mcp.CallToolParams{Name: "sync", Arguments: map[string]any{}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "rate limit") {
		t.Fatalf("second sync should be rate limited: %+v %v", res, err)
	}
	if res, err := rw.CallTool(ctx, &mcp.CallToolParams{Name: "get_status"}); err != nil || res.IsError {
		t.Fatalf("get_status: %+v %v", res, err)
	}

	// whoami tells the stdio bridge which tools to offer.
	rec := do(h, http.MethodGet, "/api/v1/whoami", nil, bearer(read.Secret))
	if !strings.Contains(rec.Body.String(), `"scope": "read"`) || !strings.Contains(rec.Body.String(), `"name": "ro"`) {
		t.Fatalf("whoami: %s", rec.Body.String())
	}
}

func TestMCPWithLogin(t *testing.T) {
	h, _, _ := setupFile(t, editable+"server: {auth: {username: admin, password: pw}}\n")
	if rec := do(h, http.MethodPost, "/mcp", strings.NewReader(`{}`), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d", rec.Code)
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	rec := do(h, http.MethodPost, "/mcp", strings.NewReader(init), map[string]string{
		"Authorization": "Basic YWRtaW46cHc=", "Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "qbt-replicator") {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
}
