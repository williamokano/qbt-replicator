package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/backend"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/qbt"
	"github.com/williamokano/qbt-replicator/internal/qbt/fake"
)

type env struct {
	svc  *app.Service
	a, b *fake.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	a, b := fake.New(true, "u", "pw-secret"), fake.New(false, "", "")
	t.Cleanup(func() { a.Close(); b.Close() })
	a.Seed(qbt.Torrent{Hash: strings.Repeat("a", 40), Name: "Ubuntu 26.04 Desktop", SavePath: "/data", Size: 4 << 30, Category: "linux"}, nil, false)
	a.Seed(qbt.Torrent{Hash: strings.Repeat("b", 40), Name: "Ubuntu 26.04 Server", SavePath: "/data", Size: 2 << 30, Category: "linux"}, nil, false)
	a.Seed(qbt.Torrent{Hash: strings.Repeat("c", 40), Name: "Private Show", SavePath: "/data", Size: 1 << 30}, nil, true)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
instances:
  - {name: a, url: %q, username: u, password: pw-secret}
  - {name: b, url: %q}
rules:
  - {name: linux, sources: [a], settle_delay: 0s}
notifications:
  - {name: tg, type: telegram, bot_token: "123:TOKEN", chat_id: "1", enabled: false}
`, a.URL, b.URL)))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.Engine().Executor().AppearTimeout = time.Second
	return &env{svc: svc, a: a, b: b}
}

func connect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call runs a tool and decodes its structured output; tool errors are returned as errors.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) error {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var msg []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg = append(msg, tc.Text)
			}
		}
		return errors.New(strings.Join(msg, " "))
	}
	if out != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decoding %s: %v", name, b, err)
		}
	}
	return nil
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func TestToolLists(t *testing.T) {
	e := newEnv(t)
	read := []string{"get_activity", "get_config", "get_history", "get_plan", "get_status", "get_torrent", "search_torrents"}
	if got := toolNames(t, connect(t, New(backend.Local(e.svc), Options{ReadOnly: true}))); !slices.Equal(got, read) {
		t.Errorf("read-only tools = %v", got)
	}
	all := append(slices.Clone(read), "include", "replicate", "sync", "test_instance", "test_notification", "unreplicate")
	slices.Sort(all)
	cs := connect(t, New(backend.Local(e.svc), Options{}))
	if got := toolNames(t, cs); !slices.Equal(got, all) {
		t.Errorf("full tools = %v", got)
	}
	res, _ := cs.ListTools(context.Background(), nil)
	for _, tl := range res.Tools {
		ro := tl.Annotations != nil && tl.Annotations.ReadOnlyHint
		if slices.Contains(read, tl.Name) != ro && tl.Name != "test_instance" {
			t.Errorf("%s: readOnlyHint = %v", tl.Name, ro)
		}
		if tl.Name == "unreplicate" && (tl.Annotations.DestructiveHint == nil || !*tl.Annotations.DestructiveHint) {
			t.Error("unreplicate must be marked destructive")
		}
	}
}

func TestReadTools(t *testing.T) {
	e := newEnv(t)
	cs := connect(t, New(backend.Local(e.svc), Options{ReadOnly: true}))

	var st statusOut
	if err := call(t, cs, "get_status", nil, &st); err != nil || len(st.Instances) != 2 || !st.Instances[0].Healthy || st.Instances[0].Torrents != 3 {
		t.Fatalf("get_status = %+v, %v", st, err)
	}

	var found searchOut
	if err := call(t, cs, "search_torrents", map[string]any{"query": "ubuntu", "limit": 1}, &found); err != nil || found.Total != 2 || len(found.Torrents) != 1 {
		t.Fatalf("search = %+v, %v", found, err)
	}
	if on := found.Torrents[0].On["a"]; !strings.HasPrefix(on, "source ") || found.Torrents[0].Size != "4.0 GiB" {
		t.Errorf("torrent = %+v", found.Torrents[0])
	}

	// Ambiguous names list the candidates; a hash prefix or exact name resolves.
	if err := call(t, cs, "get_torrent", map[string]any{"torrent": "ubuntu"}, nil); err == nil || !strings.Contains(err.Error(), "matches 2 torrents") {
		t.Fatalf("ambiguous: %v", err)
	}
	if err := call(t, cs, "get_torrent", map[string]any{"torrent": "nothing like this"}, nil); err == nil || !strings.Contains(err.Error(), "no torrent matches") {
		t.Fatalf("unknown: %v", err)
	}
	var det torrentDetailOut
	if err := call(t, cs, "get_torrent", map[string]any{"torrent": "cccc"}, &det); err != nil || det.Torrent.Name != "Private Show" {
		t.Fatalf("by prefix: %+v %v", det, err)
	}
	if len(det.Skipped) != 1 || !strings.Contains(det.Skipped[0].Reason, "private") || len(det.Planned) != 0 {
		t.Errorf("why not replicated: %+v", det)
	}
	if err := call(t, cs, "get_torrent", map[string]any{"torrent": "Ubuntu 26.04 Server"}, &det); err != nil || len(det.Planned) != 1 || det.Planned[0].Target != "b" {
		t.Fatalf("by name: %+v %v", det, err)
	}

	var plan planOut
	if err := call(t, cs, "get_plan", map[string]any{"limit": 1}, &plan); err != nil || len(plan.Actions) != 1 || !plan.Truncated {
		t.Fatalf("get_plan = %+v, %v", plan, err)
	}

	var cfg configOut
	if err := call(t, cs, "get_config", nil, &cfg); err != nil || len(cfg.Instances) != 2 || cfg.Instances[0]["auth"] != "password" {
		t.Fatalf("get_config = %+v, %v", cfg, err)
	}
	raw, _ := json.Marshal(cfg)
	for _, secret := range []string{"pw-secret", "123:TOKEN"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("get_config leaks %q", secret)
		}
	}
	if err := call(t, cs, "get_history", map[string]any{"since": "forever"}, nil); err == nil {
		t.Error("bad since accepted")
	}
}

func TestWriteToolsDefaultToDryRun(t *testing.T) {
	e := newEnv(t)
	cs := connect(t, New(backend.Local(e.svc), Options{}))
	var res resultsOut
	if err := call(t, cs, "replicate", map[string]any{"torrent": "Ubuntu 26.04 Desktop"}, &res); err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Results) != 1 || res.Results[0].Outcome != "dry_run" || !strings.Contains(res.Summary, "dry_run=false") {
		t.Fatalf("default dry run = %+v", res)
	}
	if len(e.b.Hashes()) != 0 {
		t.Fatal("dry run changed something")
	}
	if err := call(t, cs, "replicate", map[string]any{"torrent": "aaaa", "dry_run": false}, &res); err != nil || res.DryRun || res.Results[0].Outcome != "done" {
		t.Fatalf("replicate = %+v, %v", res, err)
	}
	if len(e.b.Hashes()) != 1 {
		t.Fatal("not replicated")
	}

	var hist historyListOut
	if err := call(t, cs, "get_history", map[string]any{"since": "1h", "search": "desktop"}, &hist); err != nil || len(hist.Actions) != 1 || hist.Actions[0].Result != "ok" {
		t.Fatalf("history = %+v, %v", hist, err)
	}

	if err := call(t, cs, "unreplicate", map[string]any{"torrent": "aaaa"}, &res); err != nil || !res.DryRun || len(e.b.Hashes()) != 1 {
		t.Fatalf("unreplicate dry run = %+v, %v", res, err)
	}
	if err := call(t, cs, "unreplicate", map[string]any{"torrent": "aaaa", "dry_run": false}, &res); err != nil || len(e.b.Hashes()) != 0 {
		t.Fatalf("unreplicate = %+v, %v", res, err)
	}
	if err := call(t, cs, "sync", nil, &res); err != nil || !res.DryRun {
		t.Fatalf("sync = %+v, %v", res, err)
	}

	var test app.InstanceTest
	if err := call(t, cs, "test_instance", map[string]any{"name": "a"}, &test); err != nil || !test.OK {
		t.Fatalf("test_instance = %+v, %v", test, err)
	}
	var nt testOut
	if err := call(t, cs, "test_notification", map[string]any{"name": "nope"}, &nt); err != nil || nt.OK {
		t.Fatalf("test_notification unknown = %+v, %v", nt, err)
	}
}

func TestLimit(t *testing.T) {
	e := newEnv(t)
	var writes, reads int
	cs := connect(t, New(backend.Local(e.svc), Options{Limit: func(_ context.Context, write bool) error {
		if write {
			writes++
			return errors.New("rate limit exceeded")
		}
		reads++
		return nil
	}}))
	if err := call(t, cs, "get_status", nil, nil); err != nil || reads != 1 {
		t.Fatalf("read: %v (reads %d)", err, reads)
	}
	if err := call(t, cs, "sync", nil, nil); err == nil || !strings.Contains(err.Error(), "rate limit") || writes != 1 {
		t.Fatalf("limited write: %v", err)
	}
}
