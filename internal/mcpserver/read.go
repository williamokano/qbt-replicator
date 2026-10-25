package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/replicator"
	"github.com/williamokano/qbt-replicator/internal/store"
)

// Outputs are compact on purpose: they go into a model's context.

func rate(bps int64) string {
	const unit = 1024
	if bps < unit {
		return fmt.Sprintf("%d B/s", bps)
	}
	div, exp := int64(unit), 0
	for n := bps / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB/s", float64(bps)/float64(div), "KMGTPE"[exp])
}

func size(b int64) string { return rate(b)[:len(rate(b))-2] }

func limitOr(n, def, maxN int) int {
	if n <= 0 {
		return def
	}
	return min(n, maxN)
}

type instanceOut struct {
	Name       string `json:"name"`
	Healthy    bool   `json:"healthy"`
	Error      string `json:"error,omitempty"`
	Version    string `json:"qbittorrent_version,omitempty"`
	Connection string `json:"connection_status,omitempty" jsonschema:"connected, firewalled (no forwarded port) or disconnected"`
	Upload     string `json:"upload_speed"`
	Torrents   int    `json:"torrents"`
	Sources    int    `json:"sources"`
	Replicas   int    `json:"replicas"`
}

type statusOut struct {
	Version        string        `json:"version"`
	DryRun         bool          `json:"dry_run" jsonschema:"true when the daemon only logs what it would do"`
	DaemonEnabled  bool          `json:"daemon_enabled"`
	Interval       string        `json:"poll_interval"`
	Instances      []instanceOut `json:"instances"`
	TotalUpload    string        `json:"total_upload_speed"`
	LastRunAt      *time.Time    `json:"last_run_at,omitempty"`
	LastRunActions int           `json:"last_run_actions"`
	LastRunFailed  int           `json:"last_run_failed"`
	PendingActions int           `json:"pending_actions"`
}

type torrentOut struct {
	Hash     string            `json:"hash"`
	Name     string            `json:"name"`
	Size     string            `json:"size"`
	Category string            `json:"category,omitempty"`
	Complete bool              `json:"complete"`
	Excluded bool              `json:"excluded,omitempty" jsonschema:"tagged with the exclude tag: no rule replicates it"`
	On       map[string]string `json:"on" jsonschema:"instance -> role and state, e.g. source stalledUP or replica uploading; instances without the torrent are omitted"`
	Upload   string            `json:"upload_speed"`
}

func toTorrent(r app.TorrentRow) torrentOut {
	on := map[string]string{}
	for inst, c := range r.Cells {
		if !c.Present {
			continue
		}
		role := "source"
		if c.Replica {
			role = "replica"
		}
		v := role + " " + c.State
		if c.Progress < 1 {
			v += fmt.Sprintf(" %.0f%%", c.Progress*100)
		}
		on[inst] = v
	}
	return torrentOut{Hash: r.Hash, Name: r.Name, Size: size(r.Size), Category: r.Category, Complete: r.Complete,
		Excluded: r.Excluded, On: on, Upload: rate(r.TotalUp)}
}

type actionOut struct {
	Type   string `json:"type"`
	Name   string `json:"torrent"`
	Hash   string `json:"hash"`
	Source string `json:"source,omitempty"`
	Target string `json:"target"`
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason"`
}

func toAction(a replicator.Action) actionOut {
	return actionOut{Type: string(a.Type), Name: a.Name, Hash: a.Hash, Source: a.Source, Target: a.Target, Rule: a.Rule, Reason: a.Reason}
}

type skipOut struct {
	Name   string `json:"torrent"`
	Hash   string `json:"hash"`
	Source string `json:"source"`
	Target string `json:"target,omitempty" jsonschema:"empty: every target of the rule"`
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
}

func toSkip(s replicator.Skip) skipOut {
	return skipOut{Name: s.Name, Hash: s.Hash, Source: s.Source, Target: s.Target, Rule: s.Rule, Reason: s.Reason}
}

type historyOut struct {
	Time   time.Time `json:"time"`
	Type   string    `json:"type"`
	Result string    `json:"result" jsonschema:"ok or error"`
	Name   string    `json:"torrent"`
	Hash   string    `json:"hash"`
	Source string    `json:"source,omitempty"`
	Target string    `json:"target"`
	Reason string    `json:"reason,omitempty"`
	Error  string    `json:"error,omitempty"`
}

func toHistory(a store.ActionRecord) historyOut {
	return historyOut{Time: a.Time, Type: a.Type, Result: a.Result, Name: a.Name, Hash: a.Hash, Source: a.Source,
		Target: a.Target, Reason: a.Reason, Error: a.Error}
}

type eventOut struct {
	Time     time.Time `json:"time"`
	Level    string    `json:"level"`
	Kind     string    `json:"kind"`
	Instance string    `json:"instance,omitempty"`
	Name     string    `json:"torrent,omitempty"`
	Message  string    `json:"message"`
}

// --- inputs ---

type emptyIn struct{}

type searchIn struct {
	Query     string `json:"query,omitempty" jsonschema:"fuzzy match on the torrent name, or a hash prefix; empty lists all"`
	Instance  string `json:"instance,omitempty" jsonschema:"only torrents present on this instance"`
	Category  string `json:"category,omitempty"`
	Completed bool   `json:"completed,omitempty" jsonschema:"only completed torrents"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max results (default 20, max 100)"`
}

type searchOut struct {
	Total    int          `json:"total" jsonschema:"matches before the limit"`
	Torrents []torrentOut `json:"torrents"`
}

type torrentIn struct {
	Torrent string `json:"torrent" jsonschema:"torrent hash, hash prefix or name"`
}

type torrentDetailOut struct {
	Torrent  torrentOut   `json:"torrent"`
	Planned  []actionOut  `json:"planned_actions" jsonschema:"what the next pass will do with it"`
	Skipped  []skipOut    `json:"skipped" jsonschema:"why it is not replicated to some targets"`
	History  []historyOut `json:"history" jsonschema:"actions taken on it, newest first"`
	Activity []eventOut   `json:"activity" jsonschema:"activity log entries about it, newest first"`
}

type planIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"max actions and skips each (default 50, max 200)"`
}

type planOut struct {
	Actions   []actionOut `json:"actions"`
	Skipped   []skipOut   `json:"skipped" jsonschema:"completed source torrents not replicated, with the reason"`
	Notes     []string    `json:"notes" jsonschema:"decisions not taken, e.g. orphan confirmations, retry backoff"`
	Truncated bool        `json:"truncated,omitempty"`
}

type historyIn struct {
	Search   string `json:"search,omitempty" jsonschema:"torrent name substring or hash prefix"`
	Instance string `json:"instance,omitempty" jsonschema:"source or target instance"`
	Type     string `json:"type,omitempty" jsonschema:"add, readd, remove, recheck, stop, sync_limits, exclude or include"`
	Result   string `json:"result,omitempty" jsonschema:"ok or error"`
	Since    string `json:"since,omitempty" jsonschema:"how far back, e.g. 24h or 7d"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max results (default 20, max 200)"`
}

type historyListOut struct {
	Actions []historyOut `json:"actions"`
}

type activityIn struct {
	Level    string `json:"level,omitempty" jsonschema:"info, warn or error"`
	Instance string `json:"instance,omitempty"`
	Torrent  string `json:"torrent,omitempty" jsonschema:"torrent hash, hash prefix or name"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max results (default 20, max 200)"`
}

type activityOut struct {
	Events []eventOut `json:"events"`
}

func (t *tools) addRead(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "get_status", Title: "Status", Annotations: readOnly,
		Description: "Health of every qBittorrent instance (reachable, VPN port connected/firewalled, upload speed, torrents/replicas), the last replication pass and pending actions."},
		handler(t, false, func(ctx context.Context, _ emptyIn) (statusOut, error) {
			st, err := t.b.Status(ctx, false)
			if err != nil {
				return statusOut{}, err
			}
			out := statusOut{Version: st.Version, DryRun: st.DryRun, DaemonEnabled: st.DaemonOn, Interval: st.Interval,
				TotalUpload: rate(st.TotalUp), PendingActions: st.PendingCount, Instances: []instanceOut{}}
			for _, in := range st.Instances {
				out.Instances = append(out.Instances, instanceOut{Name: in.Name, Healthy: in.Healthy, Error: in.Error,
					Version: in.AppVersion, Connection: in.ConnectionStatus, Upload: rate(in.UpSpeed),
					Torrents: in.Torrents, Sources: in.Sources, Replicas: in.Replicas})
			}
			if r := st.LastRun; r != nil {
				out.LastRunAt, out.LastRunActions, out.LastRunFailed = &r.Started, len(r.Results), r.Failed
			}
			return out, nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "search_torrents", Title: "Search torrents", Annotations: readOnly,
		Description: "Find torrents across all instances by name (fuzzy) or hash prefix; shows on which instances each one is a source or a replica."},
		handler(t, false, func(ctx context.Context, in searchIn) (searchOut, error) {
			rows, err := t.b.Torrents(ctx, app.TorrentFilter{Search: in.Query, Instance: in.Instance, Category: in.Category, Completed: in.Completed})
			if err != nil {
				return searchOut{}, err
			}
			out := searchOut{Total: len(rows), Torrents: []torrentOut{}}
			for i, r := range rows {
				if i == limitOr(in.Limit, 20, 100) {
					break
				}
				out.Torrents = append(out.Torrents, toTorrent(r))
			}
			return out, nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "get_torrent", Title: "Torrent details", Annotations: readOnly,
		Description: "Everything about one torrent: where it is, what the next pass will do with it, why it is skipped for some targets, and its history and activity. Use it to answer \"why isn't X replicated?\"."},
		handler(t, false, func(ctx context.Context, in torrentIn) (torrentDetailOut, error) {
			row, err := t.resolve(ctx, in.Torrent)
			if err != nil {
				return torrentDetailOut{}, err
			}
			out := torrentDetailOut{Torrent: toTorrent(row), Planned: []actionOut{}, Skipped: []skipOut{}, History: []historyOut{}, Activity: []eventOut{}}
			plan, err := t.b.Plan(ctx)
			if err != nil {
				return out, err
			}
			for _, a := range plan.Actions {
				if a.Hash == row.Hash {
					out.Planned = append(out.Planned, toAction(a))
				}
			}
			for _, sk := range plan.Skipped {
				if sk.Hash == row.Hash {
					out.Skipped = append(out.Skipped, toSkip(sk))
				}
			}
			hist, err := t.b.History(ctx, store.ActionFilter{Hash: row.Hash, Limit: 20})
			if err != nil {
				return out, err
			}
			for _, a := range hist {
				out.History = append(out.History, toHistory(a))
			}
			evs, err := t.b.Events(ctx, store.EventFilter{Hash: row.Hash, Limit: 20})
			if err != nil {
				return out, err
			}
			for _, e := range evs {
				out.Activity = append(out.Activity, eventOut{Time: e.Time, Level: string(e.Level), Kind: e.Kind, Instance: e.Instance, Message: e.Message})
			}
			return out, nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "get_plan", Title: "Planned actions", Annotations: readOnly,
		Description: "What the next replication pass would do, which completed torrents are not replicated and why, and notes such as orphan confirmations and retry backoff."},
		handler(t, false, func(ctx context.Context, in planIn) (planOut, error) {
			p, err := t.b.Plan(ctx)
			if err != nil {
				return planOut{}, err
			}
			n := limitOr(in.Limit, 50, 200)
			out := planOut{Actions: []actionOut{}, Skipped: []skipOut{}, Notes: p.Notes}
			if out.Notes == nil {
				out.Notes = []string{}
			}
			for i, a := range p.Actions {
				if i == n {
					out.Truncated = true
					break
				}
				out.Actions = append(out.Actions, toAction(a))
			}
			for i, sk := range p.Skipped {
				if i == n {
					out.Truncated = true
					break
				}
				out.Skipped = append(out.Skipped, toSkip(sk))
			}
			return out, nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "get_history", Title: "History", Annotations: readOnly,
		Description: "Actions qbt-replicator executed (replicas added, removed, failures…), newest first, with filters."},
		handler(t, false, func(ctx context.Context, in historyIn) (historyListOut, error) {
			f := store.ActionFilter{Search: in.Search, Instance: in.Instance, Type: in.Type, Result: in.Result, Limit: limitOr(in.Limit, 20, 200)}
			if in.Since != "" {
				d, err := config.ParseDays(in.Since)
				if err != nil {
					return historyListOut{}, err
				}
				f.Since = time.Now().Add(-d)
			}
			acts, err := t.b.History(ctx, f)
			if err != nil {
				return historyListOut{}, err
			}
			out := historyListOut{Actions: []historyOut{}}
			for _, a := range acts {
				out.Actions = append(out.Actions, toHistory(a))
			}
			return out, nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "get_activity", Title: "Activity log", Annotations: readOnly,
		Description: "The activity log (instances going down/up, errors, actions), newest first."},
		handler(t, false, func(ctx context.Context, in activityIn) (activityOut, error) {
			f := store.EventFilter{Level: in.Level, Instance: in.Instance, Limit: limitOr(in.Limit, 20, 200)}
			if in.Torrent != "" {
				row, err := t.resolve(ctx, in.Torrent)
				if err != nil {
					return activityOut{}, err
				}
				f.Hash = row.Hash
			}
			evs, err := t.b.Events(ctx, f)
			if err != nil {
				return activityOut{}, err
			}
			out := activityOut{Events: []eventOut{}}
			for _, e := range evs {
				out.Events = append(out.Events, eventOut{Time: e.Time, Level: string(e.Level), Kind: e.Kind, Instance: e.Instance, Name: e.Name, Message: e.Message})
			}
			return out, nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "get_config", Title: "Configuration", Annotations: readOnly,
		Description: "Instances (URLs, no secrets), replication rules with their filters, daemon and notification settings. Secrets are always masked."},
		handler(t, false, func(ctx context.Context, _ emptyIn) (configOut, error) {
			cfg, err := t.b.Config(ctx)
			if err != nil {
				return configOut{}, err
			}
			return toConfig(cfg), nil
		}))
}

// configOut is the configuration without secrets or rarely useful fields.
type configOut struct {
	Daemon        map[string]any   `json:"daemon"`
	Instances     []map[string]any `json:"instances"`
	Rules         []map[string]any `json:"rules"`
	Notifications []map[string]any `json:"notifications"`
}

func toConfig(c config.Config) configOut {
	out := configOut{
		Daemon: map[string]any{"enabled": c.Daemon.Enabled, "interval": c.Daemon.Interval.String(), "dry_run": c.Daemon.DryRun,
			"replica_tag": c.Daemon.ReplicaTag, "exclude_tag": c.Daemon.ExcludeTag, "orphan_grace_ticks": c.Daemon.OrphanGraceTicks,
			"orphan_grace_period": c.Daemon.OrphanGracePeriod.String()},
		Instances: []map[string]any{}, Rules: []map[string]any{}, Notifications: []map[string]any{},
	}
	for _, in := range c.Instances {
		auth := "none"
		switch {
		case in.APIKey != "" || in.APIKeyFile != "":
			auth = "api_key"
		case in.Username != "":
			auth = "password"
		}
		m := map[string]any{"name": in.Name, "url": in.URL, "auth": auth}
		if len(in.PathMappings) > 0 {
			m["path_mappings"] = in.PathMappings
		}
		out.Instances = append(out.Instances, m)
	}
	for _, r := range c.Rules {
		out.Rules = append(out.Rules, map[string]any{"name": r.Name, "sources": r.Sources, "targets": r.Targets,
			"settle_delay": r.SettleDelay.String(), "filter": r.Filter, "add": r.Add, "remove_orphans": r.RemoveOrphans,
			"follow_relocation": r.FollowRelocation, "sync_share_limits": r.SyncShareLimits})
	}
	for _, n := range c.Notifications {
		events := n.Events
		if len(events) == 0 {
			events = config.NotificationEvents
		}
		sorted := append([]string(nil), events...)
		sort.Strings(sorted)
		out.Notifications = append(out.Notifications, map[string]any{"name": n.Name, "type": n.Type, "enabled": n.Enabled,
			"events": sorted, "digest": n.Digest.String(), "daily_summary": n.DailySummary})
	}
	return out
}
