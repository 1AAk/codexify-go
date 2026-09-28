package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/health"
	"github.com/benice2me11/codexify-go/internal/supervisor"
	"github.com/benice2me11/codexify-go/internal/tunnel"
)

func Run(ctx context.Context, cfg config.Config, console bool) error {
	if err := os.MkdirAll(filepath.Dir(cfg.Log.File), 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	logFile, err := os.OpenFile(cfg.Log.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer logFile.Close()

	var out io.Writer = logFile
	if console {
		out = io.MultiWriter(os.Stderr, logFile)
	}

	level := new(slog.LevelVar)
	switch strings.ToLower(cfg.Log.Level) {
	case "debug":
		level.Set(slog.LevelDebug)
	case "warn", "warning":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		level.Set(slog.LevelInfo)
	}
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level}))

	logger.Info("codexify-go starting",
		"service", cfg.Service.Name,
		"tunnel_id", cfg.Tunnel.TunnelID,
		"tunnel_executable", cfg.Tunnel.Executable,
	)

	factory := &tunnel.Factory{Config: cfg, Output: logFile}
	checker := health.NewURLFileChecker(cfg.Tunnel.HealthURLFile)
	s := &supervisor.Supervisor{
		Factory: factory,
		Health:  checker,
		Policy: supervisor.Policy{
			MinBackoff:             cfg.Supervisor.MinBackoff.Duration(),
			MaxBackoff:             cfg.Supervisor.MaxBackoff.Duration(),
			StableWindow:           cfg.Supervisor.StableWindow.Duration(),
			StartupGrace:           cfg.Tunnel.StartupWaitTimeout.Duration() + cfg.Supervisor.HealthInterval.Duration(),
			HealthInterval:         cfg.Supervisor.HealthInterval.Duration(),
			HealthFailureThreshold: cfg.Supervisor.HealthFailureThreshold,
			ShutdownTimeout:        cfg.Supervisor.ShutdownTimeout.Duration(),
		},
		Log: logger,
	}
	err = s.Run(ctx)
	if err != nil {
		logger.Error("supervisor stopped with error", "error", err)
		return err
	}
	logger.Info("codexify-go stopped")
	return nil
}
