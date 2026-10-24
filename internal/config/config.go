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
	for i := range cfg.Instances {
		if cfg.Instances[i].Timeout == 0 {
			cfg.Instances[i].Timeout = 15 * time.Second
		}
	}
}

func (c *Config) loadSecrets() error {
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
	return out
}
