package replicator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/qbt"
)

// RunReport summarizes one reconcile pass.
type RunReport struct {
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	DryRun   bool          `json:"dry_run"`
	Plan     *Plan         `json:"plan"`
	Results  []Result      `json:"results"`
	Failed   int           `json:"failed"`
}

// Engine owns clients, planner memory and the reconcile loop.
type Engine struct {
	cfg     *config.Config
	clients map[string]*qbt.Client
	events  *events.Log
	metrics *Metrics
	exec    *Executor

	runMu   sync.Mutex // serializes reconcile passes and manual actions
	stateMu sync.RWMutex
	memory  *Memory
	last    *RunReport
	lastSn  *Snapshot
	trigger chan struct{}
}

// NewEngine builds clients for every configured instance.
func NewEngine(cfg *config.Config, log *events.Log, metrics *Metrics) (*Engine, error) {
	clients := map[string]*qbt.Client{}
	for _, in := range cfg.Instances {
		c, err := qbt.New(qbt.Options{
			Name: in.Name, BaseURL: in.URL, Username: in.Username, Password: in.Password,
			InsecureSkipVerify: in.InsecureSkipVerify, Timeout: in.Timeout,
			ExportFallbackDir: in.ExportFallbackDir,
		})
		if err != nil {
			return nil, fmt.Errorf("instance %q: %w", in.Name, err)
		}
		clients[in.Name] = c
	}
	if metrics == nil {
		metrics = NewMetrics()
	}
	return &Engine{
		cfg: cfg, clients: clients, events: log, metrics: metrics,
		exec: &Executor{Clients: clients, Events: log, Concurrency: cfg.Daemon.Concurrency,
			DryRun: cfg.Daemon.DryRun},
		memory:  loadState(cfg.StatePath(), log),
		trigger: make(chan struct{}, 1),
	}, nil
}

// loadState reads persisted planner memory; any problem yields empty memory.
func loadState(path string, log *events.Log) *Memory {
	mem := NewMemory()
	if path == "" {
		return mem
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Add(events.Event{Level: events.Warn, Kind: "state", Message: "reading state file: " + err.Error()})
		}
		return mem
	}
	if err := json.Unmarshal(b, mem); err != nil {
		log.Add(events.Event{Level: events.Warn, Kind: "state", Message: "ignoring corrupt state file: " + err.Error()})
		return NewMemory()
	}
	if mem.Orphans == nil {
		mem.Orphans = map[string]int{}
	}
	if mem.Rechecks == nil {
		mem.Rechecks = map[string]bool{}
	}
	return mem
}

// saveState persists planner memory atomically (best effort). Caller holds stateMu.
func (e *Engine) saveState() {
	path := e.cfg.StatePath()
	if path == "" {
		return
	}
	b, err := json.Marshal(e.memory)
	if err == nil {
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		e.events.Add(events.Event{Level: events.Warn, Kind: "state", Message: "writing state file: " + err.Error()})
	}
}

// Config returns the active configuration.
func (e *Engine) Config() *config.Config { return e.cfg }

// Events returns the activity log.
func (e *Engine) Events() *events.Log { return e.events }

// Metrics returns the Prometheus metrics.
func (e *Engine) Metrics() *Metrics { return e.metrics }

// Client returns the client for an instance.
func (e *Engine) Client(name string) (*qbt.Client, bool) {
	c, ok := e.clients[name]
	return c, ok
}

// Executor exposes the executor (tests tune poll timings).
func (e *Engine) Executor() *Executor { return e.exec }

// Snapshot fetches fresh state of all instances.
func (e *Engine) Snapshot(ctx context.Context) *Snapshot {
	snap := TakeSnapshot(ctx, e.clients, e.cfg.InstanceNames(), e.cfg.Daemon.ReplicaTag)
	e.metrics.observeSnapshot(snap, e.cfg.Daemon.ReplicaTag)
	e.stateMu.Lock()
	e.lastSn = snap
	e.stateMu.Unlock()
	return snap
}

// LastSnapshot returns the most recent snapshot (nil before the first).
func (e *Engine) LastSnapshot() *Snapshot {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.lastSn
}

// LastRun returns the most recent reconcile report (nil before the first).
func (e *Engine) LastRun() *RunReport {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.last
}

// Plan computes what a pass would do now without executing or committing memory.
func (e *Engine) Plan(ctx context.Context) (*Plan, *Snapshot) {
	snap := e.Snapshot(ctx)
	e.stateMu.RLock()
	mem := e.memory
	e.stateMu.RUnlock()
	return Compute(e.cfg, snap, mem, time.Now()), snap
}

// RunOnce performs one reconcile pass. dryRun is OR-ed with the configured dry_run.
func (e *Engine) RunOnce(ctx context.Context, dryRun bool) *RunReport {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	start := time.Now()
	dry := dryRun || e.cfg.Daemon.DryRun
	snap := e.Snapshot(ctx)
	for _, name := range snap.Order {
		if st := snap.Instances[name]; !st.Healthy {
			e.events.Add(events.Event{Level: events.Warn, Kind: "health", Instance: name, Message: "instance unreachable: " + st.Error})
		}
	}
	e.stateMu.RLock()
	mem := e.memory
	e.stateMu.RUnlock()
	plan := Compute(e.cfg, snap, mem, time.Now())

	exec := e.exec.withDryRun(dry)
	results := exec.Execute(ctx, plan.Actions)

	rep := &RunReport{Started: start, DryRun: dry, Plan: plan, Results: results, Duration: time.Since(start)}
	for _, r := range results {
		if r.Error != "" {
			rep.Failed++
		}
	}
	e.metrics.observeRun(rep)
	e.stateMu.Lock()
	if !dry {
		e.memory = plan.Memory
		e.saveState()
	}
	e.last = rep
	e.stateMu.Unlock()
	return rep
}

// Trigger requests an immediate pass from the daemon loop (non-blocking).
func (e *Engine) Trigger() {
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}

// Run executes the reconcile loop until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.events.Add(events.Event{Level: events.Info, Kind: "daemon",
		Message: fmt.Sprintf("daemon started (interval %s, dry_run %v)", e.cfg.Daemon.Interval, e.cfg.Daemon.DryRun)})
	t := time.NewTicker(e.cfg.Daemon.Interval)
	defer t.Stop()
	for {
		e.RunOnce(ctx, false)
		select {
		case <-ctx.Done():
			e.events.Add(events.Event{Level: events.Info, Kind: "daemon", Message: "daemon stopped"})
			return
		case <-t.C:
		case <-e.trigger:
		}
	}
}

// ErrNotReplica is returned when trying to unreplicate a torrent the replicator does not own.
var ErrNotReplica = errors.New("torrent is not a replica (missing replica tag); refusing to remove")

// ReplicateRequest describes a manual replication.
type ReplicateRequest struct {
	Hash string
	From string   // empty: first instance owning the torrent
	To   []string // empty: every other instance
	// Force skips rule filters and the settle delay (completion is still required).
	Force  bool
	DryRun bool
}

// Replicate manually replicates one torrent.
func (e *Engine) Replicate(ctx context.Context, req ReplicateRequest) ([]Result, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	snap := e.Snapshot(ctx)
	tag := e.cfg.Daemon.ReplicaTag
	from := req.From
	if from == "" {
		// Prefer an owner that some rule uses as a source.
		for _, n := range snap.Order {
			st := snap.Instances[n]
			if t, ok := st.Torrents[req.Hash]; ok && st.Healthy && !t.HasTag(tag) {
				if from == "" || (len(ruleTargets(e.cfg, from)) == 0 && len(ruleTargets(e.cfg, n)) > 0) {
					from = n
				}
			}
		}
		if from == "" {
			return nil, fmt.Errorf("torrent %s not found as a non-replica on any reachable instance", req.Hash)
		}
	}
	src, ok := snap.Instances[from]
	if !ok {
		return nil, fmt.Errorf("unknown instance %q", from)
	}
	if !src.Healthy {
		return nil, fmt.Errorf("instance %q is unreachable: %s", from, src.Error)
	}
	t, ok := src.Torrents[req.Hash]
	if !ok {
		return nil, fmt.Errorf("torrent %s not found on %s", req.Hash, from)
	}
	if t.HasTag(tag) {
		return nil, fmt.Errorf("torrent on %s is itself a replica; replicate from its source instead", from)
	}
	if t.Progress < 1 {
		return nil, fmt.Errorf("torrent is only %.1f%% complete on %s", t.Progress*100, from)
	}
	rule, hasRule := config.RuleConfig{}, false
	for _, r := range e.cfg.Rules {
		if slices.Contains(e.cfg.Expand(r.Sources), from) {
			rule, hasRule = r, true
			break
		}
	}
	if !hasRule {
		rule = defaultManualRule()
	}
	if !req.Force {
		if ok, why := Eligible(e.cfg, rule, src, t, time.Now()); !ok {
			return nil, fmt.Errorf("not eligible under rule %q: %s (use force to override)", rule.Name, why)
		}
	}
	// Without explicit targets, follow the rules: only the targets of rules
	// whose sources include this instance. Explicit targets are honored as asked.
	targets := req.To
	if len(targets) == 0 {
		targets = ruleTargets(e.cfg, from)
		if len(targets) == 0 {
			return nil, fmt.Errorf("no rule replicates from %s; choose the target instances explicitly", from)
		}
	}
	srcCfg, _ := e.cfg.Instance(from)
	var actions []Action
	for _, tn := range targets {
		tgtCfg, ok := e.cfg.Instance(tn)
		if !ok {
			return nil, fmt.Errorf("unknown instance %q", tn)
		}
		if tn == from {
			continue
		}
		if st := snap.Instances[tn]; st != nil && st.Healthy {
			if _, present := st.Torrents[req.Hash]; present {
				continue
			}
		}
		addRule := rule
		if r, ok := ruleFor(e.cfg, from, tn); ok {
			addRule = r
		}
		actions = append(actions, addAction(ActionAdd, e.cfg, addRule, srcCfg, tgtCfg, t, "manual replication"))
	}
	exec := e.exec.withDryRun(req.DryRun || e.cfg.Daemon.DryRun)
	return exec.Execute(ctx, actions), nil
}

// ruleTargets returns the union of targets of every rule whose sources include src.
func ruleTargets(cfg *config.Config, src string) []string {
	var out []string
	for _, r := range cfg.Rules {
		if !slices.Contains(cfg.Expand(r.Sources), src) {
			continue
		}
		for _, t := range cfg.Expand(r.Targets, src) {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	return out
}

func defaultManualRule() config.RuleConfig {
	return config.RuleConfig{
		Name: "manual",
		Add:  config.AddConfig{Category: config.CategoryInherit, SkipChecking: true, Start: true},
	}
}

// UnreplicateRequest describes a manual replica removal.
type UnreplicateRequest struct {
	Hash string
	On   []string // empty: every instance holding a replica
	// Exclude also tags the source torrent(s) with daemon.exclude_tag first, so
	// no rule replicates it again until it is included again.
	Exclude bool
	DryRun  bool
}

// Unreplicate removes replicas of a torrent (files kept), optionally excluding
// the torrent from future replication.
func (e *Engine) Unreplicate(ctx context.Context, req UnreplicateRequest) ([]Result, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	snap := e.Snapshot(ctx)
	tag := e.cfg.Daemon.ReplicaTag
	hash, on := req.Hash, req.On
	if len(on) == 0 {
		for _, n := range snap.Order {
			if t, ok := snap.Instances[n].Torrents[hash]; ok && t.HasTag(tag) {
				on = append(on, n)
			}
		}
		if len(on) == 0 && !req.Exclude {
			return nil, fmt.Errorf("no replicas of %s found", hash)
		}
	}
	var removes []Action
	for _, n := range on {
		st, ok := snap.Instances[n]
		if !ok {
			return nil, fmt.Errorf("unknown instance %q", n)
		}
		if !st.Healthy {
			return nil, fmt.Errorf("instance %q is unreachable", n)
		}
		t, ok := st.Torrents[hash]
		if !ok {
			continue
		}
		if !t.HasTag(tag) {
			return nil, fmt.Errorf("%s on %s: %w", hash, n, ErrNotReplica)
		}
		removes = append(removes, Action{Type: ActionRemove, Hash: hash, Name: t.Name, Target: n, Reason: "manual unreplicate (files kept)"})
	}
	exec := e.exec.withDryRun(req.DryRun || e.cfg.Daemon.DryRun)
	var results []Result
	if req.Exclude {
		// Tag the sources first so a concurrent pass cannot re-add the replicas.
		excl, err := e.tagActions(snap, hash, ActionExclude)
		if err != nil {
			return nil, err
		}
		results = exec.Execute(ctx, excl)
		for _, r := range results {
			if r.Error != "" {
				return results, fmt.Errorf("excluding on %s failed, replicas kept: %s", r.Action.Target, r.Error)
			}
		}
	}
	return append(results, exec.Execute(ctx, removes)...), nil
}

// Include removes daemon.exclude_tag from the torrent's sources so rules apply again.
func (e *Engine) Include(ctx context.Context, hash string, dryRun bool) ([]Result, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	acts, err := e.tagActions(e.Snapshot(ctx), hash, ActionInclude)
	if err != nil {
		return nil, err
	}
	results := e.exec.withDryRun(dryRun || e.cfg.Daemon.DryRun).Execute(ctx, acts)
	e.Trigger()
	return results, nil
}

// tagActions builds exclude/include actions for every reachable source copy.
func (e *Engine) tagActions(snap *Snapshot, hash string, typ ActionType) ([]Action, error) {
	excl := e.cfg.Daemon.ExcludeTag
	var acts []Action
	for _, n := range snap.Order {
		st := snap.Instances[n]
		if st == nil || !st.Healthy {
			continue
		}
		t, ok := st.Torrents[hash]
		if !ok || t.HasTag(e.cfg.Daemon.ReplicaTag) || (typ == ActionExclude) == t.HasTag(excl) {
			continue
		}
		reason := "excluded from replication (tag " + excl + ")"
		if typ == ActionInclude {
			reason = "included in replication again (removed tag " + excl + ")"
		}
		acts = append(acts, Action{Type: typ, Hash: hash, Name: t.Name, Target: n, Tags: []string{excl}, Reason: reason})
	}
	if len(acts) == 0 {
		found := false
		for _, n := range snap.Order {
			if t, ok := snap.Instances[n].Torrents[hash]; ok && !t.HasTag(e.cfg.Daemon.ReplicaTag) {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("no source copy of %s found on a reachable instance", hash)
		}
	}
	return acts, nil
}
