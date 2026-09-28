package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/benice2me11/codexify-go/internal/app"
	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/service"
	"github.com/benice2me11/codexify-go/internal/worker"
	"github.com/benice2me11/codexify-go/internal/workspace"
)

const version = "0.3.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "version", "--version", "-version":
		fmt.Println(version)
		return nil
	case "run":
		return runForeground(args[1:])
	case "doctor":
		return doctor(args[1:])
	case "service":
		return serviceCommand(args[1:])
	case "worker":
		return workerCommand(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runForeground(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	path := fs.String("config", "config.json", "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, cfg, mustAbs(*path), true)
}

func workerCommand(args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return errors.New("worker subcommand required: run")
	}
	fs := flag.NewFlagSet("worker run", flag.ContinueOnError)
	path := fs.String("config", "config.json", "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return worker.Run(ctx, cfg, false)
}

func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	path := fs.String("config", "config.json", "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	info, err := os.Stat(cfg.Tunnel.Executable)
	if err != nil {
		return fmt.Errorf("tunnel executable: %w", err)
	}
	if info.IsDir() {
		return errors.New("tunnel executable points to a directory")
	}
	root, err := workspace.New(cfg.MCP.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("workspace root: %w", err)
	}
	fmt.Println("PASS config:", mustAbs(*path))
	fmt.Println("PASS tunnel executable:", cfg.Tunnel.Executable)
	fmt.Println("PASS MCP endpoint:", cfg.Tunnel.MCPServerURL)
	fmt.Println("PASS workspace root:", root.Path())
	fmt.Println("PASS health url file:", cfg.Tunnel.HealthURLFile)
	fmt.Println("PASS service name:", cfg.Service.Name)
	return nil
}

func serviceCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("service subcommand required: install|start|stop|restart|remove|status|run")
	}
	sub := args[0]
	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	path := fs.String("config", "config.json", "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}

	switch sub {
	case "install":
		exe, err := service.ExecutablePath()
		if err != nil {
			return err
		}
		if err := service.Install(exe, mustAbs(*path), cfg); err != nil {
			return err
		}
		fmt.Println("installed", cfg.Service.Name)
		return nil
	case "start":
		return service.Start(cfg.Service.Name)
	case "stop":
		return service.Stop(cfg.Service.Name)
	case "restart":
		return service.Restart(cfg.Service.Name)
	case "remove":
		return service.Remove(cfg.Service.Name)
	case "status":
		st, err := service.Query(cfg.Service.Name)
		if err != nil {
			return err
		}
		if !st.Installed {
			fmt.Println("not installed")
			return nil
		}
		fmt.Println("installed; state:", service.StateString(st.State))
		return nil
	case "run":
		return service.Run(cfg.Service.Name, cfg, mustAbs(*path))
	default:
		return fmt.Errorf("unknown service subcommand %q", sub)
	}
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func printUsage() {
	fmt.Print(`codexify-go - Codexify-compatible Go runtime (Phase 1)

Usage:
  codexify-go run --config config.json
  codexify-go doctor --config config.json
  codexify-go service install|start|stop|restart|remove|status --config config.json
  codexify-go worker run --config config.json
  codexify-go version
`)
}
