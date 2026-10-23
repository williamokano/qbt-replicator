package replicator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/qbt"
)

// ReplicaDownloadLimit (bytes/s) is applied to every replica. Replicas must
// never download: if the shared data were incomplete they would write into
// storage owned by another instance. 0 would mean unlimited in qBittorrent.
const ReplicaDownloadLimit = 1

// Result is the outcome of one executed action.
type Result struct {
	Action   Action        `json:"action"`
	Error    string        `json:"error,omitempty"`
	DryRun   bool          `json:"dry_run,omitempty"`
	Duration time.Duration `json:"duration"`
}

// Executor applies planned actions to qBittorrent instances.
type Executor struct {
	Clients     map[string]*qbt.Client
	Events      *events.Log
	Concurrency int
	DryRun      bool
	// PollInterval/AppearTimeout/CheckTimeout/VerifyTimeout tune waiting on qBittorrent.
	PollInterval  time.Duration
	AppearTimeout time.Duration
	CheckTimeout  time.Duration
	VerifyTimeout time.Duration

	exportMu sync.Mutex
	exports  map[string][]byte
}

// withDryRun returns an executor with the same settings and the given dry-run mode.
func (e *Executor) withDryRun(dry bool) *Executor {
	return &Executor{
		Clients: e.Clients, Events: e.Events, Concurrency: e.Concurrency, DryRun: dry,
		PollInterval: e.PollInterval, AppearTimeout: e.AppearTimeout, CheckTimeout: e.CheckTimeout,
		VerifyTimeout: e.VerifyTimeout,
	}
}

// Execute runs actions with bounded concurrency and returns results in order.
func (e *Executor) Execute(ctx context.Context, actions []Action) []Result {
	e.exportMu.Lock()
	e.exports = map[string][]byte{}
	e.exportMu.Unlock()

	conc := max(e.Concurrency, 1)
	results := make([]Result, len(actions))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, a := range actions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = Result{Action: a, Error: ctx.Err().Error()}
				return
			}
			defer func() { <-sem }()
			results[i] = e.run(ctx, a)
		}()
	}
	wg.Wait()
	return results
}

func (e *Executor) run(ctx context.Context, a Action) Result {
	start := time.Now()
	res := Result{Action: a, DryRun: e.DryRun}
	if e.DryRun {
		e.Events.Add(events.Event{Level: events.Info, Kind: "dry-run", Instance: a.Target, Hash: a.Hash, Name: a.Name,
			Message: "would " + a.String()})
		return res
	}
	err := e.apply(ctx, a)
	res.Duration = time.Since(start)
	if err != nil {
		res.Error = err.Error()
		e.Events.Add(events.Event{Level: events.Error, Kind: string(a.Type), Instance: a.Target, Hash: a.Hash, Name: a.Name,
			Message: fmt.Sprintf("%s failed: %v", a.Type, err)})
		return res
	}
	e.Events.Add(events.Event{Level: events.Info, Kind: string(a.Type), Instance: a.Target, Hash: a.Hash, Name: a.Name,
		Message: a.String()})
	return res
}

func (e *Executor) client(name string) (*qbt.Client, error) {
	c, ok := e.Clients[name]
	if !ok {
		return nil, fmt.Errorf("unknown instance %q", name)
	}
	return c, nil
}

func (e *Executor) apply(ctx context.Context, a Action) error {
	tgt, err := e.client(a.Target)
	if err != nil {
		return err
	}
	switch a.Type {
	case ActionAdd:
		return e.add(ctx, tgt, a)
	case ActionReadd:
		if err := tgt.Remove(ctx, a.Hash); err != nil {
			return fmt.Errorf("removing old replica: %w", err)
		}
		if err := e.waitGone(ctx, tgt, a.Hash); err != nil {
			return err
		}
		return e.add(ctx, tgt, a)
	case ActionRemove:
		return tgt.Remove(ctx, a.Hash)
	case ActionRecheck:
		return tgt.Recheck(ctx, a.Hash)
	case ActionStop:
		return tgt.Stop(ctx, a.Hash)
	case ActionSyncLimits:
		return tgt.SetShareLimits(ctx, a.Hash, a.RatioLimit, a.SeedingTimeLimit, a.InactiveSeedingTime)
	default:
		return fmt.Errorf("unknown action %q", a.Type)
	}
}

func (e *Executor) export(ctx context.Context, src *qbt.Client, hash string) ([]byte, error) {
	e.exportMu.Lock()
	b, ok := e.exports[hash]
	e.exportMu.Unlock()
	if ok {
		return b, nil
	}
	b, err := src.Export(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("exporting from %s: %w", src.Name(), err)
	}
	e.exportMu.Lock()
	e.exports[hash] = b
	e.exportMu.Unlock()
	return b, nil
}

// add implements: export -> add stopped -> wait -> file priorities -> renames
// -> limits -> (recheck) -> start -> verify.
func (e *Executor) add(ctx context.Context, tgt *qbt.Client, a Action) error {
	src, err := e.client(a.Source)
	if err != nil {
		return err
	}
	data, err := e.export(ctx, src, a.Hash)
	if err != nil {
		return err
	}
	if a.Category != "" {
		if err := tgt.EnsureCategory(ctx, a.Category); err != nil {
			return fmt.Errorf("ensuring category %q: %w", a.Category, err)
		}
	}
	opts := qbt.AddOptions{
		SavePath: a.SavePath, Category: a.Category, Tags: a.Tags,
		Stopped: true, SkipChecking: a.SkipChecking, DownloadLimit: ReplicaDownloadLimit,
	}
	if err := tgt.AddTorrent(ctx, data, a.Hash, opts); err != nil {
		if !errors.Is(err, qbt.ErrAddFailed) {
			return err
		}
		// "Fails." usually means it already exists; verify.
		if _, gerr := tgt.Torrent(ctx, a.Hash); gerr != nil {
			return err
		}
		return nil
	}
	if err := e.waitAppear(ctx, tgt, a.Hash); err != nil {
		return err
	}
	if err := syncFiles(ctx, src, tgt, a.Hash); err != nil {
		e.stopQuietly(tgt, a.Hash)
		return err
	}
	if a.SyncLimits {
		if err := tgt.SetShareLimits(ctx, a.Hash, a.RatioLimit, a.SeedingTimeLimit, a.InactiveSeedingTime); err != nil {
			return fmt.Errorf("setting share limits: %w", err)
		}
	}
	if !a.SkipChecking {
		// Full verification while stopped: nothing can be downloaded during the check.
		if err := tgt.Recheck(ctx, a.Hash); err != nil {
			return fmt.Errorf("rechecking: %w", err)
		}
		if err := e.waitChecked(ctx, tgt, a.Hash); err != nil {
			return err
		}
		t, err := tgt.Torrent(ctx, a.Hash)
		if err != nil {
			return err
		}
		if t.Progress < 1 {
			return fmt.Errorf("replica only %.1f%% complete on shared storage; left stopped to avoid downloading", t.Progress*100)
		}
		if !a.Start {
			return nil
		}
		if err := tgt.Start(ctx, a.Hash); err != nil {
			return fmt.Errorf("starting: %w", err)
		}
		return nil
	}
	// skip_checking: qBittorrent only enters seed mode once the torrent runs and
	// reports 0% while stopped, so completeness can only be verified after start.
	// The replica's download limit (ReplicaDownloadLimit) bounds what it could
	// fetch in that window.
	if !a.Start {
		return nil
	}
	if err := tgt.Start(ctx, a.Hash); err != nil {
		return fmt.Errorf("starting: %w", err)
	}
	if err := e.waitComplete(ctx, tgt, a.Hash); err != nil {
		e.stopQuietly(tgt, a.Hash)
		return fmt.Errorf("%w; replica stopped to avoid downloading", err)
	}
	return nil
}

// syncFiles mirrors skipped files (priority 0) and renamed paths from source.
func syncFiles(ctx context.Context, src, tgt *qbt.Client, hash string) error {
	sf, err := src.Files(ctx, hash)
	if err != nil {
		return fmt.Errorf("listing source files: %w", err)
	}
	tf, err := tgt.Files(ctx, hash)
	if err != nil {
		return fmt.Errorf("listing replica files: %w", err)
	}
	byIdx := map[int]qbt.File{}
	for _, f := range tf {
		byIdx[f.Idx()] = f
	}
	var skip []int
	for _, f := range sf {
		t, ok := byIdx[f.Idx()]
		if !ok {
			return fmt.Errorf("file index %d missing on replica", f.Idx())
		}
		if f.Priority == 0 && t.Priority != 0 {
			skip = append(skip, f.Idx())
		}
		if f.Name != t.Name {
			if err := tgt.RenameFile(ctx, hash, t.Name, f.Name); err != nil {
				return fmt.Errorf("renaming %q -> %q: %w", t.Name, f.Name, err)
			}
		}
	}
	if len(skip) > 0 {
		if err := tgt.SetFilePriority(ctx, hash, skip, 0); err != nil {
			return fmt.Errorf("setting file priorities: %w", err)
		}
	}
	return nil
}

func (e *Executor) stopQuietly(c *qbt.Client, hash string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.Stop(ctx, hash)
}

func (e *Executor) poll(ctx context.Context, timeout time.Duration, what string, done func() (bool, error)) error {
	interval := e.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		ok, err := done()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (e *Executor) waitAppear(ctx context.Context, c *qbt.Client, hash string) error {
	return e.poll(ctx, orDefault(e.AppearTimeout, 30*time.Second), "torrent to appear", func() (bool, error) {
		_, err := c.Torrent(ctx, hash)
		if errors.Is(err, qbt.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	})
}

func (e *Executor) waitGone(ctx context.Context, c *qbt.Client, hash string) error {
	return e.poll(ctx, orDefault(e.AppearTimeout, 30*time.Second), "torrent removal", func() (bool, error) {
		_, err := c.Torrent(ctx, hash)
		if errors.Is(err, qbt.ErrNotFound) {
			return true, nil
		}
		return false, err
	})
}

func (e *Executor) waitComplete(ctx context.Context, c *qbt.Client, hash string) error {
	var last qbt.Torrent
	err := e.poll(ctx, orDefault(e.VerifyTimeout, 30*time.Second), "replica to report 100%", func() (bool, error) {
		t, err := c.Torrent(ctx, hash)
		if err != nil {
			return false, err
		}
		last = t
		return t.Progress >= 1 && !strings.HasPrefix(t.State, "checking"), nil
	})
	if err != nil && last.Hash != "" {
		return fmt.Errorf("replica at %.1f%% (state %s): %w", last.Progress*100, last.State, err)
	}
	return err
}

func (e *Executor) waitChecked(ctx context.Context, c *qbt.Client, hash string) error {
	return e.poll(ctx, orDefault(e.CheckTimeout, 6*time.Hour), "recheck", func() (bool, error) {
		t, err := c.Torrent(ctx, hash)
		if err != nil {
			return false, err
		}
		return !strings.HasPrefix(t.State, "checking") && t.State != qbt.StateMoving, nil
	})
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
