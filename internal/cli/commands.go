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

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/replicator"
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
			svc, err := g.service()
			if err != nil {
				return err
			}
			cfg := g.cfg
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
			if cfg.Daemon.Enabled && !noDaemon {
				wg.Add(1)
				go func() {
					defer wg.Done()
					svc.Engine().Run(ctx)
				}()
			} else {
				g.logger.Info("daemon disabled; UI/API only")
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
			svc, err := g.service()
			if err != nil {
				return err
			}
			rep := svc.SyncOnce(ctxOf(cmd), dry)
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
			svc, err := g.service()
			if err != nil {
				return err
			}
			return printPlan(cmd.OutOrStdout(), g.output, svc.Plan(ctxOf(cmd)))
		},
	}
}

func newStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show instance health, connection status and speeds",
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := g.service()
			if err != nil {
				return err
			}
			st := svc.Status(ctxOf(cmd))
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
			if err := g.load(); err != nil {
				return err
			}
			red := g.cfg.Redacted()
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
		Use:   "check",
		Short: "Check connectivity, credentials and versions of every instance",
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := g.service()
			if err != nil {
				return err
			}
			st := svc.Status(ctxOf(cmd))
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
			svc, err := g.service()
			if err != nil {
				return err
			}
			f.Fresh = true
			rows := svc.Torrents(ctxOf(cmd), f)
			return printTorrents(cmd.OutOrStdout(), g.output, g.cfg.InstanceNames(), rows)
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
			svc, err := g.service()
			if err != nil {
				return err
			}
			req.Hash = strings.ToLower(args[0])
			res, err := svc.Replicate(ctxOf(cmd), req)
			if err != nil {
				return err
			}
			return printResults(cmd.OutOrStdout(), g.output, res)
		},
	}
	cmd.Flags().StringVar(&req.From, "from", "", "source instance (default: first instance owning the torrent)")
	cmd.Flags().StringSliceVar(&req.To, "to", nil, "target instances (default: all others)")
	cmd.Flags().BoolVar(&req.Force, "force", false, "ignore rule filters and settle delay")
	cmd.Flags().BoolVar(&req.DryRun, "dry-run", false, "show what would be done")
	return cmd
}

func newUnreplicateCmd(g *globals) *cobra.Command {
	var on []string
	var dry bool
	cmd := &cobra.Command{
		Use:   "unreplicate <hash>",
		Short: "Remove replicas of a torrent (files are always kept)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := g.service()
			if err != nil {
				return err
			}
			res, err := svc.Unreplicate(ctxOf(cmd), strings.ToLower(args[0]), on, dry)
			if err != nil {
				return err
			}
			return printResults(cmd.OutOrStdout(), g.output, res)
		},
	}
	cmd.Flags().StringSliceVar(&on, "on", nil, "instances to remove the replica from (default: all replicas)")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "show what would be done")
	return cmd
}

func newEventsCmd(g *globals) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show the activity log of a running daemon",
		Long:  "Fetches /api/v1/events from a running daemon (events live in the daemon's memory).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.load(); err != nil {
				return err
			}
			return remoteEvents(ctxOf(cmd), cmd.OutOrStdout(), g, limit)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "number of events")
	return cmd
}

func newConfigCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Validate or display configuration"}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Validate the configuration file",
		RunE: func(cmd *cobra.Command, _ []string) error {
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
		Use:   "show",
		Short: "Print the effective configuration with secrets redacted",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := g.load(); err != nil {
				return err
			}
			red := g.cfg.Redacted()
			if g.output == "json" {
				return printJSON(cmd.OutOrStdout(), red)
			}
			b, err := yaml.Marshal(red)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(b)
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

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
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
