package web

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
)

// The form editor posts flat fields; repeated rows use indexed names such as
// "inst-3-url" and "rule-0-sources". Indexes need not be contiguous (rows can
// be removed client-side), they only define the order.

// formRows returns the sorted row indexes present for a prefix ("inst" or "rule").
func formRows(form url.Values, prefix string) []int {
	seen := map[int]bool{}
	for k := range form {
		rest, ok := strings.CutPrefix(k, prefix+"-")
		if !ok {
			continue
		}
		idx, _, ok := strings.Cut(rest, "-")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(idx); err == nil {
			seen[n] = true
		}
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func field(form url.Values, prefix string, i int, name string) string {
	return strings.TrimSpace(form.Get(fmt.Sprintf("%s-%d-%s", prefix, i, name)))
}

func checked(form url.Values, key string) bool {
	v := form.Get(key)
	return v == "on" || v == "true" || v == "1"
}

// list splits a comma-separated input into trimmed, non-empty values.
func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinList(v []string) string { return strings.Join(v, ", ") }

type formErrors []string

func (e *formErrors) duration(label, s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		*e = append(*e, fmt.Sprintf("%s: %q is not a duration (examples: 2s, 30s, 2m, 1h)", label, s))
		return def
	}
	return d
}

func (e *formErrors) integer(label, s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		*e = append(*e, fmt.Sprintf("%s: %q is not a whole number", label, s))
		return def
	}
	return n
}

func (e *formErrors) float(label, s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*e = append(*e, fmt.Sprintf("%s: %q is not a number", label, s))
	}
	return f
}

// instanceFromForm reads instance row i on top of the stored instance with the
// row's original name (keeping path_mappings, password_file, …). An empty
// password or API key field keeps the stored one.
func instanceFromForm(f url.Values, i int, old map[string]config.InstanceConfig, errs *formErrors) config.InstanceConfig {
	name := field(f, "inst", i, "name")
	in := old[field(f, "inst", i, "orig")]
	in.Name, in.URL = name, field(f, "inst", i, "url")
	if field(f, "inst", i, "auth") == "apikey" {
		if k := f.Get(fmt.Sprintf("inst-%d-api_key", i)); k != "" {
			in.APIKey = k
		}
		if in.APIKey == "" && in.APIKeyFile == "" {
			*errs = append(*errs, fmt.Sprintf("Instance %s: enter the API key (or switch back to username & password)", name))
		}
		in.Username, in.Password, in.PasswordFile = "", "", ""
	} else {
		in.Username = field(f, "inst", i, "username")
		if pw := f.Get(fmt.Sprintf("inst-%d-password", i)); pw != "" {
			in.Password = pw
		}
		in.APIKey, in.APIKeyFile = "", ""
	}
	in.InsecureSkipVerify = checked(f, fmt.Sprintf("inst-%d-insecure", i))
	in.Timeout = errs.duration("Instance "+name+" timeout", field(f, "inst", i, "timeout"), 15*time.Second)
	return in
}

// configFromForm applies the form onto a copy of the raw (as-written) config.
// Settings the form does not show (listen address, path mappings, …) are kept
// from base; an empty password field keeps the stored password of that instance.
func configFromForm(r *http.Request, base *config.Config) (*config.Config, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	f := r.PostForm
	cfg := *base
	var errs formErrors

	cfg.Log.Level = f.Get("log_level")
	cfg.Daemon.Enabled = checked(f, "daemon_enabled")
	cfg.Daemon.DryRun = checked(f, "daemon_dry_run")
	cfg.Daemon.Interval = errs.duration("Poll interval", f.Get("daemon_interval"), base.Daemon.Interval)
	cfg.Daemon.Concurrency = errs.integer("Concurrency", f.Get("daemon_concurrency"), base.Daemon.Concurrency)
	cfg.Daemon.OrphanGraceTicks = errs.integer("Orphan confirmations", f.Get("daemon_orphan_grace_ticks"), base.Daemon.OrphanGraceTicks)
	cfg.Daemon.OrphanGracePeriod = errs.duration("Orphan grace period", f.Get("daemon_orphan_grace_period"), base.Daemon.OrphanGracePeriod)
	if v := strings.TrimSpace(f.Get("daemon_replica_tag")); v != "" {
		cfg.Daemon.ReplicaTag = v
	}
	if v := strings.TrimSpace(f.Get("daemon_exclude_tag")); v != "" {
		cfg.Daemon.ExcludeTag = v
	}
	if v := strings.TrimSpace(f.Get("database_retention")); v != "" {
		if _, err := config.ParseDays(v); err != nil {
			errs = append(errs, "History retention: "+err.Error())
		}
		cfg.Database.Retention = v
	}
	cfg.Server.PublicURL = strings.TrimSpace(f.Get("public_url"))
	cfg.Server.Auth.Username = strings.TrimSpace(f.Get("auth_username"))
	switch pw := f.Get("auth_password"); {
	case checked(f, "auth_clear"):
		cfg.Server.Auth.Password = ""
	case pw != "":
		cfg.Server.Auth.Password = pw
	}

	oldInst := map[string]config.InstanceConfig{}
	for _, in := range base.Instances {
		oldInst[in.Name] = in
	}
	cfg.Instances = nil
	for _, i := range formRows(f, "inst") {
		name := field(f, "inst", i, "name")
		url := field(f, "inst", i, "url")
		if name == "" && url == "" {
			continue // blank row
		}
		cfg.Instances = append(cfg.Instances, instanceFromForm(f, i, oldInst, &errs))
	}

	cfg.Rules = nil
	for _, i := range formRows(f, "rule") {
		key := func(n string) string { return fmt.Sprintf("rule-%d-%s", i, n) }
		get := func(n string) string { return field(f, "rule", i, n) }
		name := get("name")
		if name == "" && get("sources") == "" {
			continue
		}
		rc := config.RuleConfig{
			Name:        name,
			Sources:     list(get("sources")),
			Targets:     list(get("targets")),
			SettleDelay: errs.duration("Rule "+name+" settle delay", get("settle_delay"), 2*time.Minute),
			Filter: config.FilterConfig{
				Categories:       list(get("categories")),
				Tags:             list(get("tags")),
				ExcludeTags:      list(get("exclude_tags")),
				Trackers:         list(get("trackers")),
				IncludePrivate:   checked(f, key("include_private")),
				MinSize:          get("min_size"),
				MinRatio:         errs.float("Rule "+name+" min ratio", get("min_ratio")),
				NameRegex:        get("name_regex"),
				ExcludeNameRegex: get("exclude_name_regex"),
				SavePaths:        list(get("save_paths")),
			},
			Add: config.AddConfig{
				Category:     get("add_category"),
				ExtraTags:    list(get("extra_tags")),
				SkipChecking: checked(f, key("skip_checking")),
				Start:        checked(f, key("start")),
			},
			RemoveOrphans:    checked(f, key("remove_orphans")),
			FollowRelocation: checked(f, key("follow_relocation")),
			SyncShareLimits:  checked(f, key("sync_share_limits")),
		}
		if len(rc.Targets) == 0 {
			rc.Targets = []string{config.Wildcard}
		}
		cfg.Rules = append(cfg.Rules, rc)
	}
	oldNotif := map[string]config.NotificationConfig{}
	for _, n := range base.Notifications {
		oldNotif[n.Name] = n
	}
	cfg.Notifications = nil
	for _, i := range formRows(f, "notif") {
		if field(f, "notif", i, "name") == "" && field(f, "notif", i, "chat_id") == "" && field(f, "notif", i, "url") == "" {
			continue // blank row
		}
		cfg.Notifications = append(cfg.Notifications, notificationFromForm(f, i, oldNotif, &errs))
	}
	if len(errs) > 0 {
		return &cfg, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return &cfg, nil
}

// eventLabels describe notification events in the form.
var eventLabels = map[string]string{
	config.EventReplicaAdded:   "Replica added",
	config.EventReplicaRemoved: "Replica removed",
	config.EventActionFailed:   "Action failed",
	config.EventInstanceDown:   "Instance unreachable",
	config.EventInstanceUp:     "Instance back",
}

// notificationFromForm reads notification row i on top of the stored notifier
// with the row's original name. Empty token fields keep the stored ones.
func notificationFromForm(f url.Values, i int, old map[string]config.NotificationConfig, errs *formErrors) config.NotificationConfig {
	get := func(n string) string { return field(f, "notif", i, n) }
	key := func(n string) string { return fmt.Sprintf("notif-%d-%s", i, n) }
	n := old[get("orig")]
	n.Name, n.Type = get("name"), get("type")
	label := "Notification " + n.Name
	n.Enabled = checked(f, key("enabled"))
	n.Digest = errs.duration(label+" digest", get("digest"), 0)
	n.DailySummary = get("daily_summary")
	n.Events = nil
	for _, ev := range config.NotificationEvents {
		if checked(f, key("ev-"+ev)) {
			n.Events = append(n.Events, ev)
		}
	}
	switch len(n.Events) {
	case 0:
		*errs = append(*errs, label+": choose at least one event (or untick Enabled)")
	case len(config.NotificationEvents):
		n.Events = nil // all
	}
	switch n.Type {
	case config.NotifyTelegram:
		if v := f.Get(key("bot_token")); v != "" {
			n.BotToken = v
		}
		n.ChatID = get("chat_id")
		n.ThreadID = int64(errs.integer(label+" topic id", get("thread_id"), 0))
		n.Silent = checked(f, key("silent"))
		n.URL, n.Token, n.TokenFile = "", "", ""
	case config.NotifyWebhook:
		n.URL = get("url")
		if v := f.Get(key("token")); v != "" {
			n.Token = v
		}
		n.BotToken, n.BotTokenFile, n.ChatID, n.ThreadID, n.Silent, n.APIURL = "", "", "", 0, false, ""
	}
	return n
}

// shortDuration renders 2m0s as 2m and 1h0m0s as 1h for the form.
func shortDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
