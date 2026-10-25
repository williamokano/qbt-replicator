package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/williamokano/qbt-replicator/internal/qbt"
	"github.com/williamokano/qbt-replicator/internal/qbt/fake"
)

func TestLocalURL(t *testing.T) {
	cases := map[[2]string]string{
		{":8080", "/"}:           "http://127.0.0.1:8080/",
		{"0.0.0.0:9000", "/x"}:   "http://127.0.0.1:9000/x/",
		{"10.0.0.2:8080", "/a/"}: "http://10.0.0.2:8080/a/",
		{"garbage", "/"}:         "http://127.0.0.1:8080/",
	}
	for in, want := range cases {
		if got := localURL(in[0], in[1]); got != want {
			t.Errorf("localURL(%v)=%s want %s", in, got, want)
		}
	}
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestConfigCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	doc := `
instances:
  - {name: a, url: "http://a", password: secret}
  - {name: b, url: "http://b"}
rules:
  - {name: r, sources: [a], filter: {include_private: true}}
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "-c", path, "config", "validate")
	if err != nil || !strings.Contains(out, "OK") || !strings.Contains(out, "WARNING") {
		t.Fatalf("validate: %v %s", err, out)
	}
	out, err = run(t, "-c", path, "config", "show")
	if err != nil || strings.Contains(out, "secret") || !strings.Contains(out, "********") {
		t.Fatalf("show leaked or failed: %v %s", err, out)
	}
	out, err = run(t, "-c", path, "-o", "json", "instances", "list")
	if err != nil || !strings.Contains(out, `"name": "a"`) {
		t.Fatalf("instances list: %v %s", err, out)
	}
	if _, err := run(t, "-c", filepath.Join(t.TempDir(), "missing.yaml"), "config", "validate"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestVersion(t *testing.T) {
	out, err := run(t, "version")
	if err != nil || !strings.Contains(out, "qbt-replicator") {
		t.Fatalf("%v %s", err, out)
	}
}

func TestConfigInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "config.yaml")
	if out, err := run(t, "-c", path, "config", "init"); err != nil || !strings.Contains(out, "wrote starter") {
		t.Fatalf("init: %v %s", err, out)
	}
	if _, err := run(t, "-c", path, "config", "init"); err == nil {
		t.Fatal("init must not overwrite")
	}
	if out, err := run(t, "-c", path, "config", "validate"); err != nil || !strings.Contains(out, "OK (0 instances") {
		t.Fatalf("starter should validate: %v %s", err, out)
	}
}

func TestHistoryAcrossCommands(t *testing.T) {
	a, b := fake.New(true, "", ""), fake.New(true, "", "")
	defer a.Close()
	defer b.Close()
	a.Seed(qbt.Torrent{Hash: "h1", Name: "Linux ISO", SavePath: "/data", Size: 42}, nil, false)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	doc := fmt.Sprintf("instances:\n  - {name: a, url: %q}\n  - {name: b, url: %q}\nrules:\n  - {name: r, sources: [a], settle_delay: 0s}\n", a.URL, b.URL)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "-c", path, "sync"); err != nil {
		t.Fatalf("sync: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "qbt-replicator.db")); err != nil {
		t.Fatalf("database not created next to the config: %v", err)
	}
	out, err := run(t, "-c", path, "history", "linux")
	if err != nil || !strings.Contains(out, "a -> b") || !strings.Contains(out, "Linux ISO") {
		t.Fatalf("history: %v %s", err, out)
	}
	out, err = run(t, "-c", path, "-o", "json", "history", "--type", "remove")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("filtered history: %v %s", err, out)
	}
	out, err = run(t, "-c", path, "events", "--hash", "h1")
	if err != nil || !strings.Contains(out, "INFO") && !strings.Contains(out, "info") {
		t.Fatalf("events: %v %s", err, out)
	}
}

func TestNotifyCommands(t *testing.T) {
	var hits int
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer hook.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	doc := fmt.Sprintf("notifications:\n  - {name: hook, type: webhook, url: %q}\n", hook.URL)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "-c", path, "notify", "test", "hook"); err != nil || hits != 1 {
		t.Fatalf("notify test: %v %s (hits %d)", err, out, hits)
	}
	if _, err := run(t, "-c", path, "notify", "test", "nope"); err == nil {
		t.Fatal("unknown notifier accepted")
	}
	if out, err := run(t, "-c", path, "notify", "list"); err != nil || !strings.Contains(out, "hook") {
		t.Fatalf("notify list: %v %s", err, out)
	}
}

func TestAPIKeyCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log: {level: info}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "-c", path, "apikey", "create", "ha", "--scope", "full")
	if err != nil || !strings.HasPrefix(out, "qbtr_") || !strings.Contains(out, "not shown again") {
		t.Fatalf("create: %v %s", err, out)
	}
	if _, err := run(t, "-c", path, "apikey", "create", "x", "--scope", "admin"); err == nil {
		t.Fatal("invalid scope accepted")
	}
	out, err = run(t, "-c", path, "apikey", "list")
	if err != nil || !strings.Contains(out, "ha") || !strings.Contains(out, "full") || strings.Count(out, "qbtr_") != 1 {
		t.Fatalf("list: %v %s", err, out)
	}
	if out, err := run(t, "-c", path, "apikey", "revoke", "ha"); err != nil || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke: %v %s", err, out)
	}
	if _, err := run(t, "-c", path, "apikey", "revoke", "ha"); err == nil {
		t.Fatal("revoking twice should fail")
	}
}
