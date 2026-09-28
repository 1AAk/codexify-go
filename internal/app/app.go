package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/health"
	"github.com/benice2me11/codexify-go/internal/mcpserver"
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

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	mcpRuntime, err := mcpserver.New(cfg, logger)
	if err != nil {
		return fmt.Errorf("start MCP runtime: %w", err)
	}
	authRef, authEnv := mcpRuntime.TunnelEnvironment()
	mcpserver.MergeTunnelEnvironment(&cfg, authRef, authEnv)

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

	mcpErr := make(chan error, 1)
	go func() { mcpErr <- mcpRuntime.Serve() }()
	supervisorErr := make(chan error, 1)
	go func() { supervisorErr <- s.Run(runCtx) }()

	var runErr error
	supervisorFinished := false
	mcpFinished := false
	select {
	case <-ctx.Done():
	case err := <-mcpErr:
		mcpFinished = true
		if err != nil {
			runErr = fmt.Errorf("MCP server stopped: %w", err)
		}
	case err := <-supervisorErr:
		supervisorFinished = true
		if err != nil {
			runErr = fmt.Errorf("tunnel supervisor stopped: %w", err)
		}
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Supervisor.ShutdownTimeout.Duration())
	defer shutdownCancel()
	if err := mcpRuntime.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shutdown MCP server: %w", err)
	}
	if !supervisorFinished {
		select {
		case err := <-supervisorErr:
			if err != nil && runErr == nil {
				runErr = fmt.Errorf("shutdown tunnel supervisor: %w", err)
			}
		case <-shutdownCtx.Done():
			if runErr == nil {
				runErr = errors.New("timed out waiting for tunnel supervisor shutdown")
			}
		}
	}
	if !mcpFinished {
		select {
		case err := <-mcpErr:
			if err != nil && runErr == nil {
				runErr = fmt.Errorf("MCP server shutdown: %w", err)
			}
		default:
		}
	}
	if runErr != nil {
		logger.Error("codexify-go stopped with error", "error", runErr)
		return runErr
	}
	logger.Info("codexify-go stopped")
	return nil
}
