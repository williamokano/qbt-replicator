// Package backend is the set of operations shared by the CLI and the MCP
// server, implemented locally by app.Service and remotely by client.Client.
package backend

import (
	"context"
	"fmt"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/client"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/notify"
	"github.com/williamokano/qbt-replicator/internal/replicator"
	"github.com/williamokano/qbt-replicator/internal/store"
)

// Backend is what the CLI commands and MCP tools need. Locally it is
// app.Service (reading the config file and the database); remotely it is the
// JSON API of a running qbt-replicator.
type Backend interface {
	Status(ctx context.Context, fresh bool) (app.Status, error)
	Plan(ctx context.Context) (*replicator.Plan, error)
	Sync(ctx context.Context, dryRun bool) (*replicator.RunReport, error)
	Torrents(ctx context.Context, f app.TorrentFilter) ([]app.TorrentRow, error)
	Config(ctx context.Context) (config.Config, error) // secrets masked
	TestInstance(ctx context.Context, name string) (app.InstanceTest, error)
	Replicate(ctx context.Context, req replicator.ReplicateRequest) ([]replicator.Result, error)
	Unreplicate(ctx context.Context, req replicator.UnreplicateRequest) ([]replicator.Result, error)
	Include(ctx context.Context, hash string, dryRun bool) ([]replicator.Result, error)
	Events(ctx context.Context, f store.EventFilter) ([]events.Event, error)
	History(ctx context.Context, f store.ActionFilter) ([]store.ActionRecord, error)
	Notifications(ctx context.Context) ([]notify.Status, error)
	TestNotification(ctx context.Context, name string) error
}

var _ Backend = (*client.Client)(nil)

// Local runs operations in this process.
func Local(svc *app.Service) Backend { return local{svc} }

type local struct{ svc *app.Service }

func (l local) Status(ctx context.Context, _ bool) (app.Status, error) { return l.svc.Status(ctx), nil }
func (l local) Plan(ctx context.Context) (*replicator.Plan, error)     { return l.svc.Plan(ctx), nil }
func (l local) Sync(ctx context.Context, dry bool) (*replicator.RunReport, error) {
	return l.svc.SyncOnce(ctx, dry), nil
}

func (l local) Torrents(ctx context.Context, f app.TorrentFilter) ([]app.TorrentRow, error) {
	return l.svc.Torrents(ctx, f), nil
}
func (l local) Config(context.Context) (config.Config, error) { return l.svc.ConfigRedacted(), nil }

func (l local) TestInstance(ctx context.Context, name string) (app.InstanceTest, error) {
	in, ok := l.svc.Config().Instance(name)
	if !ok {
		return app.InstanceTest{}, fmt.Errorf("unknown instance %q", name)
	}
	return l.svc.TestInstance(ctx, in), nil
}

func (l local) Replicate(ctx context.Context, req replicator.ReplicateRequest) ([]replicator.Result, error) {
	return l.svc.Replicate(ctx, req)
}

func (l local) Unreplicate(ctx context.Context, req replicator.UnreplicateRequest) ([]replicator.Result, error) {
	return l.svc.Unreplicate(ctx, req)
}

func (l local) Include(ctx context.Context, hash string, dry bool) ([]replicator.Result, error) {
	return l.svc.Include(ctx, hash, dry)
}

func (l local) Events(ctx context.Context, f store.EventFilter) ([]events.Event, error) {
	return l.svc.EventHistory(ctx, f)
}

func (l local) History(ctx context.Context, f store.ActionFilter) ([]store.ActionRecord, error) {
	return l.svc.History(ctx, f)
}

func (l local) Notifications(ctx context.Context) ([]notify.Status, error) {
	return l.svc.Notifications(ctx), nil
}

func (l local) TestNotification(ctx context.Context, name string) error {
	for _, n := range l.svc.Config().Notifications {
		if n.Name == name {
			return l.svc.TestNotification(ctx, n)
		}
	}
	return fmt.Errorf("unknown notification %q", name)
}
