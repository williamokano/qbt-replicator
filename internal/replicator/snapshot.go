// Package replicator plans and executes the replication of completed torrents
// from source qBittorrent instances to target instances sharing the same storage.
package replicator

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/williamokano/qbt-replicator/internal/qbt"
)

// InstanceState is the observed state of one instance.
type InstanceState struct {
	Name       string                 `json:"name"`
	URL        string                 `json:"url"`
	Healthy    bool                   `json:"healthy"`
	Error      string                 `json:"error,omitempty"`
	AppVersion string                 `json:"app_version,omitempty"`
	APIVersion string                 `json:"api_version,omitempty"`
	Transfer   qbt.TransferInfo       `json:"transfer"`
	ListenPort int                    `json:"listen_port,omitempty"`
	Torrents   map[string]qbt.Torrent `json:"-"`
	// Private holds resolved private flags for completed, non-replica torrents.
	Private   map[string]bool `json:"-"`
	FetchedAt time.Time       `json:"fetched_at"`
}

// Snapshot is the observed state of all instances at one point in time.
type Snapshot struct {
	At        time.Time                 `json:"at"`
	Order     []string                  `json:"order"`
	Instances map[string]*InstanceState `json:"instances"`
}

// AllHealthy reports whether every instance was reachable.
func (s *Snapshot) AllHealthy() bool {
	for _, st := range s.Instances {
		if !st.Healthy {
			return false
		}
	}
	return true
}

// Hashes returns all torrent hashes seen across instances, sorted.
func (s *Snapshot) Hashes() []string {
	seen := map[string]bool{}
	for _, st := range s.Instances {
		for h := range st.Torrents {
			seen[h] = true
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// TakeSnapshot fetches every instance concurrently. Failures mark the
// instance unhealthy instead of failing the snapshot.
func TakeSnapshot(ctx context.Context, clients map[string]*qbt.Client, order []string, replicaTag string) *Snapshot {
	snap := &Snapshot{At: time.Now(), Order: order, Instances: map[string]*InstanceState{}}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, name := range order {
		c := clients[name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := fetchInstance(ctx, c, replicaTag)
			mu.Lock()
			snap.Instances[name] = st
			mu.Unlock()
		}()
	}
	wg.Wait()
	return snap
}

func fetchInstance(ctx context.Context, c *qbt.Client, replicaTag string) *InstanceState {
	st := &InstanceState{Name: c.Name(), URL: c.BaseURL(), FetchedAt: time.Now()}
	fail := func(err error) *InstanceState {
		st.Healthy = false
		st.Error = err.Error()
		return st
	}
	if c.APIVersion().IsZero() {
		if err := c.Login(ctx); err != nil {
			return fail(err)
		}
	}
	ts, err := c.Torrents(ctx)
	if err != nil {
		return fail(err)
	}
	st.Torrents = make(map[string]qbt.Torrent, len(ts))
	st.Private = map[string]bool{}
	for _, t := range ts {
		st.Torrents[t.Hash] = t
		if t.Progress >= 1 && !t.HasTag(replicaTag) {
			p, err := c.IsPrivate(ctx, t)
			if err != nil {
				return fail(err)
			}
			st.Private[t.Hash] = p
		}
	}
	if ti, err := c.TransferInfo(ctx); err == nil {
		st.Transfer = ti
	}
	if port, err := c.ListenPort(ctx); err == nil {
		st.ListenPort = port
	}
	st.Healthy = true
	st.AppVersion = c.AppVersion()
	st.APIVersion = c.APIVersion().String()
	return st
}
