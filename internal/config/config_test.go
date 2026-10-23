package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
instances:
  - name: a
    url: http://a:8080
    password: ${TEST_QBT_PW}
  - name: b
    url: http://b:8080
rules:
  - sources: [a]
`

func TestParseDefaultsAndEnvExpansion(t *testing.T) {
	t.Setenv("TEST_QBT_PW", "s3cret")
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instances[0].Password != "s3cret" {
		t.Errorf("password not expanded: %q", cfg.Instances[0].Password)
	}
	if cfg.Daemon.Interval != 30*time.Second || cfg.Daemon.Concurrency != 2 || cfg.Daemon.ReplicaTag != "qbtr-replica" {
		t.Errorf("daemon defaults wrong: %+v", cfg.Daemon)
	}
	if cfg.Server.Listen != ":8080" || cfg.Server.BasePath != "/" {
		t.Errorf("server defaults wrong: %+v", cfg.Server)
	}
	r := cfg.Rules[0]
	if r.Name != "rule-1" || r.SettleDelay != 2*time.Minute || r.Add.Category != CategoryInherit ||
		!r.Add.SkipChecking || !r.Add.Start || !r.RemoveOrphans || !r.FollowRelocation || r.Filter.IncludePrivate {
		t.Errorf("rule defaults wrong: %+v", r)
	}
	if len(r.Targets) != 1 || r.Targets[0] != Wildcard {
		t.Errorf("targets default wrong: %v", r.Targets)
	}
	if cfg.Instances[0].Timeout != 15*time.Second {
		t.Errorf("timeout default wrong: %v", cfg.Instances[0].Timeout)
	}
}

func TestExplicitFalseIsKept(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `    add: {skip_checking: false, start: false, category: ""}
    remove_orphans: false
    settle_delay: 0s
`))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Rules[0]
	if r.Add.SkipChecking || r.Add.Start || r.RemoveOrphans || r.SettleDelay != 0 || r.Add.Category != "" {
		t.Errorf("explicit values overridden: %+v", r)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("QBTR_LOG_LEVEL", "debug")
	t.Setenv("QBTR_DAEMON_DRY_RUN", "true")
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "debug" || !cfg.Daemon.DryRun {
		t.Errorf("env overrides not applied: %+v %+v", cfg.Log, cfg.Daemon)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"one instance": `
instances: [{name: a, url: "http://a"}]
rules: [{sources: [a]}]`,
		"duplicate": `
instances: [{name: a, url: "http://a"}, {name: a, url: "http://b"}]
rules: [{sources: [a]}]`,
		"bad url": `
instances: [{name: a, url: "a:8080"}, {name: b, url: "http://b"}]
rules: [{sources: [a]}]`,
		"unknown instance": `
instances: [{name: a, url: "http://a"}, {name: b, url: "http://b"}]
rules: [{sources: [c]}]`,
		"no rules": `
instances: [{name: a, url: "http://a"}, {name: b, url: "http://b"}]`,
		"short interval": `
daemon: {interval: 1s}
instances: [{name: a, url: "http://a"}, {name: b, url: "http://b"}]
rules: [{sources: [a]}]`,
		"bad size": `
instances: [{name: a, url: "http://a"}, {name: b, url: "http://b"}]
rules: [{sources: [a], filter: {min_size: lots}}]`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestPasswordFileAndLoad(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("filepw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(minimal, "password: ${TEST_QBT_PW}", "password_file: "+pw, 1)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instances[0].Password != "filepw" || cfg.Path != path {
		t.Errorf("got %+v", cfg.Instances[0])
	}
	if red := cfg.Redacted(); red.Instances[0].Password != "********" || cfg.Instances[0].Password != "filepw" {
		t.Error("redaction wrong or mutated original")
	}
}

func TestResolvePath(t *testing.T) {
	if got := ResolvePath("/x.yaml"); got != "/x.yaml" {
		t.Errorf("flag: %s", got)
	}
	t.Setenv("QBTR_CONFIG", "/env.yaml")
	if got := ResolvePath(""); got != "/env.yaml" {
		t.Errorf("env: %s", got)
	}
}

func TestExpand(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Expand([]string{"*"}, "a"); len(got) != 1 || got[0] != "b" {
		t.Errorf("expand: %v", got)
	}
	if got := cfg.Expand([]string{"a", "*"}); len(got) != 2 {
		t.Errorf("expand dedupe: %v", got)
	}
}

func TestPathMapping(t *testing.T) {
	in := InstanceConfig{PathMappings: []PathMapping{
		{Instance: "/downloads", Canonical: "/data"},
		{Instance: "/downloads/tv", Canonical: "/media/tv"},
	}}
	cases := map[string]string{
		"/downloads/movies/x": "/data/movies/x",
		"/downloads/tv/show":  "/media/tv/show",
		"/downloadsX/y":       "/downloadsX/y",
		"/other":              "/other",
	}
	for in2, want := range cases {
		if got := in.ToCanonical(in2); got != want {
			t.Errorf("ToCanonical(%s)=%s want %s", in2, got, want)
		}
		if want != in2 {
			if back := in.FromCanonical(want); back != in2 {
				t.Errorf("FromCanonical(%s)=%s want %s", want, back, in2)
			}
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"": 0, "1024": 1024, "1KiB": 1024, "1.5GiB": 3 << 29, "500MB": 500e6, "2 gb": 2e9}
	for s, want := range cases {
		got, err := ParseSize(s)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q)=%d,%v want %d", s, got, err, want)
		}
	}
	if _, err := ParseSize("-1"); err == nil {
		t.Error("negative accepted")
	}
}

func TestEnvExpansionIsYAMLSafe(t *testing.T) {
	t.Setenv("TEST_QBT_PW", `we"ird # pa$$:w0rd`)
	t.Setenv("TEST_INTERVAL", "45s")
	cfg, err := Parse([]byte("daemon:\n  interval: ${TEST_INTERVAL}\n" + minimal))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Instances[0].Password; got != `we"ird # pa$$:w0rd` {
		t.Errorf("password mangled: %q", got)
	}
	if cfg.Daemon.Interval != 45*time.Second {
		t.Errorf("interval: %v", cfg.Daemon.Interval)
	}
}
