package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/client"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/mcpserver"
	"github.com/williamokano/qbt-replicator/internal/replicator"
	"github.com/williamokano/qbt-replicator/internal/store"
	"github.com/williamokano/qbt-replicator/internal/version"
	"github.com/williamokano/qbt-replicator/internal/web"
)

func newServeCmd(g *globals) *cobra.Command {
	var noDaemon bool
	var listen string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the replication daemon and the web UI/API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.localOnly("serve"); err != nil {
				return err
			}
			path := config.ResolvePath(g.configPath)
			created := false
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				if err := config.WriteStarter(path); err != nil {
					return fmt.Errorf("no config at %s and could not create one: %w", path, err)
				}
				created = true
			}
			g.configPath = path
			svc, err := g.service()
			if err != nil {
				return err
			}
			cfg := g.cfg
			if created {
				g.logger.Warn("no configuration found; created a starter config, finish it in the web UI (Config page)", "path", path)
			}
			if listen != "" {
				cfg.Server.Listen = listen
			}
			ctx, stop := signal.NotifyContext(ctxOf(cmd), os.Interrupt, syscall.SIGTERM)
			defer stop()

			srv := &http.Server{
				Addr:              cfg.Server.Listen,
				Handler:           web.New(svc, g.logger),
				ReadHeaderTimeout: 10 * time.Second,
			}
			ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Server.Listen)
			if err != nil {
				return fmt.Errorf("listening on %s: %w", cfg.Server.Listen, err)
			}
			g.logger.Info("web server listening", "addr", ln.Addr().String(), "base_path", cfg.Server.BasePath,
				"auth", cfg.Server.Auth.Enabled(), "config", cfg.Path, "version", version.Version)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				svc.RunDaemon(ctx, noDaemon)
			}()
			if !cfg.Daemon.Enabled || noDaemon {
				g.logger.Info("daemon disabled; UI/API only")
			}
			if !cfg.Configured() {
				g.logger.Warn("no instances configured yet (setup mode); add them on the Config page")
			}
			errCh := make(chan error, 1)
			go func() { errCh <- srv.Serve(ln) }()

			select {
			case <-ctx.Done():
			case err := <-errCh:
				if !errors.Is(err, http.ErrServerClosed) {
					stop()
					wg.Wait()
					return err
				}
			}
			g.logger.Info("shutting down")
			shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutCtx)
			wg.Wait()
			return nil
		},
	}
	cmd.Flags().BoolVar(&noDaemon, "no-daemon", false, "serve the UI/API only, do not run the reconcile loop")
	cmd.Flags().StringVar(&listen, "listen", "", "override server.listen (e.g. :8080)")
	return cmd
}

func newSyncCmd(g *globals) *cobra.Command {
	var dry bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Run a single reconcile pass and exit (cron friendly)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			rep, err := b.Sync(ctxOf(cmd), dry)
			if err != nil {
				return err
			}
			if err := printReport(cmd.OutOrStdout(), g.output, rep); err != nil {
				return err
			}
			if rep.Failed > 0 {
				return fmt.Errorf("%d action(s) failed", rep.Failed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dry, "dry-run", false, "plan and log actions without executing them")
	return cmd
}

func newPlanCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Show the actions the next pass would take",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			p, err := b.Plan(ctxOf(cmd))
			if err != nil {
				return err
			}
			return printPlan(cmd.OutOrStdout(), g.output, p)
		},
	}
}

func newStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show instance health, connection status and speeds",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			st, err := b.Status(ctxOf(cmd), true)
			if err != nil {
				return err
			}
			return printStatus(cmd.OutOrStdout(), g.output, st)
		},
	}
}

func newInstancesCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "instances", Short: "Inspect configured instances"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured instances",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			red, err := b.Config(ctxOf(cmd))
			if err != nil {
				return err
			}
			if g.output == "json" {
				return printJSON(cmd.OutOrStdout(), red.Instances)
			}
			tw := newTable(cmd.OutOrStdout())
			fmt.Fprintln(tw, "NAME\tURL\tUSER\tPATH MAPPINGS")
			for _, in := range red.Instances {
				var maps []string
				for _, m := range in.PathMappings {
					maps = append(maps, m.Instance+"=>"+m.Canonical)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", in.Name, in.URL, in.Username, strings.Join(maps, ","))
			}
			return tw.Flush()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "test <name>",
		Short: "Test one instance's URL and credentials, with an explanation if it fails",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			name := args[0]
			res, err := b.TestInstance(ctxOf(cmd), name)
			if err != nil {
				return err
			}
			if g.output == "json" {
				if err := printJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else {
				mark := "OK"
				if !res.OK {
					mark = "FAILED"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s\n", name, mark, res.Message)
			}
			if !res.OK {
				return fmt.Errorf("instance %s: test failed", name)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Check connectivity, credentials and versions of every instance",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			st, err := b.Status(ctxOf(cmd), true)
			if err != nil {
				return err
			}
			if err := printStatus(cmd.OutOrStdout(), g.output, st); err != nil {
				return err
			}
			bad := 0
			for _, in := range st.Instances {
				if !in.Healthy {
					bad++
				}
			}
			if bad > 0 {
				return fmt.Errorf("%d instance(s) unhealthy", bad)
			}
			return nil
		},
	})
	return cmd
}

func newTorrentsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "torrents", Short: "Inspect torrents across instances"}
	var f app.TorrentFilter
	list := &cobra.Command{
		Use:   "list",
		Short: "List torrents as a torrent x instance matrix",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			f.Fresh = true
			rows, err := b.Torrents(ctxOf(cmd), f)
			if err != nil {
				return err
			}
			cfg, err := b.Config(ctxOf(cmd))
			if err != nil {
				return err
			}
			return printTorrents(cmd.OutOrStdout(), g.output, cfg.InstanceNames(), rows)
		},
	}
	list.Flags().StringVar(&f.Instance, "instance", "", "only torrents present on this instance")
	list.Flags().StringVar(&f.Search, "search", "", "name substring or hash prefix")
	list.Flags().StringVar(&f.Category, "category", "", "only this category")
	list.Flags().BoolVar(&f.Completed, "completed", false, "only completed torrents")
	cmd.AddCommand(list)
	return cmd
}

func newReplicateCmd(g *globals) *cobra.Command {
	var req replicator.ReplicateRequest
	cmd := &cobra.Command{
		Use:   "replicate <hash>",
		Short: "Manually replicate a completed torrent to other instances",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			req.Hash = strings.ToLower(args[0])
			res, err := b.Replicate(ctxOf(cmd), req)
			if err != nil {
				return err
			}
			return printResults(cmd.OutOrStdout(), g.output, res)
		},
	}
	cmd.Flags().StringVar(&req.From, "from", "", "source instance (default: first instance owning the torrent)")
	cmd.Flags().StringSliceVar(&req.To, "to", nil, "target instances (default: the targets of the rules for the source)")
	cmd.Flags().BoolVar(&req.Force, "force", false, "ignore rule filters and settle delay")
	cmd.Flags().BoolVar(&req.DryRun, "dry-run", false, "show what would be done")
	return cmd
}

func newUnreplicateCmd(g *globals) *cobra.Command {
	var on []string
	var dry, exclude bool
	cmd := &cobra.Command{
		Use:   "unreplicate <hash>",
		Short: "Remove replicas of a torrent (files are always kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			res, err := b.Unreplicate(ctxOf(cmd), replicator.UnreplicateRequest{
				Hash: strings.ToLower(args[0]), On: on, Exclude: exclude, DryRun: dry,
			})
			if err != nil {
				return err
			}
			return printResults(cmd.OutOrStdout(), g.output, res)
		},
	}
	cmd.Flags().StringSliceVar(&on, "on", nil, "instances to remove the replica from (default: all replicas)")
	cmd.Flags().BoolVar(&exclude, "exclude", false, "also tag the source torrent with daemon.exclude_tag so rules never re-add it")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "show what would be done")
	return cmd
}

func newIncludeCmd(g *globals) *cobra.Command {
	var dry bool
	cmd := &cobra.Command{
		Use:   "include <hash>",
		Short: "Let rules replicate a torrent excluded with 'unreplicate --exclude' again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			res, err := b.Include(ctxOf(cmd), strings.ToLower(args[0]), dry)
			if err != nil {
				return err
			}
			return printResults(cmd.OutOrStdout(), g.output, res)
		},
	}
	cmd.Flags().BoolVar(&dry, "dry-run", false, "show what would be done")
	return cmd
}

func newEventsCmd(g *globals) *cobra.Command {
	var f store.EventFilter
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show the activity log",
		Long:  "Reads the activity log from the database (shared with a running daemon), or from --host.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			evs, err := b.Events(ctxOf(cmd), f)
			if err != nil {
				return err
			}
			return printEvents(cmd.OutOrStdout(), g.output, evs)
		},
	}
	cmd.Flags().IntVar(&f.Limit, "limit", 50, "number of events")
	cmd.Flags().StringVar(&f.Hash, "hash", "", "only events about this torrent")
	cmd.Flags().StringVar(&f.Instance, "instance", "", "only events about this instance")
	cmd.Flags().StringVar(&f.Level, "level", "", "only this level (info|warn|error)")
	return cmd
}

func newHistoryCmd(g *globals) *cobra.Command {
	var f store.ActionFilter
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "history [search]",
		Short: "Show what qbt-replicator did (executed actions)",
		Long: `Lists executed actions from the database, newest first. The optional
argument matches the torrent name (substring) or hash prefix.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				f.Search = args[0]
			}
			if since > 0 {
				f.Since = time.Now().Add(-since)
			}
			acts, err := b.History(ctxOf(cmd), f)
			if err != nil {
				return err
			}
			return printHistory(cmd.OutOrStdout(), g.output, acts)
		},
	}
	cmd.Flags().IntVar(&f.Limit, "limit", 50, "number of actions")
	cmd.Flags().StringVar(&f.Hash, "hash", "", "only this torrent (full hash)")
	cmd.Flags().StringVar(&f.Instance, "instance", "", "only actions with this source or target instance")
	cmd.Flags().StringVar(&f.Type, "type", "", "only this action type (add, readd, remove, recheck, stop, sync_limits, exclude, include)")
	cmd.Flags().StringVar(&f.Result, "result", "", "only this result (ok|error)")
	cmd.Flags().DurationVar(&since, "since", 0, "only actions newer than this (e.g. 24h)")
	return cmd
}

func newNotifyCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "notify", Short: "List notification destinations or send a test message"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List notifiers with their delivery counts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			st, err := b.Notifications(ctxOf(cmd))
			if err != nil {
				return err
			}
			if g.output == "json" {
				return printJSON(cmd.OutOrStdout(), st)
			}
			tw := newTable(cmd.OutOrStdout())
			fmt.Fprintln(tw, "NAME\tTYPE\tENABLED\tPENDING\tSENT\tFAILED")
			for _, n := range st {
				fmt.Fprintf(tw, "%s\t%s\t%v\t%d\t%d\t%d\n", n.Name, n.Type, n.Enabled,
					n.Counts[store.OutboxPending], n.Counts[store.OutboxSent], n.Counts[store.OutboxFailed])
			}
			return tw.Flush()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "test <name>",
		Short: "Send a test message with a configured notifier",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			if err := b.TestNotification(ctxOf(cmd), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "test message sent with %s\n", args[0])
			return nil
		},
	})
	return cmd
}

func newAPIKeyCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apikey",
		Short: "Create, list or revoke API keys (local only)",
		Long: `API keys give scripts, the remote CLI and MCP clients access to the JSON API.
Keys are managed here or in the web UI (Config > API keys), never through the API.`,
	}
	var scope string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an API key and print it (shown only once)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.localOnly("apikey"); err != nil {
				return err
			}
			svc, err := g.service()
			if err != nil {
				return err
			}
			k, err := svc.CreateAPIKey(ctxOf(cmd), args[0], scope)
			if err != nil {
				return err
			}
			if g.output == "json" {
				return printJSON(cmd.OutOrStdout(), k)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n\nAPI key %q (%s) created. Copy it now: it is not shown again.\n", k.Secret, k.Name, k.Scope)
			return nil
		},
	}
	create.Flags().StringVar(&scope, "scope", app.ScopeRead, "read (GET only) or full (also sync, replicate, config)")
	cmd.AddCommand(create, &cobra.Command{
		Use:   "list",
		Short: "List API keys",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.localOnly("apikey"); err != nil {
				return err
			}
			svc, err := g.service()
			if err != nil {
				return err
			}
			keys, err := svc.APIKeys(ctxOf(cmd))
			if err != nil {
				return err
			}
			if g.output == "json" {
				return printJSON(cmd.OutOrStdout(), keys)
			}
			tw := newTable(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tNAME\tKEY\tSCOPE\tCREATED\tLAST USED")
			for _, k := range keys {
				used := "never"
				if !k.LastUsed.IsZero() {
					used = k.LastUsed.Format(time.DateTime)
				}
				fmt.Fprintf(tw, "%d\t%s\t%s…\t%s\t%s\t%s\n", k.ID, k.Name, k.Prefix, k.Scope, k.Created.Format(time.DateTime), used)
			}
			return tw.Flush()
		},
	}, &cobra.Command{
		Use:   "revoke <id|name>",
		Short: "Revoke an API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := g.localOnly("apikey"); err != nil {
				return err
			}
			svc, err := g.service()
			if err != nil {
				return err
			}
			k, err := svc.RevokeAPIKey(ctxOf(cmd), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "API key %q revoked\n", k.Name)
			return nil
		},
	})
	return cmd
}

func newMCPCmd(g *globals) *cobra.Command {
	var readOnly bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server over stdio for AI assistants (Claude Desktop, …)",
		Long: `Speaks the Model Context Protocol on stdin/stdout so an AI assistant can
inspect and drive qbt-replicator. With --host it uses a running qbt-replicator's
API (a read-only key gets only the read tools); without it, the local config and
database. A running server also offers MCP over HTTP at /mcp.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			if c, ok := b.(*client.Client); ok {
				id, err := c.WhoAmI(ctxOf(cmd))
				if err != nil {
					return err
				}
				readOnly = readOnly || id.Scope != app.ScopeFull
			}
			srv := mcpserver.New(b, mcpserver.Options{ReadOnly: readOnly})
			ctx, stop := signal.NotifyContext(ctxOf(cmd), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return srv.Run(ctx, &mcp.StdioTransport{})
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "offer only the tools that read")
	return cmd
}

func newConfigCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Validate or display configuration"}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Validate the configuration file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.localOnly("config validate"); err != nil {
				return err
			}
			if err := g.load(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: OK (%d instances, %d rules)\n", g.cfg.Path, len(g.cfg.Instances), len(g.cfg.Rules))
			for _, r := range g.cfg.Rules {
				if r.Filter.IncludePrivate {
					fmt.Fprintf(cmd.OutOrStdout(), "WARNING: rule %q replicates private torrents; many private trackers forbid seeding one account from several IPs\n", r.Name)
				}
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Write a starter configuration file (never overwrites)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.localOnly("config init"); err != nil {
				return err
			}
			path := config.ResolvePath(g.configPath)
			if err := config.WriteStarter(path); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote starter config to %s\n", path)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration with secrets redacted",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.backend()
			if err != nil {
				return err
			}
			red, err := b.Config(ctxOf(cmd))
			if err != nil {
				return err
			}
			if g.output == "json" {
				return printJSON(cmd.OutOrStdout(), red)
			}
			out, err := yaml.Marshal(red)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	})
	return cmd
}

func newHealthcheckCmd(g *globals) *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe /healthz of a running server (used by Docker HEALTHCHECK)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if target == "" {
				listen, base := ":8080", "/"
				if err := g.load(); err == nil {
					listen, base = g.cfg.Server.Listen, g.cfg.Server.BasePath
				}
				target = localURL(listen, base) + "healthz"
			}
			ctx, cancel := context.WithTimeout(ctxOf(cmd), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s returned %d", target, resp.StatusCode)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "ok")
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "url", "", "URL to probe (default derived from server.listen)")
	return cmd
}

func newVersionCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information (and the server's with --host)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
			if g.remoteHost() == "" {
				return nil
			}
			b, err := g.backend()
			if err != nil {
				return err
			}
			st, err := b.Status(ctxOf(cmd), false)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "server %s: qbt-replicator %s\n", g.remoteHost(), st.Version)
			return nil
		},
	}
}

// localURL turns a listen address into a loopback base URL ending in "/".
func localURL(listen, base string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "", "8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	base = strings.TrimRight(base, "/") + "/"
	return "http://" + net.JoinHostPort(host, port) + base
}
