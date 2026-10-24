package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/qbt"
	"github.com/williamokano/qbt-replicator/internal/qbt/fake"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	cfg, err := config.Parse([]byte("instances: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestTestInstance(t *testing.T) {
	srv := fake.New(true, "admin", "pw")
	defer srv.Close()
	srv.Seed(qbt.Torrent{Hash: "h", Name: "n", SavePath: "/d"}, nil, false)
	svc := newTestService(t)
	ctx := context.Background()

	ok := svc.TestInstance(ctx, config.InstanceConfig{Name: "a", URL: srv.URL, Username: "admin", Password: "pw"})
	if !ok.OK || ok.Torrents != 1 || ok.AppVersion == "" || !strings.Contains(ok.Message, "Connected") {
		t.Fatalf("good credentials: %+v", ok)
	}

	t.Setenv("TEST_PW", "pw")
	env := svc.TestInstance(ctx, config.InstanceConfig{URL: srv.URL, Username: "admin", Password: "${TEST_PW}"})
	if !env.OK {
		t.Fatalf("${VAR} password not expanded: %+v", env)
	}

	bad := svc.TestInstance(ctx, config.InstanceConfig{URL: srv.URL, Username: "admin", Password: "nope"})
	if bad.OK || !strings.Contains(bad.Message, "Wrong username or password") {
		t.Fatalf("bad credentials: %+v", bad)
	}

	srv.Close() // nothing listening any more
	down := svc.TestInstance(ctx, config.InstanceConfig{URL: srv.URL, Timeout: 2 * time.Second})
	if down.OK || !strings.Contains(down.Message, "refused") {
		t.Fatalf("unreachable: %+v", down)
	}

	if r := svc.TestInstance(ctx, config.InstanceConfig{URL: "gluetun-1:8080"}); r.OK || !strings.Contains(r.Message, "WebUI URL") {
		t.Fatalf("invalid url: %+v", r)
	}
}

func TestTestInstanceAPIKey(t *testing.T) {
	srv := fake.New(true, "admin", "pw")
	defer srv.Close()
	srv.APIKey = "qbt_good"
	svc := newTestService(t)
	ctx := context.Background()
	t.Setenv("TEST_KEY", "qbt_good")
	if r := svc.TestInstance(ctx, config.InstanceConfig{URL: srv.URL, APIKey: "${TEST_KEY}"}); !r.OK {
		t.Fatalf("api key: %+v", r)
	}
	r := svc.TestInstance(ctx, config.InstanceConfig{URL: srv.URL, APIKey: "qbt_bad"})
	if r.OK || !strings.Contains(r.Message, "API key rejected") {
		t.Fatalf("bad api key: %+v", r)
	}
}
