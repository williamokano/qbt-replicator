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
