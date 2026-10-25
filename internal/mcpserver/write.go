package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/replicator"
)

type resultOut struct {
	Type     string `json:"type"`
	Name     string `json:"torrent"`
	Hash     string `json:"hash"`
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	Reason   string `json:"reason,omitempty"`
	Outcome  string `json:"outcome" jsonschema:"done, dry_run (nothing changed) or error"`
	Error    string `json:"error,omitempty"`
	Duration string `json:"duration,omitempty"`
}

type resultsOut struct {
	DryRun  bool        `json:"dry_run" jsonschema:"true: nothing was changed, this is what would happen"`
	Results []resultOut `json:"results"`
	Summary string      `json:"summary"`
}

func toResults(res []replicator.Result, dry bool) resultsOut {
	out := resultsOut{DryRun: dry, Results: []resultOut{}}
	failed := 0
	for _, r := range res {
		o := resultOut{Type: string(r.Action.Type), Name: r.Action.Name, Hash: r.Action.Hash, Source: r.Action.Source,
			Target: r.Action.Target, Reason: r.Action.Reason, Outcome: "done"}
		switch {
		case r.Error != "":
			o.Outcome, o.Error = "error", r.Error
			failed++
		case r.DryRun:
			o.Outcome = "dry_run"
		}
		if r.Duration > 0 {
			o.Duration = r.Duration.String()
		}
		out.Results = append(out.Results, o)
	}
	switch {
	case len(res) == 0:
		out.Summary = "nothing to do"
	case dry:
		out.Summary = fmt.Sprintf("dry run: %d action(s) would be taken; call again with dry_run=false to apply", len(res))
	default:
		out.Summary = fmt.Sprintf("%d action(s) executed, %d failed", len(res), failed)
	}
	return out
}

// dryRun defaults to true: changes must be asked for explicitly.
func dryRun(v *bool) bool { return v == nil || *v }

type syncIn struct {
	DryRun *bool `json:"dry_run,omitempty" jsonschema:"default true: only report what would be done"`
}

type replicateIn struct {
	Torrent string   `json:"torrent" jsonschema:"torrent hash, hash prefix or name"`
	From    string   `json:"from,omitempty" jsonschema:"source instance (default: the instance that owns it)"`
	To      []string `json:"to,omitempty" jsonschema:"target instances (default: the targets of the rules for the source)"`
	Force   bool     `json:"force,omitempty" jsonschema:"ignore rule filters (private flag, settle delay…); the torrent must still be complete"`
	DryRun  *bool    `json:"dry_run,omitempty" jsonschema:"default true: only report what would be done"`
}

type unreplicateIn struct {
	Torrent string   `json:"torrent" jsonschema:"torrent hash, hash prefix or name"`
	On      []string `json:"on,omitempty" jsonschema:"instances to remove the replica from (default: all replicas)"`
	Exclude bool     `json:"exclude,omitempty" jsonschema:"also tag the source so no rule replicates it again (undo with include)"`
	DryRun  *bool    `json:"dry_run,omitempty" jsonschema:"default true: only report what would be done"`
}

type includeIn struct {
	Torrent string `json:"torrent" jsonschema:"torrent hash, hash prefix or name"`
	DryRun  *bool  `json:"dry_run,omitempty" jsonschema:"default true: only report what would be done"`
}

type nameIn struct {
	Name string `json:"name" jsonschema:"the configured name"`
}

type testOut struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func (t *tools) addWrite(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "sync", Title: "Run a replication pass",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)},
		Description: "Run one replication pass now (adds missing replicas, removes orphaned ones; files are never deleted). dry_run defaults to true."},
		handler(t, true, func(ctx context.Context, in syncIn) (resultsOut, error) {
			rep, err := t.b.Sync(ctx, dryRun(in.DryRun))
			if err != nil {
				return resultsOut{}, err
			}
			return toResults(rep.Results, rep.DryRun), nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "replicate", Title: "Replicate a torrent",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
		Description: "Add a completed torrent to other instances as a seed-only replica at the same path. dry_run defaults to true."},
		handler(t, true, func(ctx context.Context, in replicateIn) (resultsOut, error) {
			row, err := t.resolve(ctx, in.Torrent)
			if err != nil {
				return resultsOut{}, err
			}
			dry := dryRun(in.DryRun)
			res, err := t.b.Replicate(ctx, replicator.ReplicateRequest{Hash: row.Hash, From: in.From, To: in.To, Force: in.Force, DryRun: dry})
			if err != nil {
				return resultsOut{}, err
			}
			return toResults(res, dry), nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "unreplicate", Title: "Remove replicas",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)},
		Description: "Remove replicas of a torrent from instances (the files are always kept; source torrents are never touched). Rules may add it again unless exclude is set. dry_run defaults to true."},
		handler(t, true, func(ctx context.Context, in unreplicateIn) (resultsOut, error) {
			row, err := t.resolve(ctx, in.Torrent)
			if err != nil {
				return resultsOut{}, err
			}
			dry := dryRun(in.DryRun)
			res, err := t.b.Unreplicate(ctx, replicator.UnreplicateRequest{Hash: row.Hash, On: in.On, Exclude: in.Exclude, DryRun: dry})
			if err != nil {
				return resultsOut{}, err
			}
			return toResults(res, dry), nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "include", Title: "Include a torrent again",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
		Description: "Remove the exclude tag from a torrent so rules replicate it again (undoes unreplicate with exclude). dry_run defaults to true."},
		handler(t, true, func(ctx context.Context, in includeIn) (resultsOut, error) {
			row, err := t.resolve(ctx, in.Torrent)
			if err != nil {
				return resultsOut{}, err
			}
			dry := dryRun(in.DryRun)
			res, err := t.b.Include(ctx, row.Hash, dry)
			if err != nil {
				return resultsOut{}, err
			}
			return toResults(res, dry), nil
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "test_instance", Title: "Test an instance",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
		Description: "Check that a configured qBittorrent instance is reachable and its credentials work, with an explanation when not."},
		handler(t, true, func(ctx context.Context, in nameIn) (app.InstanceTest, error) {
			return t.b.TestInstance(ctx, in.Name)
		}))

	mcp.AddTool(s, &mcp.Tool{Name: "test_notification", Title: "Send a test notification",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
		Description: "Send a test message with a configured notifier (Telegram or webhook)."},
		handler(t, true, func(ctx context.Context, in nameIn) (testOut, error) {
			if err := t.b.TestNotification(ctx, in.Name); err != nil {
				return testOut{Message: err.Error()}, nil
			}
			return testOut{OK: true, Message: "test message sent"}, nil
		}))
}
