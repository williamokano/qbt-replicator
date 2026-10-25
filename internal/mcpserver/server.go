// Package mcpserver exposes qbt-replicator to AI assistants as an MCP server.
// Tools are thin wrappers over backend.Backend, so the same server runs over
// HTTP inside the daemon (/mcp) or over stdio against a local or remote backend.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/backend"
	"github.com/williamokano/qbt-replicator/internal/version"
)

// Options configures a server.
type Options struct {
	// ReadOnly hides the tools that change anything (read-only API keys).
	ReadOnly bool
	// Limit is called before every tool call (rate limiting); write is true
	// for tools that change something. nil means unlimited.
	Limit func(ctx context.Context, write bool) error
}

const instructions = `qbt-replicator replicates completed torrents from a source qBittorrent instance
to other instances that share the same storage, so one torrent seeds from several VPN tunnels.
Replicas carry a replica tag; removing a replica never deletes files.
Start with get_status. To explain one torrent ("why isn't X on qbt-3?") use get_torrent, which
includes the planner's reason for skipping it. Tools that change something default to
dry_run=true: show the user the dry-run result and only repeat with dry_run=false when they agree.`

// New builds the MCP server.
func New(b backend.Backend, opts Options) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "qbt-replicator", Title: "qbt-replicator", Version: version.Version},
		&mcp.ServerOptions{Instructions: instructions})
	t := &tools{b: b, opts: opts}
	t.addRead(s)
	if !opts.ReadOnly {
		t.addWrite(s)
	}
	return s
}

type tools struct {
	b    backend.Backend
	opts Options
}

func (t *tools) limit(ctx context.Context, write bool) error {
	if t.opts.Limit == nil {
		return nil
	}
	return t.opts.Limit(ctx, write)
}

func ptr[T any](v T) *T { return &v }

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}

// handler wraps a tool with rate limiting.
func handler[In, Out any](t *tools, write bool, f func(context.Context, In) (Out, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		if err := t.limit(ctx, write); err != nil {
			return nil, zero, err
		}
		out, err := f(ctx, in)
		return nil, out, err
	}
}

var fullHash = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// resolve turns a hash, hash prefix or (part of a) name into one torrent.
func (t *tools) resolve(ctx context.Context, query string) (app.TorrentRow, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return app.TorrentRow{}, errors.New("give a torrent name, hash or hash prefix")
	}
	rows, err := t.b.Torrents(ctx, app.TorrentFilter{Search: query})
	if err != nil {
		return app.TorrentRow{}, err
	}
	lq := strings.ToLower(query)
	for _, r := range rows {
		if r.Hash == lq || strings.EqualFold(r.Name, query) {
			return r, nil
		}
	}
	var prefixed []app.TorrentRow
	for _, r := range rows {
		if strings.HasPrefix(r.Hash, lq) {
			prefixed = append(prefixed, r)
		}
	}
	if len(prefixed) == 1 {
		return prefixed[0], nil
	}
	switch {
	case len(rows) == 1:
		return rows[0], nil
	case len(rows) == 0 && fullHash.MatchString(query):
		return app.TorrentRow{}, fmt.Errorf("no torrent with hash %s on any reachable instance", lq)
	case len(rows) == 0:
		return app.TorrentRow{}, fmt.Errorf("no torrent matches %q; try search_torrents with a shorter query", query)
	}
	var names []string
	for i, r := range rows {
		if i == 5 {
			names = append(names, fmt.Sprintf("… and %d more", len(rows)-5))
			break
		}
		names = append(names, fmt.Sprintf("%s (%s)", r.Name, r.Hash[:min(8, len(r.Hash))]))
	}
	return app.TorrentRow{}, fmt.Errorf("%q matches %d torrents, pass the hash of one: %s", query, len(rows), strings.Join(names, "; "))
}
