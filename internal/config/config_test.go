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
	if cfg.Daemon.Interval != 5*time.Second || cfg.Daemon.Concurrency != 2 || cfg.Daemon.ReplicaTag != "qbtr-replica" {
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
daemon: {interval: 500ms}
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

// The documented full example and the example file must stay valid.
func TestDocumentedExamplesParse(t *testing.T) {
	for _, k := range []string{"QBT1_PASSWORD", "QBT2_PASSWORD", "QBT3_PASSWORD", "QBT3_API_KEY", "QBTR_UI_PASSWORD", "TELEGRAM_BOT_TOKEN"} {
		t.Setenv(k, "x")
	}
	doc, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "## Full example")
	if !ok {
		t.Fatal("full example section missing")
	}
	_, block, _ := strings.Cut(rest, "```yaml\n")
	block, _, _ = strings.Cut(block, "```")
	cfg, err := Parse([]byte(block))
	if err != nil {
		t.Fatalf("documented example invalid: %v", err)
	}
	if cfg.Rules[0].Filter.ExcludeNameRE == nil || !cfg.Rules[0].Filter.ExcludeNameRE.MatchString("Show.Sample.mkv") {
		t.Error("documented exclude_name_regex does not match a sample")
	}
	if _, err := Load("../../config.example.yaml"); err != nil {
		t.Fatalf("config.example.yaml invalid: %v", err)
	}
}

func TestDatabase(t *testing.T) {
	cfg, err := Parse([]byte("database: {retention: 30d}"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.RetentionDur != 30*24*time.Hour {
		t.Errorf("retention = %s", cfg.Database.RetentionDur)
	}
	if got := cfg.DatabaseURL(); got != "memory" {
		t.Errorf("url without config path = %q", got)
	}
	cfg.Path = "/config/config.yaml"
	if got := cfg.DatabaseURL(); got != "sqlite:///config/qbt-replicator.db" {
		t.Errorf("default url = %q", got)
	}
	def, _ := Parse(nil)
	if def.Database.RetentionDur != 90*24*time.Hour {
		t.Errorf("default retention = %s", def.Database.RetentionDur)
	}
	for _, bad := range []string{"database: {retention: 3x}", "database: {retention: -1h}", "database: {url: postgres://x}"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
	for in, want := range map[string]time.Duration{"0": 0, "12h": 12 * time.Hour, "1d": 24 * time.Hour} {
		if d, err := ParseDays(in); err != nil || d != want {
			t.Errorf("ParseDays(%q) = %s, %v", in, d, err)
		}
	}
}

func TestNotifications(t *testing.T) {
	t.Setenv("TG_TOKEN", "123:abc")
	cfg, err := Parse([]byte(`
server: {public_url: "https://qbtr.example.org"}
notifications:
  - {type: telegram, bot_token: "${TG_TOKEN}", chat_id: "-100123", thread_id: 7}
  - {name: hook, type: webhook, url: "http://hook:9000/x", enabled: false, digest: 0s, events: [action_failed], daily_summary: "08:30"}
`))
	if err != nil {
		t.Fatal(err)
	}
	tg, hook := cfg.Notifications[0], cfg.Notifications[1]
	if tg.Name != "telegram" || !tg.Enabled || tg.Digest != 30*time.Second || tg.BotToken != "123:abc" || tg.ThreadID != 7 {
		t.Errorf("telegram defaults: %+v", tg)
	}
	if !tg.Wants(EventReplicaAdded) || hook.Wants(EventReplicaAdded) || !hook.Wants(EventActionFailed) {
		t.Error("Wants wrong")
	}
	if hook.Enabled || hook.Digest != 0 {
		t.Errorf("explicit values not kept: %+v", hook)
	}
	if r := cfg.Redacted(); r.Notifications[0].BotToken != "********" || cfg.Notifications[0].BotToken != "123:abc" {
		t.Error("bot token not redacted (or original modified)")
	}
	for _, bad := range []string{
		"notifications: [{type: telegram, chat_id: '1'}]",
		"notifications: [{type: telegram, bot_token: x}]",
		"notifications: [{type: webhook, url: 'ftp://x'}]",
		"notifications: [{type: email}]",
		"notifications: [{type: webhook, url: 'http://x', events: [everything]}]",
		"notifications: [{type: webhook, url: 'http://x', daily_summary: '25:00'}]",
		"notifications: [{type: webhook, url: 'http://x'}, {type: webhook, url: 'http://y'}]",
		"server: {public_url: 'replicator.lan'}",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}

func TestMaskNotificationSecrets(t *testing.T) {
	raw := []byte("notifications:\n  - {name: tg, type: telegram, bot_token: '123:abc', chat_id: '1'}\n  - {name: h, type: webhook, url: 'http://x', token: s3cret}\n")
	masked, err := MaskSecrets(raw)
	if err != nil || strings.Contains(string(masked), "123:abc") || strings.Contains(string(masked), "s3cret") {
		t.Fatalf("not masked: %s %v", masked, err)
	}
	back, err := UnmaskSecrets(masked, raw)
	if err != nil || !strings.Contains(string(back), "123:abc") || !strings.Contains(string(back), "s3cret") {
		t.Fatalf("not restored: %s %v", back, err)
	}
}
