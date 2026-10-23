package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
