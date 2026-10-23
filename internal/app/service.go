// Package app is the façade shared by the CLI and the web server: every
// user-facing operation is a method here, and both front-ends are thin adapters.
package app

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/replicator"
	"github.com/williamokano/qbt-replicator/internal/version"
)

// Service exposes replicator operations. The engine is swapped atomically when
// the configuration is changed from the UI/API (see Reload).
type Service struct {
	mu      sync.RWMutex
	saveMu  sync.Mutex
	engine  *replicator.Engine
	events  *events.Log
	metrics *replicator.Metrics
	reload  chan struct{}
	started time.Time
}

// New builds a Service from configuration.
func New(cfg *config.Config, logger *slog.Logger) (*Service, error) {
	s := &Service{
		events:  events.New(500, logger),
		metrics: replicator.NewMetrics(),
		reload:  make(chan struct{}, 1),
		started: time.Now(),
	}
	eng, err := replicator.NewEngine(cfg, s.events, s.metrics)
	if err != nil {
		return nil, err
	}
	s.engine = eng
	return s, nil
}

// Engine returns the current engine (daemon loop, metrics).
func (s *Service) Engine() *replicator.Engine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.engine
}

// Config returns the active configuration.
func (s *Service) Config() *config.Config { return s.Engine().Config() }

// InstanceStatus is a per-instance summary.
type InstanceStatus struct {
	Name             string `json:"name"`
	URL              string `json:"url"`
	Healthy          bool   `json:"healthy"`
	Error            string `json:"error,omitempty"`
	AppVersion       string `json:"app_version,omitempty"`
	APIVersion       string `json:"api_version,omitempty"`
	ConnectionStatus string `json:"connection_status,omitempty"`
	ListenPort       int    `json:"listen_port,omitempty"`
	UpSpeed          int64  `json:"up_speed"`
	DlSpeed          int64  `json:"dl_speed"`
	Torrents         int    `json:"torrents"`
	Replicas         int    `json:"replicas"`
	Sources          int    `json:"sources"`
}

// Status is the overall status.
type Status struct {
	Version      string                `json:"version"`
	StartedAt    time.Time             `json:"started_at"`
	DryRun       bool                  `json:"dry_run"`
	DaemonOn     bool                  `json:"daemon_enabled"`
	Interval     string                `json:"interval"`
	SnapshotAt   time.Time             `json:"snapshot_at"`
	Instances    []InstanceStatus      `json:"instances"`
	TotalUp      int64                 `json:"total_up_speed"`
	TotalDown    int64                 `json:"total_down_speed"`
	LastRun      *replicator.RunReport `json:"last_run,omitempty"`
	PendingCount int                   `json:"pending_actions"`
}

// Status fetches a fresh snapshot and summarizes it.
func (s *Service) Status(ctx context.Context) Status {
	return s.statusFrom(s.Engine().Snapshot(ctx))
}

// CachedStatus summarizes the last snapshot, fetching one if none exists yet.
func (s *Service) CachedStatus(ctx context.Context) Status {
	snap := s.Engine().LastSnapshot()
	if snap == nil {
		snap = s.Engine().Snapshot(ctx)
	}
	return s.statusFrom(snap)
}

func (s *Service) statusFrom(snap *replicator.Snapshot) Status {
	cfg := s.Config()
	st := Status{
		Version: version.Version, StartedAt: s.started, DryRun: cfg.Daemon.DryRun,
		DaemonOn: cfg.Daemon.Enabled, Interval: cfg.Daemon.Interval.String(),
		SnapshotAt: snap.At, LastRun: s.Engine().LastRun(),
	}
	if st.LastRun != nil && st.LastRun.Plan != nil {
		st.PendingCount = len(st.LastRun.Plan.Actions)
	}
	tag := cfg.Daemon.ReplicaTag
	for _, name := range snap.Order {
		in := snap.Instances[name]
		is := InstanceStatus{
			Name: name, URL: in.URL, Healthy: in.Healthy, Error: in.Error,
			AppVersion: in.AppVersion, APIVersion: in.APIVersion,
			ConnectionStatus: in.Transfer.ConnectionStatus, ListenPort: in.ListenPort,
			UpSpeed: in.Transfer.UpSpeed, DlSpeed: in.Transfer.DlSpeed, Torrents: len(in.Torrents),
		}
		for _, t := range in.Torrents {
			if t.HasTag(tag) {
				is.Replicas++
			} else {
				is.Sources++
			}
		}
		st.TotalUp += is.UpSpeed
		st.TotalDown += is.DlSpeed
		st.Instances = append(st.Instances, is)
	}
	return st
}

// Cell is one torrent on one instance.
type Cell struct {
	Present  bool    `json:"present"`
	Replica  bool    `json:"replica"`
	State    string  `json:"state,omitempty"`
	Progress float64 `json:"progress"`
	UpSpeed  int64   `json:"up_speed"`
	Ratio    float64 `json:"ratio"`
	SavePath string  `json:"save_path,omitempty"`
}

// TorrentRow is a torrent across all instances.
type TorrentRow struct {
	Hash     string          `json:"hash"`
	Name     string          `json:"name"`
	Size     int64           `json:"size"`
	Category string          `json:"category"`
	Complete bool            `json:"complete"`
	Cells    map[string]Cell `json:"instances"`
	TotalUp  int64           `json:"total_up_speed"`
	Copies   int             `json:"copies"`
}

// TorrentFilter narrows Torrents.
type TorrentFilter struct {
	Instance  string
	Search    string
	Category  string
	Completed bool
	Fresh     bool // force a new snapshot instead of the cached one
}

// Torrents returns the torrent × instance matrix.
func (s *Service) Torrents(ctx context.Context, f TorrentFilter) []TorrentRow {
	snap := s.Engine().LastSnapshot()
	if snap == nil || f.Fresh {
		snap = s.Engine().Snapshot(ctx)
	}
	tag := s.Config().Daemon.ReplicaTag
	rows := map[string]*TorrentRow{}
	for _, name := range snap.Order {
		in := snap.Instances[name]
		for h, t := range in.Torrents {
			r, ok := rows[h]
			if !ok {
				r = &TorrentRow{Hash: h, Cells: map[string]Cell{}}
				rows[h] = r
			}
			if r.Name == "" || !t.HasTag(tag) {
				r.Name, r.Size, r.Category = t.Name, max(t.TotalSize, t.Size), t.Category
			}
			if t.Progress >= 1 {
				r.Complete = true
			}
			r.Cells[name] = Cell{Present: true, Replica: t.HasTag(tag), State: t.State, Progress: t.Progress,
				UpSpeed: t.UpSpeed, Ratio: t.Ratio, SavePath: t.SavePath}
			r.TotalUp += t.UpSpeed
			r.Copies++
		}
	}
	out := make([]TorrentRow, 0, len(rows))
	search := strings.ToLower(f.Search)
	for _, r := range rows {
		if f.Instance != "" && !r.Cells[f.Instance].Present {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(r.Name), search) && !strings.HasPrefix(r.Hash, search) {
			continue
		}
		if f.Category != "" && r.Category != f.Category {
			continue
		}
		if f.Completed && !r.Complete {
			continue
		}
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b TorrentRow) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}

// Plan previews actions without executing them.
func (s *Service) Plan(ctx context.Context) *replicator.Plan {
	p, _ := s.Engine().Plan(ctx)
	return p
}

// SyncOnce runs one reconcile pass.
func (s *Service) SyncOnce(ctx context.Context, dryRun bool) *replicator.RunReport {
	return s.Engine().RunOnce(ctx, dryRun)
}

// TriggerSync asks the daemon loop to run now (non-blocking).
func (s *Service) TriggerSync() { s.Engine().Trigger() }

// Replicate manually replicates a torrent.
func (s *Service) Replicate(ctx context.Context, req replicator.ReplicateRequest) ([]replicator.Result, error) {
	return s.Engine().Replicate(ctx, req)
}

// Unreplicate removes replicas (files kept).
func (s *Service) Unreplicate(ctx context.Context, hash string, on []string, dryRun bool) ([]replicator.Result, error) {
	return s.Engine().Unreplicate(ctx, hash, on, dryRun)
}

// Events returns recent events, newest first.
func (s *Service) Events(limit int) []events.Event { return s.Engine().Events().Recent(limit) }

// ConfigRedacted returns the configuration with secrets masked.
func (s *Service) ConfigRedacted() config.Config { return s.Config().Redacted() }
