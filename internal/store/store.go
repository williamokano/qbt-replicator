// Package store persists history (events, runs, actions), planner state and
// the notification outbox. It uses database/sql with SQLite (pure Go driver, no
// CGO); the SQL sticks to a portable subset so a Postgres implementation can be
// added behind the same API later.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// MemoryURL keeps everything in memory (nothing survives a restart).
const MemoryURL = "memory"

// Store is the persistence layer. It is safe for concurrent use.
type Store struct {
	db  *sql.DB
	url string
}

// Open opens (and migrates) the database at url:
//
//	sqlite:///config/qbt-replicator.db   absolute path
//	sqlite://data/qbt.db                 relative path
//	memory                               in-memory, for tests or no persistence
func Open(ctx context.Context, url string) (*Store, error) {
	dsn, err := dsnFor(url)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// SQLite allows one writer; a single connection avoids "database is locked"
	// and keeps in-memory databases shared.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, url: url}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func dsnFor(url string) (string, error) {
	switch {
	case url == MemoryURL || url == "":
		return "file::memory:?_pragma=foreign_keys(1)", nil
	case strings.HasPrefix(url, "sqlite://"):
		path := strings.TrimPrefix(url, "sqlite://")
		if path == "" {
			return "", errors.New("database url: sqlite:// needs a file path")
		}
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return "", fmt.Errorf("creating database directory: %w", err)
			}
		}
		return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", nil
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
		return "", errors.New("database url: postgres is not supported yet; use sqlite:// or memory")
	default:
		return "", fmt.Errorf("database url %q: expected sqlite:///path/to/file.db or memory", url)
	}
}

// URL returns the configured database URL.
func (s *Store) URL() string { return s.url }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is usable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

var migrations = []string{
	// 1: initial schema
	`CREATE TABLE events (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		ts       INTEGER NOT NULL,
		level    TEXT NOT NULL,
		kind     TEXT NOT NULL,
		instance TEXT NOT NULL DEFAULT '',
		hash     TEXT NOT NULL DEFAULT '',
		name     TEXT NOT NULL DEFAULT '',
		message  TEXT NOT NULL
	);
	CREATE INDEX events_ts ON events(ts);
	CREATE INDEX events_hash ON events(hash);
	CREATE TABLE runs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		started     INTEGER NOT NULL,
		duration_ms INTEGER NOT NULL,
		trigger     TEXT NOT NULL,
		dry_run     INTEGER NOT NULL,
		actions     INTEGER NOT NULL,
		failed      INTEGER NOT NULL
	);
	CREATE INDEX runs_started ON runs(started);
	CREATE TABLE actions (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id      INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		ts          INTEGER NOT NULL,
		type        TEXT NOT NULL,
		hash        TEXT NOT NULL,
		name        TEXT NOT NULL,
		source      TEXT NOT NULL DEFAULT '',
		target      TEXT NOT NULL,
		rule        TEXT NOT NULL DEFAULT '',
		reason      TEXT NOT NULL DEFAULT '',
		result      TEXT NOT NULL,
		error       TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX actions_ts ON actions(ts);
	CREATE INDEX actions_hash ON actions(hash);
	CREATE TABLE kv (
		key     TEXT PRIMARY KEY,
		value   BLOB NOT NULL,
		updated INTEGER NOT NULL
	);
	CREATE TABLE outbox (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		created      INTEGER NOT NULL,
		notifier     TEXT NOT NULL,
		kind         TEXT NOT NULL,
		payload      TEXT NOT NULL,
		status       TEXT NOT NULL,
		attempts     INTEGER NOT NULL DEFAULT 0,
		next_attempt INTEGER NOT NULL,
		last_error   TEXT NOT NULL DEFAULT '',
		sent_at      INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX outbox_due ON outbox(notifier, status, next_attempt);`,
	// 2: API keys
	`CREATE TABLE api_keys (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		name      TEXT NOT NULL,
		prefix    TEXT NOT NULL,
		hash      TEXT NOT NULL UNIQUE,
		scope     TEXT NOT NULL,
		created   INTEGER NOT NULL,
		last_used INTEGER NOT NULL DEFAULT 0
	);`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("migrations table: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this qbt-replicator (%d); upgrade qbt-replicator", current, len(migrations))
	}
	for v := current + 1; v <= len(migrations); v++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v-1]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", v, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied) VALUES (?, ?)`, v, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: %w", v, err)
		}
	}
	return nil
}

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
