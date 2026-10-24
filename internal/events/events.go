// Package events keeps a bounded, thread-safe log of replicator activity.
package events

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Level is the event severity.
type Level string

// Levels.
const (
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

// Event is one activity log entry.
type Event struct {
	ID       uint64    `json:"id"`
	Time     time.Time `json:"time"`
	Level    Level     `json:"level"`
	Kind     string    `json:"kind"`
	Instance string    `json:"instance,omitempty"`
	Hash     string    `json:"hash,omitempty"`
	Name     string    `json:"name,omitempty"`
	Message  string    `json:"message"`
}

// Log is a ring buffer of events that also mirrors them to slog.
type Log struct {
	mu     sync.Mutex
	buf    []Event
	next   int
	full   bool
	lastID uint64
	logger *slog.Logger
	sinks  []func(Event)
}

// AddSink registers a function called with every event after it is recorded
// (persistence, notifications). Sinks run synchronously and must be quick.
func (l *Log) AddSink(f func(Event)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sinks = append(l.sinks, f)
}

// New creates a log keeping the last size events.
func New(size int, logger *slog.Logger) *Log {
	if size < 1 {
		size = 1
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Log{buf: make([]Event, size), logger: logger}
}

// Add records an event (ID and Time are filled in).
func (l *Log) Add(e Event) Event {
	l.mu.Lock()
	l.lastID++
	e.ID = l.lastID
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.buf[l.next] = e
	l.next = (l.next + 1) % len(l.buf)
	if l.next == 0 {
		l.full = true
	}
	sinks := l.sinks
	l.mu.Unlock()
	for _, f := range sinks {
		f(e)
	}

	lvl := slog.LevelInfo
	switch e.Level {
	case Warn:
		lvl = slog.LevelWarn
	case Error:
		lvl = slog.LevelError
	}
	attrs := []any{"kind", e.Kind}
	if e.Instance != "" {
		attrs = append(attrs, "instance", e.Instance)
	}
	if e.Hash != "" {
		attrs = append(attrs, "hash", e.Hash)
	}
	if e.Name != "" {
		attrs = append(attrs, "torrent", e.Name)
	}
	l.logger.Log(context.Background(), lvl, e.Message, attrs...)
	return e
}

// Recent returns up to limit events, newest first (limit <= 0 means all).
func (l *Log) Recent(limit int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.next
	if l.full {
		n = len(l.buf)
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Event, 0, limit)
	for i := 1; i <= limit; i++ {
		idx := (l.next - i + len(l.buf)) % len(l.buf)
		out = append(out, l.buf[idx])
	}
	return out
}
