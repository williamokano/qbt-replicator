package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/qbt"
	"github.com/williamokano/qbt-replicator/internal/qbt/fake"
)

// From an empty starter config, saving a real config through the service must
// start replicating without a restart.
func TestSaveConfigReloadsRunningDaemon(t *testing.T) {
	a, b := fake.New(true, "", ""), fake.New(false, "", "")
	defer a.Close()
	defer b.Close()
	a.Seed(qbt.Torrent{Hash: "h1", Name: "iso", SavePath: "/data", Size: 1}, nil, false)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.WriteStarter(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.RunDaemon(ctx, false); close(done) }()

	res, err := svc.SaveConfig(fmt.Sprintf(`
daemon: {interval: 5s}
instances:
  - {name: a, url: %q}
  - {name: b, url: %q}
rules:
  - {name: r, sources: [a], settle_delay: 0s}
`, a.URL, b.URL))
	if err != nil || !res.Saved {
		t.Fatalf("save: %v %+v", err, res)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := b.Get("h1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not replicate after config reload")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b, _ := os.ReadFile(path); len(b) == 0 {
		t.Error("config not written")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}
