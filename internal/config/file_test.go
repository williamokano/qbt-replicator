package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStarterIsValidSetupMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if err := WriteStarter(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteStarter(path); err == nil {
		t.Fatal("starter must never overwrite an existing file")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("starter config invalid: %v", err)
	}
	if cfg.Configured() {
		t.Error("starter should be in setup mode")
	}
}

func TestMaskRoundTrip(t *testing.T) {
	prev := []byte(`# my comment
server:
  auth: {username: u, password: "uipw"}
instances:
  - name: a
    url: http://a
    password: "se:cr#et"
  - name: b
    url: http://b
    password: ${B_PW}
rules: [{sources: [a]}]
`)
	masked, err := MaskSecrets(prev)
	if err != nil {
		t.Fatal(err)
	}
	m := string(masked)
	if strings.Contains(m, "se:cr#et") || strings.Contains(m, "uipw") || !strings.Contains(m, "${B_PW}") || !strings.Contains(m, "# my comment") {
		t.Fatalf("masking wrong:\n%s", m)
	}
	// User edits something else and reorders instances; masked secrets are restored by name.
	edited := strings.Replace(m, "http://a", "http://a2", 1)
	restored, err := UnmaskSecrets([]byte(edited), prev)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(restored)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instances[0].Password != "se:cr#et" || cfg.Instances[0].URL != "http://a2" || cfg.Server.Auth.Password != "uipw" {
		t.Errorf("restore wrong: %+v %+v", cfg.Instances[0], cfg.Server.Auth)
	}
	// A new instance with a masked password cannot be restored.
	added := strings.Replace(m, "rules:", "  - {name: c, url: \"http://c\", password: \""+Mask+"\"}\nrules:", 1)
	if _, err := UnmaskSecrets([]byte(added), prev); !errors.Is(err, ErrUnknownMasked) {
		t.Errorf("expected ErrUnknownMasked, got %v", err)
	}
}

func TestSaveFileKeepsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveFile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Errorf("content %q", b)
	}
	if b, _ := os.ReadFile(path + ".bak"); string(b) != "old" {
		t.Errorf("backup %q", b)
	}
}

// Uncommenting the starter's example blocks must yield a valid configuration.
func TestStarterExamplesUncommentToValidConfig(t *testing.T) {
	t.Setenv("QBT1_PASSWORD", "a")
	t.Setenv("QBT2_PASSWORD", "b")
	var out []string
	uncomment := false
	for _, l := range strings.Split(Starter, "\n") {
		if strings.HasPrefix(l, "# instances:") {
			uncomment = true
		}
		if uncomment && strings.HasPrefix(l, "# ") {
			l = strings.TrimPrefix(l, "# ")
		}
		out = append(out, l)
	}
	cfg, err := Parse([]byte(strings.Join(out, "\n")))
	if err != nil {
		t.Fatalf("uncommented starter invalid: %v", err)
	}
	if len(cfg.Instances) != 2 || len(cfg.Rules) != 1 {
		t.Errorf("got %d instances, %d rules", len(cfg.Instances), len(cfg.Rules))
	}
}

func TestYAMLErrorPointsAtBadLine(t *testing.T) {
	doc := "# comment\n\nlog:\n  level: info\n\ninstances: []\n - name: qbt-1\n   url: http://a\nrules: []\n"
	_, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "line 7") || !strings.Contains(msg, `remove the "[]"`) {
		t.Errorf("unhelpful error: %s", msg)
	}
	if _, err := UnmaskSecrets([]byte(doc), nil); err == nil || !strings.Contains(err.Error(), "line 7") {
		t.Errorf("editor path error not explained: %v", err)
	}
	if _, err := Parse([]byte("log:\n\tlevel: info\n")); err == nil || !strings.Contains(err.Error(), "tabs") {
		t.Errorf("tab hint missing: %v", err)
	}
}

func TestAPIKeyConfig(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("qbt_fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(`
instances:
  - {name: a, url: "http://a", api_key: qbt_inline}
  - {name: b, url: "http://b", api_key_file: ` + keyFile + `}
rules: [{sources: [a]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instances[0].APIKey != "qbt_inline" || cfg.Instances[1].APIKey != "qbt_fromfile" {
		t.Fatalf("keys: %+v", cfg.Instances)
	}
	if red := cfg.Redacted(); red.Instances[0].APIKey != Mask {
		t.Error("api key not redacted")
	}
	masked, err := MaskSecrets([]byte("instances:\n  - name: a\n    api_key: qbt_inline\n"))
	if err != nil || strings.Contains(string(masked), "qbt_inline") {
		t.Errorf("api key not masked in editor: %s %v", masked, err)
	}
	_, err = Parse([]byte(`
instances:
  - {name: a, url: "http://a", api_key: k, username: u, password: p}
  - {name: b, url: "http://b"}
rules: [{sources: [a]}]
`))
	if err == nil || !strings.Contains(err.Error(), "either api_key or username") {
		t.Errorf("expected conflict error, got %v", err)
	}
}
