package events

import (
	"io"
	"log/slog"
	"testing"
)

func TestRingBuffer(t *testing.T) {
	l := New(3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(l.Recent(0)) != 0 {
		t.Fatal("expected empty")
	}
	for i := range 5 {
		l.Add(Event{Level: Info, Kind: "k", Message: string(rune('a' + i))})
	}
	got := l.Recent(0)
	if len(got) != 3 || got[0].Message != "e" || got[2].Message != "c" || got[0].ID != 5 {
		t.Fatalf("got %+v", got)
	}
	if got := l.Recent(1); len(got) != 1 || got[0].Message != "e" {
		t.Fatalf("limit: %+v", got)
	}
}
