// Package cli implements the cobra command tree. Commands are thin adapters
// over app.Service, the same façade used by the web server.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/williamokano/qbt-replicator/internal/app"
	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/version"
)

type globals struct {
	configPath string
	logLevel   string
	output     string

	cfg    *config.Config
	logger *slog.Logger
	svc    *app.Service
}

// Execute runs the root command.
func Execute() int {
	if err := NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// NewRoot builds the command tree.
func NewRoot() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "qbt-replicator",
		Short: "Replicate completed torrents across qBittorrent instances that share storage",
		Long: `qbt-replicator watches several qBittorrent instances that share the same
storage. When a torrent completes on a source instance it is exported and added
to the target instances at the same path, so it seeds from several IPs/tunnels.

The configuration file is looked up in this order:
  --config flag, $QBTR_CONFIG, ` + config.DefaultPath + `, ./config.yaml`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.SetVersionTemplate(version.String() + "\n")
	pf := root.PersistentFlags()
	pf.StringVarP(&g.configPath, "config", "c", "", "config file (default $QBTR_CONFIG or "+config.DefaultPath+")")
	pf.StringVar(&g.logLevel, "log-level", "", "override log.level (debug|info|warn|error)")
	pf.StringVarP(&g.output, "output", "o", "table", "output format: table|json")

	root.AddCommand(
		newServeCmd(g),
		newSyncCmd(g),
		newPlanCmd(g),
		newStatusCmd(g),
		newInstancesCmd(g),
		newTorrentsCmd(g),
		newReplicateCmd(g),
		newUnreplicateCmd(g),
		newEventsCmd(g),
		newConfigCmd(g),
		newHealthcheckCmd(g),
		newVersionCmd(),
	)
	return root
}

// load reads the config and sets up logging.
func (g *globals) load() error {
	if g.cfg != nil {
		return nil
	}
	cfg, err := config.Load(config.ResolvePath(g.configPath))
	if err != nil {
		return err
	}
	if g.logLevel != "" {
		cfg.Log.Level = g.logLevel
	}
	g.cfg = cfg
	g.logger = NewLogger(cfg.Log, os.Stderr)
	slog.SetDefault(g.logger)
	return nil
}

// service loads config and builds the shared service.
func (g *globals) service() (*app.Service, error) {
	if g.svc != nil {
		return g.svc, nil
	}
	if err := g.load(); err != nil {
		return nil, err
	}
	svc, err := app.New(g.cfg, g.logger)
	if err != nil {
		return nil, err
	}
	g.svc = svc
	return svc, nil
}

// NewLogger builds a slog logger from config.
func NewLogger(c config.LogConfig, w io.Writer) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(c.Level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.EqualFold(c.Format, "json") {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func ctxOf(cmd *cobra.Command) context.Context {
	if cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}
