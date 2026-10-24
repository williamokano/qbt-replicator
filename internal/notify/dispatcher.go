package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/store"
)

const (
	// maxAttempts before a notification is given up (about 4 hours of retries).
	maxAttempts = 10
	// dedupeWindow suppresses repeats of the same failure.
	dedupeWindow = 6 * time.Hour
	// pollInterval is how often the outbox is checked without a wake-up.
	pollInterval = 5 * time.Second
)

// retryDelay is the wait after the given number of failed attempts: 30s doubling up to 1h.
func retryDelay(attempts int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempts && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

type target struct {
	cfg    config.NotificationConfig
	sender Sender
}

// Dispatcher enqueues notices into the outbox and delivers them.
type Dispatcher struct {
	store *store.Store
	log   *slog.Logger
	wake  chan struct{}
	// Now is the clock (tests replace it).
	Now func() time.Time
	// Summary builds the daily summary notice.
	Summary func(ctx context.Context) Notice

	mu        sync.Mutex
	targets   []target
	publicURL string
	seen      map[string]time.Time // dedupe: failure key -> last notified
}

// NewDispatcher creates a dispatcher over the store's outbox.
func NewDispatcher(st *store.Store, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{store: st, log: logger, wake: make(chan struct{}, 1), Now: time.Now, seen: map[string]time.Time{}}
}

// Configure applies the notifications of cfg (at start and on every reload).
// Pending notifications of notifiers that were removed are dropped.
func (d *Dispatcher) Configure(ctx context.Context, cfg *config.Config) error {
	var targets []target
	var names []string
	for _, n := range cfg.Notifications {
		names = append(names, n.Name)
		if !n.Enabled {
			continue
		}
		s, err := NewSender(n)
		if err != nil {
			return err
		}
		targets = append(targets, target{cfg: n, sender: s})
	}
	d.mu.Lock()
	d.targets = targets
	d.publicURL = strings.TrimRight(cfg.Server.PublicURL, "/")
	d.mu.Unlock()
	return d.store.DropPending(ctx, names)
}

// Notify queues a notice for every enabled notifier subscribed to its kind.
// Repeated failures (same action, instance and torrent) are sent at most once
// per dedupe window.
func (d *Dispatcher) Notify(n Notice) {
	now := d.Now()
	if n.Time.IsZero() {
		n.Time = now
	}
	d.mu.Lock()
	switch n.Kind {
	case config.EventReplicaAdded, config.EventReplicaRemoved:
		// Success: a later failure of the same action is news again.
		for _, inst := range strings.Split(n.Instance, ",") {
			f := n
			f.Kind, f.Instance = config.EventActionFailed, inst
			delete(d.seen, f.dedupeKey())
		}
	case config.EventActionFailed:
		key := n.dedupeKey()
		if last, ok := d.seen[key]; ok && now.Sub(last) < dedupeWindow {
			d.mu.Unlock()
			return
		}
		d.seen[key] = now
		for k, t := range d.seen {
			if now.Sub(t) >= dedupeWindow {
				delete(d.seen, k)
			}
		}
	}
	if n.URL == "" && n.Hash != "" && d.publicURL != "" {
		n.URL = d.publicURL + "/history?hash=" + n.Hash
	}
	targets := d.targets
	d.mu.Unlock()

	queued := false
	for _, t := range targets {
		if t.cfg.Wants(n.Kind) {
			d.enqueue(t.cfg.Name, n)
			queued = true
		}
	}
	if queued {
		d.Wake()
	}
}

func (d *Dispatcher) enqueue(notifier string, n Notice) {
	b, err := json.Marshal(n)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.store.Enqueue(ctx, notifier, n.Kind, string(b), n.Time); err != nil {
		d.log.Warn("queueing notification failed", "notifier", notifier, "err", err)
	}
}

// Wake makes Run deliver now instead of at the next poll.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run delivers queued notifications and daily summaries until ctx is done.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		d.Flush(ctx)
		d.dailySummaries(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

// Flush delivers due notifications of every notifier, one batch each.
func (d *Dispatcher) Flush(ctx context.Context) { d.flush(ctx, false) }

// FlushNow delivers due notifications without waiting for the digest window
// (on shutdown and after one-shot CLI commands).
func (d *Dispatcher) FlushNow(ctx context.Context) { d.flush(ctx, true) }

func (d *Dispatcher) flush(ctx context.Context, now bool) {
	d.mu.Lock()
	targets := d.targets
	d.mu.Unlock()
	for _, t := range targets {
		d.flushOne(ctx, t, now)
	}
}

func (d *Dispatcher) flushOne(ctx context.Context, t target, force bool) {
	now := d.Now()
	items, err := d.store.Due(ctx, t.cfg.Name, now, 50)
	if err != nil || len(items) == 0 {
		return
	}
	// Digest: wait until the oldest new item is digest old, so a burst becomes one message.
	if !force && t.cfg.Digest > 0 && items[0].Attempts == 0 && now.Sub(items[0].Created) < t.cfg.Digest {
		return
	}
	batch := make([]Notice, 0, len(items))
	ids := make([]int64, 0, len(items))
	attempts := 0
	for _, it := range items {
		var n Notice
		if json.Unmarshal([]byte(it.Payload), &n) == nil {
			batch = append(batch, n)
		}
		ids = append(ids, it.ID)
		attempts = max(attempts, it.Attempts)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = t.sender.Send(sendCtx, batch)
	if err == nil {
		if err := d.store.MarkSent(ctx, ids, d.Now()); err != nil {
			d.log.Warn("marking notifications sent failed", "notifier", t.cfg.Name, "err", err)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	delay := retryDelay(attempts + 1)
	var ra *RetryAfterError
	if errors.As(err, &ra) {
		delay = max(delay, ra.After)
	}
	d.log.Warn("sending notification failed", "notifier", t.cfg.Name, "attempt", attempts+1, "retry_in", delay, "err", err)
	if err := d.store.MarkRetry(ctx, ids, err.Error(), now.Add(delay), maxAttempts); err != nil {
		d.log.Warn("recording notification failure failed", "notifier", t.cfg.Name, "err", err)
	}
}

// dailySummaries queues each notifier's summary once a day, within an hour
// after its configured time (a summary missed while stopped is skipped).
func (d *Dispatcher) dailySummaries(ctx context.Context) {
	if d.Summary == nil {
		return
	}
	d.mu.Lock()
	targets := d.targets
	d.mu.Unlock()
	now := d.Now()
	var summary *Notice
	for _, t := range targets {
		at, err := time.ParseInLocation("15:04", t.cfg.DailySummary, now.Location())
		if t.cfg.DailySummary == "" || err != nil {
			continue
		}
		due := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
		if now.Before(due) || now.Sub(due) >= time.Hour {
			continue
		}
		key := "summary_sent:" + t.cfg.Name
		day := now.Format(time.DateOnly)
		if last, err := d.store.Get(ctx, key); err == nil && string(last) == day {
			continue
		}
		if summary == nil {
			s := d.Summary(ctx)
			s.Kind, s.Time = KindDailySummary, now
			summary = &s
		}
		d.enqueue(t.cfg.Name, *summary)
		if err := d.store.Put(ctx, key, []byte(day)); err != nil {
			d.log.Warn("saving summary state failed", "err", err)
		}
		d.Wake()
	}
}

// Test sends a test message directly (no outbox), for the Test button.
func Test(ctx context.Context, c config.NotificationConfig) error {
	s, err := NewSender(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return s.Send(ctx, []Notice{{
		Kind: KindTest, Level: LevelInfo, Time: time.Now(),
		Title: "Test notification from qbt-replicator",
		Text:  fmt.Sprintf("If you can read this, the %s notifier %q works.", c.Type, c.Name),
	}})
}

// Status is a notifier's delivery state.
type Status struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Enabled bool           `json:"enabled"`
	Counts  map[string]int `json:"counts"` // pending, sent, failed
}

// Statuses reports outbox counts per configured notifier.
func (d *Dispatcher) Statuses(ctx context.Context, cfg *config.Config) []Status {
	out := make([]Status, 0, len(cfg.Notifications))
	for _, n := range cfg.Notifications {
		c, err := d.store.OutboxCounts(ctx, n.Name)
		if err != nil {
			c = map[string]int{}
		}
		out = append(out, Status{Name: n.Name, Type: n.Type, Enabled: n.Enabled, Counts: c})
	}
	return out
}
