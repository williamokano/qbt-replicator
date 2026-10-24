// Package config loads and validates the qbt-replicator YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

// DefaultPath is the configuration file location inside the Docker image.
const DefaultPath = "/config/config.yaml"

// EnvPrefix is the prefix for environment variable overrides (e.g. QBTR_LOG_LEVEL).
const EnvPrefix = "QBTR"

// Wildcard matches every instance in rule sources/targets.
const Wildcard = "*"

// Config is the root configuration document.
type Config struct {
	Log       LogConfig        `mapstructure:"log" json:"log" yaml:"log"`
	Server    ServerConfig     `mapstructure:"server" json:"server" yaml:"server"`
	Daemon    DaemonConfig     `mapstructure:"daemon" json:"daemon" yaml:"daemon"`
	Instances []InstanceConfig `mapstructure:"instances" json:"instances" yaml:"instances"`
	Rules     []RuleConfig     `mapstructure:"rules" json:"rules" yaml:"rules"`
	Database  DatabaseConfig   `mapstructure:"database" json:"database" yaml:"database"`
	// Notifications lists where to send notifications (Telegram, webhooks).
	Notifications []NotificationConfig `mapstructure:"notifications" json:"notifications" yaml:"notifications"`

	// Path is the file the configuration was loaded from (not part of YAML).
	Path string `mapstructure:"-" json:"-" yaml:"-"`
}

// LogConfig configures slog output.
type LogConfig struct {
	Level  string `mapstructure:"level" json:"level" yaml:"level"`
	Format string `mapstructure:"format" json:"format" yaml:"format"`
}

// ServerConfig configures the HTTP server.
type ServerConfig struct {
	Listen   string     `mapstructure:"listen" json:"listen" yaml:"listen"`
	BasePath string     `mapstructure:"base_path" json:"base_path" yaml:"base_path"`
	Auth     AuthConfig `mapstructure:"auth" json:"auth" yaml:"auth"`
	// PublicURL is how users reach the web UI (e.g. https://replicator.example.org),
	// used for links in notifications. Empty: no links.
	PublicURL string `mapstructure:"public_url" json:"public_url" yaml:"public_url"`
}

// AuthConfig enables HTTP basic auth on the web UI/API when both fields are set.
type AuthConfig struct {
	Username string `mapstructure:"username" json:"username" yaml:"username"`
	Password string `mapstructure:"password" json:"password" yaml:"password"`
}

// Enabled reports whether basic auth is configured.
func (a AuthConfig) Enabled() bool { return a.Username != "" && a.Password != "" }

// DaemonConfig configures the reconcile loop.
type DaemonConfig struct {
	Enabled     bool          `mapstructure:"enabled" json:"enabled" yaml:"enabled"`
	Interval    time.Duration `mapstructure:"interval" json:"interval" yaml:"interval"`
	Concurrency int           `mapstructure:"concurrency" json:"concurrency" yaml:"concurrency"`
	DryRun      bool          `mapstructure:"dry_run" json:"dry_run" yaml:"dry_run"`
	ReplicaTag  string        `mapstructure:"replica_tag" json:"replica_tag" yaml:"replica_tag"`
	// OrphanGraceTicks is how many consecutive ticks a replica must be orphaned before removal.
	OrphanGraceTicks int `mapstructure:"orphan_grace_ticks" json:"orphan_grace_ticks" yaml:"orphan_grace_ticks"`
	// OrphanGracePeriod is the minimum time a replica must be orphaned before removal
	// (in addition to OrphanGraceTicks), so fast polling cannot remove replicas hastily.
	OrphanGracePeriod time.Duration `mapstructure:"orphan_grace_period" json:"orphan_grace_period" yaml:"orphan_grace_period"`
	// ExcludeTag marks source torrents that must never be replicated (any rule).
	ExcludeTag string `mapstructure:"exclude_tag" json:"exclude_tag" yaml:"exclude_tag"`
	// StateFile persists planner memory (orphan confirmations) across runs.
	// Empty means "state.json" next to the config file; "none" disables it.
	StateFile string `mapstructure:"state_file" json:"state_file" yaml:"state_file"`
}

// DatabaseConfig configures where history and planner state are stored.
type DatabaseConfig struct {
	// URL is sqlite:///path/to/file.db or "memory". Empty means
	// qbt-replicator.db next to the config file.
	URL string `mapstructure:"url" json:"url" yaml:"url"`
	// Retention is how long history is kept, e.g. "90d", "720h"; "0" keeps it forever.
	Retention string `mapstructure:"retention" json:"retention" yaml:"retention"`

	// RetentionDur is Retention parsed during validation.
	RetentionDur time.Duration `mapstructure:"-" json:"-" yaml:"-"`
}

// DatabaseURL resolves the database location.
func (c *Config) DatabaseURL() string {
	switch {
	case c.Database.URL != "":
		return c.Database.URL
	case c.Path == "":
		return "memory"
	default:
		return "sqlite://" + filepath.Join(filepath.Dir(c.Path), "qbt-replicator.db")
	}
}

// ParseDays parses a Go duration that may also use a "d" (24h) suffix, e.g. "90d".
func ParseDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err != nil || days < 0 || fmt.Sprint(days) != n {
			return 0, fmt.Errorf("%q is not a number of days", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (examples: 90d, 720h)", s)
	}
	return d, nil
}

// StatePath resolves the planner state file ("" when disabled).
func (c *Config) StatePath() string {
	switch c.Daemon.StateFile {
	case "none":
		return ""
	case "":
		if c.Path == "" {
			return ""
		}
		return filepath.Join(filepath.Dir(c.Path), "state.json")
	default:
		return c.Daemon.StateFile
	}
}

// InstanceConfig describes one qBittorrent WebUI.
type InstanceConfig struct {
	Name         string `mapstructure:"name" json:"name" yaml:"name"`
	URL          string `mapstructure:"url" json:"url" yaml:"url"`
	Username     string `mapstructure:"username" json:"username" yaml:"username"`
	Password     string `mapstructure:"password" json:"password" yaml:"password"`
	PasswordFile string `mapstructure:"password_file" json:"password_file" yaml:"password_file"`
	// APIKey authenticates with a qBittorrent WebUI API key (Settings > WebUI,
	// qBittorrent 5.2+) instead of username/password.
	APIKey             string        `mapstructure:"api_key" json:"api_key" yaml:"api_key"`
	APIKeyFile         string        `mapstructure:"api_key_file" json:"api_key_file" yaml:"api_key_file"`
	InsecureSkipVerify bool          `mapstructure:"insecure_skip_verify" json:"insecure_skip_verify" yaml:"insecure_skip_verify"`
	Timeout            time.Duration `mapstructure:"timeout" json:"timeout" yaml:"timeout"`
	PathMappings       []PathMapping `mapstructure:"path_mappings" json:"path_mappings" yaml:"path_mappings"`
	ExportFallbackDir  string        `mapstructure:"export_fallback_dir" json:"export_fallback_dir" yaml:"export_fallback_dir"`
}

// PathMapping translates an instance-local path prefix to a canonical prefix.
type PathMapping struct {
	Instance  string `mapstructure:"instance" json:"instance" yaml:"instance"`
	Canonical string `mapstructure:"canonical" json:"canonical" yaml:"canonical"`
}

// RuleConfig describes which torrents replicate from where to where.
type RuleConfig struct {
	Name             string        `mapstructure:"name" json:"name" yaml:"name"`
	Sources          []string      `mapstructure:"sources" json:"sources" yaml:"sources"`
	Targets          []string      `mapstructure:"targets" json:"targets" yaml:"targets"`
	SettleDelay      time.Duration `mapstructure:"settle_delay" json:"settle_delay" yaml:"settle_delay"`
	Filter           FilterConfig  `mapstructure:"filter" json:"filter" yaml:"filter"`
	Add              AddConfig     `mapstructure:"add" json:"add" yaml:"add"`
	RemoveOrphans    bool          `mapstructure:"remove_orphans" json:"remove_orphans" yaml:"remove_orphans"`
	FollowRelocation bool          `mapstructure:"follow_relocation" json:"follow_relocation" yaml:"follow_relocation"`
	SyncShareLimits  bool          `mapstructure:"sync_share_limits" json:"sync_share_limits" yaml:"sync_share_limits"`
}

// FilterConfig selects eligible source torrents.
type FilterConfig struct {
	Categories     []string `mapstructure:"categories" json:"categories" yaml:"categories"`
	Tags           []string `mapstructure:"tags" json:"tags" yaml:"tags"`
	ExcludeTags    []string `mapstructure:"exclude_tags" json:"exclude_tags" yaml:"exclude_tags"`
	Trackers       []string `mapstructure:"trackers" json:"trackers" yaml:"trackers"`
	IncludePrivate bool     `mapstructure:"include_private" json:"include_private" yaml:"include_private"`
	MinSize        string   `mapstructure:"min_size" json:"min_size" yaml:"min_size"`
	MinRatio       float64  `mapstructure:"min_ratio" json:"min_ratio" yaml:"min_ratio"`
	// NameRegex / ExcludeNameRegex match the torrent name (Go regexp; prefix (?i) for case-insensitive).
	NameRegex        string `mapstructure:"name_regex" json:"name_regex" yaml:"name_regex"`
	ExcludeNameRegex string `mapstructure:"exclude_name_regex" json:"exclude_name_regex" yaml:"exclude_name_regex"`
	// SavePaths limits to torrents whose save path starts with one of these prefixes.
	SavePaths []string `mapstructure:"save_paths" json:"save_paths" yaml:"save_paths"`

	// Derived during validation.
	MinSizeBytes  int64          `mapstructure:"-" json:"-" yaml:"-"`
	NameRE        *regexp.Regexp `mapstructure:"-" json:"-" yaml:"-"`
	ExcludeNameRE *regexp.Regexp `mapstructure:"-" json:"-" yaml:"-"`
}

// AddConfig controls how replicas are added to targets.
type AddConfig struct {
	// Category is "inherit" (copy source category), "" (none) or an explicit name.
	Category     string   `mapstructure:"category" json:"category" yaml:"category"`
	ExtraTags    []string `mapstructure:"extra_tags" json:"extra_tags" yaml:"extra_tags"`
	SkipChecking bool     `mapstructure:"skip_checking" json:"skip_checking" yaml:"skip_checking"`
	Start        bool     `mapstructure:"start" json:"start" yaml:"start"`
}

// Notification types.
const (
	NotifyTelegram = "telegram"
	NotifyWebhook  = "webhook"
)

// Notification kinds a notifier can subscribe to.
const (
	EventReplicaAdded   = "replica_added"
	EventReplicaRemoved = "replica_removed"
	EventActionFailed   = "action_failed"
	EventInstanceDown   = "instance_down"
	EventInstanceUp     = "instance_up"
)

// NotificationEvents lists every subscribable kind (also the default).
var NotificationEvents = []string{EventReplicaAdded, EventReplicaRemoved, EventActionFailed, EventInstanceDown, EventInstanceUp}

// NotificationConfig is one notification destination.
type NotificationConfig struct {
	Name    string `mapstructure:"name" json:"name" yaml:"name"`
	Type    string `mapstructure:"type" json:"type" yaml:"type"` // telegram | webhook
	Enabled bool   `mapstructure:"enabled" json:"enabled" yaml:"enabled"`
	// Events to send; empty means all of NotificationEvents.
	Events []string `mapstructure:"events" json:"events" yaml:"events"`
	// Digest batches notifications for this long into one message (0 = send at once).
	Digest time.Duration `mapstructure:"digest" json:"digest" yaml:"digest"`
	// DailySummary sends a summary every day at this local time ("HH:MM"); empty = off.
	DailySummary string `mapstructure:"daily_summary" json:"daily_summary" yaml:"daily_summary"`

	// Telegram.
	BotToken     string `mapstructure:"bot_token" json:"bot_token" yaml:"bot_token"`
	BotTokenFile string `mapstructure:"bot_token_file" json:"bot_token_file" yaml:"bot_token_file"`
	ChatID       string `mapstructure:"chat_id" json:"chat_id" yaml:"chat_id"`
	// ThreadID posts into a forum topic of a supergroup.
	ThreadID int64 `mapstructure:"thread_id" json:"thread_id" yaml:"thread_id"`
	// Silent delivers informational messages without sound (failures still ring).
	Silent bool   `mapstructure:"silent" json:"silent" yaml:"silent"`
	APIURL string `mapstructure:"api_url" json:"api_url" yaml:"api_url"`

	// Webhook.
	URL       string `mapstructure:"url" json:"url" yaml:"url"`
	Token     string `mapstructure:"token" json:"token" yaml:"token"`
	TokenFile string `mapstructure:"token_file" json:"token_file" yaml:"token_file"`
}

// Wants reports whether the notifier subscribes to an event kind.
func (n NotificationConfig) Wants(kind string) bool {
	if len(n.Events) == 0 {
		return slices.Contains(NotificationEvents, kind)
	}
	return slices.Contains(n.Events, kind)
}

// CategoryInherit makes replicas use the source torrent's category.
const CategoryInherit = "inherit"

func setDefaults(v *viper.Viper) {
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "text")
	v.SetDefault("server.listen", ":8080")
	v.SetDefault("server.base_path", "/")
	v.SetDefault("daemon.enabled", true)
	v.SetDefault("daemon.interval", "5s")
	v.SetDefault("daemon.concurrency", 2)
	v.SetDefault("daemon.dry_run", false)
	v.SetDefault("daemon.replica_tag", "qbtr-replica")
	v.SetDefault("daemon.orphan_grace_ticks", 2)
	v.SetDefault("daemon.orphan_grace_period", "1m")
	v.SetDefault("daemon.exclude_tag", "qbtr-exclude")
	v.SetDefault("database.url", "")
	v.SetDefault("database.retention", "90d")
}

// ResolvePath picks the configuration path: explicit flag, then $QBTR_CONFIG,
// then /config/config.yaml, then ./config.yaml.
func ResolvePath(flag string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv(EnvPrefix + "_CONFIG"); env != "" {
		return env
	}
	if _, err := os.Stat(DefaultPath); err == nil {
		return DefaultPath
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	return DefaultPath
}

// Load reads, expands, decodes and validates the configuration at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("config %q: %w", path, err)
	}
	cfg.Path = path
	return cfg, nil
}

// Parse decodes YAML bytes (after ${VAR} expansion) into a validated Config.
func Parse(raw []byte) (*Config, error) {
	cfg, err := decode(raw, true)
	if err != nil {
		return nil, err
	}
	if err := cfg.loadSecrets(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ParseRaw decodes the file as written, with defaults applied but without
// ${VAR} expansion, QBTR_* overrides, password files or validation. The form
// editor uses it so saving never copies environment values into the file.
func ParseRaw(raw []byte) (*Config, error) {
	return decode(raw, false)
}

func decode(raw []byte, live bool) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	setDefaults(v)
	data := raw
	if live {
		v.SetEnvPrefix(EnvPrefix)
		v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
		v.AutomaticEnv()
		expanded, err := expandEnv(raw)
		if err != nil {
			return nil, err
		}
		data = expanded
	} else if err := unmarshalNode(raw, &yaml.Node{}); err != nil {
		return nil, err
	}
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}
	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}
	applyRuleDefaults(v, cfg)
	return cfg, nil
}

// applyRuleDefaults fills per-rule defaults; viper cannot default list elements.
func applyRuleDefaults(v *viper.Viper, cfg *Config) {
	raw, _ := v.Get("rules").([]any)
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		var m map[string]any
		if i < len(raw) {
			m, _ = raw[i].(map[string]any)
		}
		has := func(path ...string) bool {
			cur := m
			for j, p := range path {
				val, ok := cur[p]
				if !ok {
					return false
				}
				if j == len(path)-1 {
					return true
				}
				cur, ok = val.(map[string]any)
				if !ok {
					return false
				}
			}
			return false
		}
		if len(r.Targets) == 0 {
			r.Targets = []string{Wildcard}
		}
		if !has("settle_delay") {
			r.SettleDelay = 2 * time.Minute
		}
		if !has("add", "category") {
			r.Add.Category = CategoryInherit
		}
		if !has("add", "skip_checking") {
			r.Add.SkipChecking = true
		}
		if !has("add", "start") {
			r.Add.Start = true
		}
		if !has("remove_orphans") {
			r.RemoveOrphans = true
		}
		if !has("follow_relocation") {
			r.FollowRelocation = true
		}
	}
	rawN, _ := v.Get("notifications").([]any)
	for i := range cfg.Notifications {
		n := &cfg.Notifications[i]
		var m map[string]any
		if i < len(rawN) {
			m, _ = rawN[i].(map[string]any)
		}
		if _, ok := m["enabled"]; !ok {
			n.Enabled = true
		}
		if _, ok := m["digest"]; !ok {
			n.Digest = 30 * time.Second
		}
		if n.Name == "" {
			n.Name = n.Type
		}
	}
	for i := range cfg.Instances {
		if cfg.Instances[i].Timeout == 0 {
			cfg.Instances[i].Timeout = 15 * time.Second
		}
	}
}

func (c *Config) loadSecrets() error {
	for i := range c.Notifications {
		n := &c.Notifications[i]
		for _, f := range []struct {
			key  string
			path string
			dst  *string
		}{{"bot_token_file", n.BotTokenFile, &n.BotToken}, {"token_file", n.TokenFile, &n.Token}} {
			if f.path == "" {
				continue
			}
			b, err := os.ReadFile(f.path)
			if err != nil {
				return fmt.Errorf("notification %q: reading %s: %w", n.Name, f.key, err)
			}
			*f.dst = strings.TrimSpace(string(b))
		}
	}
	for i := range c.Instances {
		in := &c.Instances[i]
		for _, f := range []struct {
			key  string
			path string
			dst  *string
		}{{"password_file", in.PasswordFile, &in.Password}, {"api_key_file", in.APIKeyFile, &in.APIKey}} {
			if f.path == "" {
				continue
			}
			b, err := os.ReadFile(f.path)
			if err != nil {
				return fmt.Errorf("instance %q: reading %s: %w", in.Name, f.key, err)
			}
			*f.dst = strings.TrimSpace(string(b))
		}
	}
	return nil
}

// Validate checks semantic correctness and computes derived fields.
func (c *Config) Validate() error {
	var errs []error
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q is invalid", c.Log.Level))
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format %q is invalid (text|json)", c.Log.Format))
	}
	if c.Daemon.Interval < time.Second {
		errs = append(errs, fmt.Errorf("daemon.interval must be >= 1s, got %s", c.Daemon.Interval))
	}
	if c.Daemon.OrphanGracePeriod < 0 {
		errs = append(errs, errors.New("daemon.orphan_grace_period must be >= 0"))
	}
	if c.Daemon.Concurrency < 1 {
		errs = append(errs, errors.New("daemon.concurrency must be >= 1"))
	}
	if strings.TrimSpace(c.Daemon.ReplicaTag) == "" || strings.Contains(c.Daemon.ReplicaTag, ",") {
		errs = append(errs, errors.New("daemon.replica_tag must be non-empty and contain no commas"))
	}
	if t := strings.TrimSpace(c.Daemon.ExcludeTag); t == "" || strings.Contains(t, ",") || t == c.Daemon.ReplicaTag {
		errs = append(errs, errors.New("daemon.exclude_tag must be non-empty, contain no commas and differ from replica_tag"))
	}
	if c.Daemon.OrphanGraceTicks < 1 {
		errs = append(errs, errors.New("daemon.orphan_grace_ticks must be >= 1"))
	}
	switch d, err := ParseDays(c.Database.Retention); {
	case err != nil:
		errs = append(errs, fmt.Errorf("database.retention: %w", err))
	case d < 0:
		errs = append(errs, errors.New("database.retention must be >= 0"))
	default:
		c.Database.RetentionDur = d
	}
	if u := c.Database.URL; u != "" && u != "memory" && !strings.HasPrefix(u, "sqlite://") {
		errs = append(errs, fmt.Errorf("database.url %q: expected sqlite:///path/to/file.db or memory", u))
	}
	if u := c.Server.PublicURL; u != "" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			errs = append(errs, fmt.Errorf("server.public_url %q must be an absolute http(s) URL", u))
		}
	}
	errs = append(errs, c.ValidateNotifications()...)
	if !strings.HasPrefix(c.Server.BasePath, "/") {
		errs = append(errs, errors.New("server.base_path must start with /"))
	}
	c.Server.BasePath = path.Clean(c.Server.BasePath)

	// No instances at all is "setup mode" (fresh starter config): valid, idle.
	if len(c.Instances) == 1 {
		errs = append(errs, errors.New("at least two instances are required"))
	}
	names := map[string]bool{}
	for i, in := range c.Instances {
		if in.Name == "" {
			errs = append(errs, fmt.Errorf("instances[%d]: name is required", i))
			continue
		}
		if in.Name == Wildcard {
			errs = append(errs, fmt.Errorf("instances[%d]: name %q is reserved", i, Wildcard))
		}
		if names[in.Name] {
			errs = append(errs, fmt.Errorf("instances[%d]: duplicate name %q", i, in.Name))
		}
		names[in.Name] = true
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("instance %q: url %q must be an absolute http(s) URL", in.Name, in.URL))
		}
		if in.APIKey != "" && (in.Username != "" || in.Password != "") {
			errs = append(errs, fmt.Errorf("instance %q: use either api_key or username/password, not both", in.Name))
		}
		for j, m := range in.PathMappings {
			if !strings.HasPrefix(m.Instance, "/") || !strings.HasPrefix(m.Canonical, "/") {
				errs = append(errs, fmt.Errorf("instance %q: path_mappings[%d] must use absolute paths", in.Name, j))
			}
		}
	}

	if len(c.Rules) == 0 && len(c.Instances) > 0 {
		errs = append(errs, errors.New("at least one rule is required"))
	}
	ruleNames := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Name == "" {
			r.Name = fmt.Sprintf("rule-%d", i+1)
		}
		if ruleNames[r.Name] {
			errs = append(errs, fmt.Errorf("rules[%d]: duplicate name %q", i, r.Name))
		}
		ruleNames[r.Name] = true
		if len(r.Sources) == 0 {
			errs = append(errs, fmt.Errorf("rule %q: sources is required", r.Name))
		}
		for _, list := range [][]string{r.Sources, r.Targets} {
			for _, n := range list {
				if n != Wildcard && !names[n] {
					errs = append(errs, fmt.Errorf("rule %q: unknown instance %q", r.Name, n))
				}
			}
		}
		if r.SettleDelay < 0 {
			errs = append(errs, fmt.Errorf("rule %q: settle_delay must be >= 0", r.Name))
		}
		if r.Filter.MinSize != "" {
			n, err := ParseSize(r.Filter.MinSize)
			if err != nil {
				errs = append(errs, fmt.Errorf("rule %q: filter.min_size: %w", r.Name, err))
			}
			r.Filter.MinSizeBytes = n
		}
		for _, f := range []struct {
			key string
			src string
			dst **regexp.Regexp
		}{{"name_regex", r.Filter.NameRegex, &r.Filter.NameRE}, {"exclude_name_regex", r.Filter.ExcludeNameRegex, &r.Filter.ExcludeNameRE}} {
			if f.src == "" {
				continue
			}
			re, err := regexp.Compile(f.src)
			if err != nil {
				errs = append(errs, fmt.Errorf("rule %q: filter.%s: %w", r.Name, f.key, err))
				continue
			}
			*f.dst = re
		}
		for j, p := range r.Filter.SavePaths {
			if !strings.HasPrefix(p, "/") && !strings.Contains(p, ":") {
				errs = append(errs, fmt.Errorf("rule %q: filter.save_paths[%d] must be an absolute path", r.Name, j))
			}
		}
	}
	return errors.Join(errs...)
}

// ValidateNotifications checks the notifications section.
func (c *Config) ValidateNotifications() []error {
	var errs []error
	names := map[string]bool{}
	for i, n := range c.Notifications {
		label := fmt.Sprintf("notifications[%d]", i)
		if n.Name != "" {
			label = fmt.Sprintf("notification %q", n.Name)
		}
		if names[n.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate name (give each notification a unique name)", label))
		}
		names[n.Name] = true
		switch n.Type {
		case NotifyTelegram:
			if n.BotToken == "" {
				errs = append(errs, fmt.Errorf("%s: bot_token is required (from @BotFather)", label))
			}
			if n.ChatID == "" {
				errs = append(errs, fmt.Errorf("%s: chat_id is required", label))
			}
			if n.APIURL != "" {
				if u, err := url.Parse(n.APIURL); err != nil || u.Host == "" {
					errs = append(errs, fmt.Errorf("%s: api_url %q is not a URL", label, n.APIURL))
				}
			}
		case NotifyWebhook:
			if u, err := url.Parse(n.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Errorf("%s: url %q must be an absolute http(s) URL", label, n.URL))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: type %q is invalid (telegram|webhook)", label, n.Type))
		}
		for _, e := range n.Events {
			if !slices.Contains(NotificationEvents, e) {
				errs = append(errs, fmt.Errorf("%s: unknown event %q (one of %s)", label, e, strings.Join(NotificationEvents, ", ")))
			}
		}
		if n.Digest < 0 {
			errs = append(errs, fmt.Errorf("%s: digest must be >= 0", label))
		}
		if n.DailySummary != "" {
			if _, err := time.Parse("15:04", n.DailySummary); err != nil {
				errs = append(errs, fmt.Errorf("%s: daily_summary %q must be a time like 09:00", label, n.DailySummary))
			}
		}
	}
	return errs
}

// Configured reports whether any instance is defined (false = setup mode).
func (c *Config) Configured() bool { return len(c.Instances) > 0 }

// Instance returns the instance config by name.
func (c *Config) Instance(name string) (InstanceConfig, bool) {
	for _, in := range c.Instances {
		if in.Name == name {
			return in, true
		}
	}
	return InstanceConfig{}, false
}

// InstanceNames returns instance names in configuration order.
func (c *Config) InstanceNames() []string {
	out := make([]string, 0, len(c.Instances))
	for _, in := range c.Instances {
		out = append(out, in.Name)
	}
	return out
}

// Expand resolves a list that may contain the wildcard into instance names,
// excluding the names in exclude.
func (c *Config) Expand(list []string, exclude ...string) []string {
	skip := map[string]bool{}
	for _, e := range exclude {
		skip[e] = true
	}
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if !skip[n] && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range list {
		if n == Wildcard {
			for _, in := range c.Instances {
				add(in.Name)
			}
			continue
		}
		add(n)
	}
	return out
}

// Redacted returns a deep copy with secrets masked, safe to display.
func (c *Config) Redacted() Config {
	out := *c
	out.Instances = make([]InstanceConfig, len(c.Instances))
	for i, in := range c.Instances {
		if in.Password != "" {
			in.Password = "********"
		}
		if in.APIKey != "" {
			in.APIKey = "********"
		}
		out.Instances[i] = in
	}
	if out.Server.Auth.Password != "" {
		out.Server.Auth.Password = "********"
	}
	out.Notifications = make([]NotificationConfig, len(c.Notifications))
	for i, n := range c.Notifications {
		if n.BotToken != "" {
			n.BotToken = "********"
		}
		if n.Token != "" {
			n.Token = "********"
		}
		out.Notifications[i] = n
	}
	return out
}
