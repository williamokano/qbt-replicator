package app

import (
	"context"
	"fmt"
	"time"

	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/store"
)

// History returns executed actions, newest first.
func (s *Service) History(ctx context.Context, f store.ActionFilter) ([]store.ActionRecord, error) {
	return s.store.ListActions(ctx, f)
}

// EventHistory returns persisted activity log entries, newest first.
func (s *Service) EventHistory(ctx context.Context, f store.EventFilter) ([]events.Event, error) {
	return s.store.ListEvents(ctx, f)
}

// HistoryStats counts executed actions since t.
func (s *Service) HistoryStats(ctx context.Context, since time.Time) (store.Stats, error) {
	return s.store.ActionStats(ctx, since)
}

// Prune deletes history older than database.retention (0 keeps everything).
func (s *Service) Prune(ctx context.Context) (int64, error) {
	keep := s.Config().Database.RetentionDur
	if keep <= 0 {
		return 0, nil
	}
	return s.store.Prune(ctx, time.Now().Add(-keep))
}

// pruneLoop applies the retention at start and then hourly.
func (s *Service) pruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := s.Prune(ctx); err != nil {
			s.events.Add(events.Event{Level: events.Warn, Kind: "history", Message: "pruning history: " + err.Error()})
		} else if n > 0 {
			s.logger.Info(fmt.Sprintf("pruned %d history rows older than %s", n, s.Config().Database.Retention))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
