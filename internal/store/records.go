package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/events"
)

// --- events (activity log) ---

// AddEvent persists an activity log entry and returns its id.
func (s *Store) AddEvent(ctx context.Context, e events.Event) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (ts, level, kind, instance, hash, name, message) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		unixMilli(e.Time), string(e.Level), e.Kind, e.Instance, e.Hash, e.Name, e.Message)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EventFilter narrows ListEvents. Zero values mean "any".
type EventFilter struct {
	Hash     string
	Instance string
	Level    string
	BeforeID int64 // pagination: only ids below this
	Limit    int
}

// ListEvents returns events newest first.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]events.Event, error) {
	var where []string
	var args []any
	if f.Hash != "" {
		where, args = append(where, "hash = ?"), append(args, f.Hash)
	}
	if f.Instance != "" {
		where, args = append(where, "instance = ?"), append(args, f.Instance)
	}
	if f.Level != "" {
		where, args = append(where, "level = ?"), append(args, f.Level)
	}
	if f.BeforeID > 0 {
		where, args = append(where, "id < ?"), append(args, f.BeforeID)
	}
	q := `SELECT id, ts, level, kind, instance, hash, name, message FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // constant clauses; values are bound parameters
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limitOr(f.Limit, 100))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []events.Event
	for rows.Next() {
		var e events.Event
		var id, ts int64
		var level string
		if err := rows.Scan(&id, &ts, &level, &e.Kind, &e.Instance, &e.Hash, &e.Name, &e.Message); err != nil {
			return nil, err
		}
		e.ID, e.Time, e.Level = uint64(id), fromMilli(ts), events.Level(level) //nolint:gosec // AUTOINCREMENT ids are positive
		out = append(out, e)
	}
	return out, rows.Err()
}

func limitOr(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, 1000)
}

// --- runs and actions (execution history) ---

// Run is one reconcile pass or manual operation with its actions.
type Run struct {
	ID       int64          `json:"id"`
	Started  time.Time      `json:"started"`
	Duration time.Duration  `json:"duration"`
	Trigger  string         `json:"trigger"` // daemon, sync, manual
	DryRun   bool           `json:"dry_run"`
	Failed   int            `json:"failed"`
	Actions  []ActionRecord `json:"actions,omitempty"`
}

// ActionRecord is one executed (or dry-run) action.
type ActionRecord struct {
	ID       int64         `json:"id"`
	RunID    int64         `json:"run_id"`
	Time     time.Time     `json:"time"`
	Type     string        `json:"type"`
	Hash     string        `json:"hash"`
	Name     string        `json:"name"`
	Source   string        `json:"source,omitempty"`
	Target   string        `json:"target"`
	Rule     string        `json:"rule,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	Result   string        `json:"result"` // ok, error, dry_run
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration"`
}

// AddRun stores a run and its actions atomically and returns the run id.
func (s *Store) AddRun(ctx context.Context, r Run) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO runs (started, duration_ms, trigger, dry_run, actions, failed) VALUES (?, ?, ?, ?, ?, ?)`,
		unixMilli(r.Started), r.Duration.Milliseconds(), r.Trigger, boolInt(r.DryRun), len(r.Actions), r.Failed)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, a := range r.Actions {
		ts := a.Time
		if ts.IsZero() {
			ts = r.Started
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO actions (run_id, ts, type, hash, name, source, target, rule, reason, result, error, duration_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, unixMilli(ts), a.Type, a.Hash, a.Name, a.Source, a.Target, a.Rule, a.Reason, a.Result, a.Error, a.Duration.Milliseconds()); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// ActionFilter narrows ListActions. Zero values mean "any".
type ActionFilter struct {
	Hash     string
	Search   string // substring of the torrent name (case-insensitive)
	Instance string // source or target
	Type     string
	Result   string
	Since    time.Time
	BeforeID int64
	Limit    int
}

// ListActions returns actions newest first.
func (s *Store) ListActions(ctx context.Context, f ActionFilter) ([]ActionRecord, error) {
	var where []string
	var args []any
	if f.Hash != "" {
		where, args = append(where, "hash = ?"), append(args, f.Hash)
	}
	if f.Search != "" {
		where, args = append(where, "(LOWER(name) LIKE ? OR hash LIKE ?)"), append(args, "%"+strings.ToLower(f.Search)+"%", strings.ToLower(f.Search)+"%")
	}
	if f.Instance != "" {
		where, args = append(where, "(source = ? OR target = ?)"), append(args, f.Instance, f.Instance)
	}
	if f.Type != "" {
		where, args = append(where, "type = ?"), append(args, f.Type)
	}
	if f.Result != "" {
		where, args = append(where, "result = ?"), append(args, f.Result)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "ts >= ?"), append(args, unixMilli(f.Since))
	}
	if f.BeforeID > 0 {
		where, args = append(where, "id < ?"), append(args, f.BeforeID)
	}
	q := `SELECT id, run_id, ts, type, hash, name, source, target, rule, reason, result, error, duration_ms FROM actions`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // constant clauses; values are bound parameters
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limitOr(f.Limit, 100))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionRecord
	for rows.Next() {
		var a ActionRecord
		var ts, dur int64
		if err := rows.Scan(&a.ID, &a.RunID, &ts, &a.Type, &a.Hash, &a.Name, &a.Source, &a.Target, &a.Rule, &a.Reason, &a.Result, &a.Error, &dur); err != nil {
			return nil, err
		}
		a.Time, a.Duration = fromMilli(ts), time.Duration(dur)*time.Millisecond
		out = append(out, a)
	}
	return out, rows.Err()
}

// Stats summarizes actions since a point in time (for summaries).
type Stats struct {
	Since   time.Time      `json:"since"`
	ByType  map[string]int `json:"by_type"`
	Failed  int            `json:"failed"`
	Actions int            `json:"actions"`
}

// ActionStats counts non-dry-run actions since t.
func (s *Store) ActionStats(ctx context.Context, since time.Time) (Stats, error) {
	st := Stats{Since: since, ByType: map[string]int{}}
	rows, err := s.db.QueryContext(ctx,
		`SELECT type, result, COUNT(*) FROM actions WHERE ts >= ? AND result != 'dry_run' GROUP BY type, result`, unixMilli(since))
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var typ, result string
		var n int
		if err := rows.Scan(&typ, &result, &n); err != nil {
			return st, err
		}
		st.Actions += n
		if result == "error" {
			st.Failed += n
			continue
		}
		st.ByType[typ] += n
	}
	return st, rows.Err()
}

// --- key/value (planner state, notification bookkeeping) ---

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("store: not found")

// Get reads a value.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

// Put writes a value.
func (s *Store) Put(ctx context.Context, key string, value []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated = excluded.updated`,
		key, value, time.Now().UnixMilli())
	return err
}

// --- notification outbox ---

// Outbox statuses.
const (
	OutboxPending = "pending"
	OutboxSent    = "sent"
	OutboxFailed  = "failed"
)

// OutboxItem is a queued notification for one notifier.
type OutboxItem struct {
	ID          int64     `json:"id"`
	Created     time.Time `json:"created"`
	Notifier    string    `json:"notifier"`
	Kind        string    `json:"kind"`
	Payload     string    `json:"payload"` // JSON
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error,omitempty"`
}

// Enqueue adds a pending notification.
func (s *Store) Enqueue(ctx context.Context, notifier, kind, payload string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO outbox (created, notifier, kind, payload, status, next_attempt) VALUES (?, ?, ?, ?, ?, ?)`,
		unixMilli(at), notifier, kind, payload, OutboxPending, unixMilli(at))
	return err
}

// Due returns pending items for a notifier whose next attempt is at or before
// now, in creation order.
func (s *Store) Due(ctx context.Context, notifier string, now time.Time, limit int) ([]OutboxItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created, notifier, kind, payload, status, attempts, next_attempt, last_error
		 FROM outbox WHERE notifier = ? AND status = ? AND next_attempt <= ? ORDER BY id LIMIT ?`,
		notifier, OutboxPending, unixMilli(now), limitOr(limit, 100))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		var created, next int64
		if err := rows.Scan(&it.ID, &created, &it.Notifier, &it.Kind, &it.Payload, &it.Status, &it.Attempts, &next, &it.LastError); err != nil {
			return nil, err
		}
		it.Created, it.NextAttempt = fromMilli(created), fromMilli(next)
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkSent marks items as delivered.
func (s *Store) MarkSent(ctx context.Context, ids []int64, at time.Time) error {
	return s.updateOutbox(ctx, ids, `UPDATE outbox SET status = 'sent', sent_at = ?, last_error = '' WHERE id = ?`, unixMilli(at))
}

// MarkRetry records a failed attempt; items move to "failed" after maxAttempts.
func (s *Store) MarkRetry(ctx context.Context, ids []int64, errMsg string, next time.Time, maxAttempts int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE outbox SET attempts = attempts + 1, last_error = ?, next_attempt = ?,
			   status = CASE WHEN attempts + 1 >= ? THEN 'failed' ELSE status END
			 WHERE id = ?`, errMsg, unixMilli(next), maxAttempts, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) updateOutbox(ctx context.Context, ids []int64, q string, arg any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, q, arg, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OutboxCounts returns item counts per status for a notifier.
func (s *Store) OutboxCounts(ctx context.Context, notifier string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM outbox WHERE notifier = ? GROUP BY status`, notifier)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// --- retention ---

// Prune deletes history older than before (events, runs with their actions,
// and delivered or failed notifications). Pending notifications are kept.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	ts := unixMilli(before)
	var total int64
	for _, q := range []string{
		`DELETE FROM events WHERE ts < ?`,
		`DELETE FROM actions WHERE run_id IN (SELECT id FROM runs WHERE started < ?)`,
		`DELETE FROM runs WHERE started < ?`,
		`DELETE FROM outbox WHERE status != 'pending' AND created < ?`,
	} {
		res, err := s.db.ExecContext(ctx, q, ts)
		if err != nil {
			return total, fmt.Errorf("pruning: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
