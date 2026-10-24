package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/notify"
)

// Notifications reports each configured notifier and its delivery counts.
func (s *Service) Notifications(ctx context.Context) []notify.Status {
	return s.notify.Statuses(ctx, s.Config())
}

// TestNotification sends a test message with a (saved or unsaved) notifier.
func (s *Service) TestNotification(ctx context.Context, n config.NotificationConfig) error {
	n, err := n.Resolve()
	if err != nil {
		return err
	}
	tmp := config.Config{Notifications: []config.NotificationConfig{n}}
	if errs := tmp.ValidateNotifications(); len(errs) > 0 {
		return errs[0]
	}
	return notify.Test(ctx, n)
}

// summary builds the daily summary: the last 24 hours of actions and the
// current state of every instance.
func (s *Service) summary(ctx context.Context) notify.Notice {
	n := notify.Notice{Level: notify.LevelInfo, Title: "Daily summary"}
	var lines []string
	if st, err := s.HistoryStats(ctx, time.Now().Add(-24*time.Hour)); err == nil {
		line := fmt.Sprintf("Last 24 h: %d replicas added, %d removed", st.ByType["add"]+st.ByType["readd"], st.ByType["remove"])
		if st.Failed > 0 {
			line += fmt.Sprintf(", %d failed actions", st.Failed)
		}
		lines = append(lines, line)
	}
	status := s.Status(ctx)
	for _, in := range status.Instances {
		if !in.Healthy {
			lines = append(lines, fmt.Sprintf("%s: unreachable (%s)", in.Name, in.Error))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %d torrents (%d replicas), ↑ %s", in.Name, in.Torrents, in.Replicas, rate(in.UpSpeed)))
	}
	lines = append(lines, "Total upload: "+rate(status.TotalUp))
	n.Text = strings.Join(lines, "\n")
	return n
}

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
