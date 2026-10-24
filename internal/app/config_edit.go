package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"

	"github.com/williamokano/qbt-replicator/internal/config"
	"github.com/williamokano/qbt-replicator/internal/events"
	"github.com/williamokano/qbt-replicator/internal/replicator"
)

// RunDaemon runs the reconcile loop until ctx is done, restarting it on the
// new engine whenever the configuration is reloaded. With disabled true (the
// --no-daemon flag) or daemon.enabled false it only waits for reloads.
func (s *Service) RunDaemon(ctx context.Context, disabled bool) {
	go s.pruneLoop(ctx)
	go s.notify.Run(ctx)
	for {
		eng := s.Engine()
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		if !disabled && eng.Config().Daemon.Enabled {
			go func() {
				defer close(done)
				eng.Run(runCtx)
			}()
		} else {
			close(done)
		}
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return
		case <-s.reload:
			cancel()
			<-done
		}
	}
}

// Reload swaps in a new engine built from cfg and restarts the daemon loop.
func (s *Service) Reload(cfg *config.Config) error {
	eng, err := replicator.NewEngine(cfg, s.events, s.metrics, s.store)
	if err != nil {
		return err
	}
	if err := s.notify.Configure(context.Background(), cfg); err != nil {
		return err
	}
	eng.OnNotice(s.notify.Notify)
	s.mu.Lock()
	s.engine = eng
	s.mu.Unlock()
	s.metrics.ResetInstances()
	select {
	case s.reload <- struct{}{}:
	default:
	}
	s.events.Add(events.Event{Level: events.Info, Kind: "config",
		Message: fmt.Sprintf("configuration reloaded (%d instances, %d rules)", len(cfg.Instances), len(cfg.Rules))})
	return nil
}

// ConfigFile is what the editor shows.
type ConfigFile struct {
	Path      string   `json:"path"`
	Content   string   `json:"content"` // literal passwords masked
	Overrides []string `json:"env_overrides,omitempty"`
}

// RawConfig returns the configuration file with literal secrets masked.
func (s *Service) RawConfig() (ConfigFile, error) {
	path := s.Config().Path
	out := ConfigFile{Path: path, Overrides: config.EnvOverrides()}
	if path == "" {
		return out, errors.New("configuration was not loaded from a file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	masked, err := config.MaskSecrets(raw)
	if err != nil {
		// Unparseable file on disk: show it as-is so it can be fixed.
		masked = raw
	}
	out.Content = string(masked)
	return out, nil
}

// SaveResult describes an applied configuration change.
type SaveResult struct {
	Saved bool `json:"saved"`
	// RestartRequired lists settings that only take effect after a restart.
	RestartRequired []string `json:"restart_required,omitempty"`
}

// SaveConfig validates edited YAML (masked passwords are restored from the
// current file), writes it atomically (keeping a .bak) and applies it.
func (s *Service) SaveConfig(edited string) (SaveResult, error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	cur := s.Config()
	if cur.Path == "" {
		return SaveResult{}, errors.New("configuration was not loaded from a file")
	}
	prev, _ := os.ReadFile(cur.Path)
	raw, err := config.UnmaskSecrets([]byte(edited), prev)
	if err != nil {
		return SaveResult{}, err
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return SaveResult{}, err
	}
	cfg.Path = cur.Path
	if err := config.SaveFile(cur.Path, raw); err != nil {
		return SaveResult{}, err
	}
	if err := s.Reload(cfg); err != nil {
		return SaveResult{Saved: true}, fmt.Errorf("saved, but applying failed: %w", err)
	}
	res := SaveResult{Saved: true}
	if cfg.Server.Listen != cur.Server.Listen {
		res.RestartRequired = append(res.RestartRequired, "server.listen")
	}
	if cfg.Server.BasePath != cur.Server.BasePath {
		res.RestartRequired = append(res.RestartRequired, "server.base_path")
	}
	if cfg.Log != cur.Log {
		res.RestartRequired = append(res.RestartRequired, "log")
	}
	return res, nil
}

// RawConfigStruct returns the configuration file decoded as written (no ${VAR}
// expansion or QBTR_* overrides) with defaults applied, for the form editor.
func (s *Service) RawConfigStruct() (*config.Config, error) {
	path := s.Config().Path
	if path == "" {
		return nil, errors.New("configuration was not loaded from a file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := config.ParseRaw(raw)
	if err != nil {
		return nil, err
	}
	cfg.Path = path
	return cfg, nil
}

// formHeader is written at the top of files saved from the form editor.
const formHeader = `# qbt-replicator configuration, saved from the web UI form editor.
# Full reference: https://github.com/williamokano/qbt-replicator/blob/main/docs/configuration.md
# ${VAR} in values is read from the environment.
`

// SaveConfigStruct serializes cfg (built by the form editor) and saves it like SaveConfig.
func (s *Service) SaveConfigStruct(cfg *config.Config) (SaveResult, error) {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return SaveResult{}, err
	}
	return s.SaveConfig(formHeader + string(b))
}
