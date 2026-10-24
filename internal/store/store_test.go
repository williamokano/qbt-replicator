package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/williamokano/qbt-replicator/internal/events"
)

func openMem(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), MemoryURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenURLs(t *testing.T) {
	ctx := context.Background()
	for _, u := range []string{"postgres://x", "mysql://x", "sqlite://"} {
		if _, err := Open(ctx, u); err == nil {
			t.Errorf("Open(%q) succeeded", u)
		}
	}
}

func TestFileMigrationsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "q.db")
	s, err := Open(ctx, "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s, err = Open(ctx, "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := s.Get(ctx, "k")
	if err != nil || string(v) != "v" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if _, err := s.Get(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("missing key err = %v", err)
	}
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	now := time.Now().Truncate(time.Millisecond)
	for i, h := range []string{"aa", "bb", "aa"} {
		if _, err := s.AddEvent(ctx, events.Event{Time: now.Add(time.Duration(i) * time.Second), Level: events.Info, Kind: "add", Hash: h, Message: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListEvents(ctx, EventFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("ListEvents = %d, %v", len(all), err)
	}
	if all[0].ID != 3 || !all[0].Time.Equal(now.Add(2*time.Second)) || all[0].Level != events.Info {
		t.Fatalf("newest event = %+v", all[0])
	}
	aa, _ := s.ListEvents(ctx, EventFilter{Hash: "aa"})
	if len(aa) != 2 {
		t.Fatalf("hash filter = %d", len(aa))
	}
	page, _ := s.ListEvents(ctx, EventFilter{BeforeID: 3, Limit: 1})
	if len(page) != 1 || page[0].ID != 2 {
		t.Fatalf("page = %+v", page)
	}
}

func TestRunsAndActions(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	start := time.Now().Add(-time.Minute)
	_, err := s.AddRun(ctx, Run{Started: start, Duration: time.Second, Trigger: "daemon", Failed: 1, Actions: []ActionRecord{
		{Type: "add", Hash: "aa", Name: "Ubuntu ISO", Source: "q1", Target: "q2", Result: "ok", Duration: 1500 * time.Millisecond},
		{Type: "add", Hash: "aa", Name: "Ubuntu ISO", Source: "q1", Target: "q3", Result: "error", Error: "boom"},
		{Type: "remove", Hash: "bb", Name: "Debian", Target: "q2", Result: "ok"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.AddRun(ctx, Run{Started: start, Trigger: "sync", DryRun: true, Actions: []ActionRecord{{Type: "add", Hash: "cc", Name: "x", Target: "q2", Result: "dry_run"}}})

	got, err := s.ListActions(ctx, ActionFilter{Search: "ubuntu"})
	if err != nil || len(got) != 2 {
		t.Fatalf("search = %d, %v", len(got), err)
	}
	if got[1].Duration != 1500*time.Millisecond || got[1].RunID != 1 || !got[1].Time.Equal(start.Truncate(time.Millisecond)) {
		t.Fatalf("action = %+v", got[1])
	}
	if got, _ := s.ListActions(ctx, ActionFilter{Instance: "q3"}); len(got) != 1 {
		t.Fatalf("instance filter = %d", len(got))
	}
	if got, _ := s.ListActions(ctx, ActionFilter{Result: "error"}); len(got) != 1 || got[0].Error != "boom" {
		t.Fatalf("result filter = %+v", got)
	}

	st, err := s.ActionStats(ctx, start.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if st.Actions != 3 || st.Failed != 1 || st.ByType["add"] != 1 || st.ByType["remove"] != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestOutbox(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	now := time.Now()
	for _, k := range []string{"a", "b"} {
		if err := s.Enqueue(ctx, "tg", k, `{}`, now); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Enqueue(ctx, "hook", "c", `{}`, now)

	due, err := s.Due(ctx, "tg", now, 10)
	if err != nil || len(due) != 2 || due[0].Kind != "a" {
		t.Fatalf("Due = %+v, %v", due, err)
	}
	if err := s.MarkSent(ctx, []int64{due[0].ID}, now); err != nil {
		t.Fatal(err)
	}
	next := now.Add(time.Minute)
	if err := s.MarkRetry(ctx, []int64{due[1].ID}, "timeout", next, 2); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.Due(ctx, "tg", now, 10); len(due) != 0 {
		t.Fatalf("retry not delayed: %+v", due)
	}
	due, _ = s.Due(ctx, "tg", next, 10)
	if len(due) != 1 || due[0].Attempts != 1 || due[0].LastError != "timeout" {
		t.Fatalf("after retry = %+v", due)
	}
	_ = s.MarkRetry(ctx, []int64{due[0].ID}, "timeout", next, 2)
	counts, _ := s.OutboxCounts(ctx, "tg")
	if counts[OutboxSent] != 1 || counts[OutboxFailed] != 1 || counts[OutboxPending] != 0 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	s := openMem(t)
	old, recent := time.Now().Add(-48*time.Hour), time.Now()
	_, _ = s.AddEvent(ctx, events.Event{Time: old, Level: events.Info, Kind: "k", Message: "old"})
	_, _ = s.AddEvent(ctx, events.Event{Time: recent, Level: events.Info, Kind: "k", Message: "new"})
	_, _ = s.AddRun(ctx, Run{Started: old, Trigger: "daemon", Actions: []ActionRecord{{Type: "add", Hash: "a", Name: "a", Target: "q", Result: "ok"}}})
	_, _ = s.AddRun(ctx, Run{Started: recent, Trigger: "daemon", Actions: []ActionRecord{{Type: "add", Hash: "b", Name: "b", Target: "q", Result: "ok"}}})
	_ = s.Enqueue(ctx, "tg", "old-pending", "{}", old)
	_ = s.Enqueue(ctx, "tg", "old-sent", "{}", old)
	due, _ := s.Due(ctx, "tg", recent, 10)
	_ = s.MarkSent(ctx, []int64{due[1].ID}, old)

	n, err := s.Prune(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // event, action, run, sent notification
		t.Fatalf("pruned %d rows", n)
	}
	ev, _ := s.ListEvents(ctx, EventFilter{})
	ac, _ := s.ListActions(ctx, ActionFilter{})
	if len(ev) != 1 || len(ac) != 1 || ac[0].Hash != "b" {
		t.Fatalf("left %d events, %+v actions", len(ev), ac)
	}
	if due, _ := s.Due(ctx, "tg", recent, 10); len(due) != 1 {
		t.Fatalf("pending notification pruned")
	}
}
