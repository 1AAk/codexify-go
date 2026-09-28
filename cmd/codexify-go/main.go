package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/benice2me11/codexify-go/internal/app"
	"github.com/benice2me11/codexify-go/internal/buildinfo"
	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/selfupdate"
	"github.com/benice2me11/codexify-go/internal/service"
	"github.com/benice2me11/codexify-go/internal/tunnel"
	"github.com/benice2me11/codexify-go/internal/worker"
	"github.com/benice2me11/codexify-go/internal/workspace"
)

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
		fmt.Println(buildinfo.Version)
		return nil
	case "run":
		return runForeground(args[1:])
	case "doctor":
		return doctor(args[1:])
	case "service":
		return serviceCommand(args[1:])
	case "tunnel":
		return tunnelCommand(args[1:])
	case "update":
		return updateCommand(args[1:])
	case "update-worker":
		return updateWorkerCommand(args[1:])
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
	if cfg.Tunnel.Executable != "" {
		info, err := os.Stat(cfg.Tunnel.Executable)
		if err != nil {
			return fmt.Errorf("tunnel executable: %w", err)
		}
		if info.IsDir() {
			return errors.New("tunnel executable points to a directory")
		}
		fmt.Println("PASS tunnel executable:", cfg.Tunnel.Executable)
	} else {
		status := tunnel.ManagedStatus(context.Background(), cfg.Tunnel.ManagedDir)
		if status.Verified {
			fmt.Printf("PASS managed tunnel runtime: v%s %s\n", status.Version, status.Path)
		} else {
			fmt.Printf("WARN managed tunnel runtime: %s (run `codexify-go tunnel install --config %s`)\n", status.Detail, *path)
		}
	}
	root, err := workspace.New(cfg.MCP.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("workspace root: %w", err)
	}
	fmt.Println("PASS config:", mustAbs(*path))
	fmt.Println("PASS MCP endpoint:", cfg.Tunnel.MCPServerURL)
	fmt.Println("PASS workspace root:", root.Path())
	fmt.Println("PASS health url file:", cfg.Tunnel.HealthURLFile)
	fmt.Println("PASS service name:", cfg.Service.Name)
	return nil
}

func tunnelCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("tunnel subcommand required: install|status|verify")
	}
	sub := args[0]
	fs := flag.NewFlagSet("tunnel "+sub, flag.ContinueOnError)
	path := fs.String("config", "config.json", "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	switch sub {
	case "install":
		if cfg.Tunnel.Executable != "" {
			return errors.New("tunnel.executable is explicit; managed install is not used")
		}
		resolved, err := tunnel.EnsureManaged(ctx, cfg.Tunnel.ManagedDir)
		if err != nil {
			return err
		}
		fmt.Printf("installed verified tunnel-client-runtime v%s: %s\n", tunnel.ClientVersion, resolved)
		return nil
	case "status":
		if cfg.Tunnel.Executable != "" {
			fmt.Println("explicit tunnel executable:", cfg.Tunnel.Executable)
			return nil
		}
		status := tunnel.ManagedStatus(ctx, cfg.Tunnel.ManagedDir)
		fmt.Printf("managed=%t installed=%t verified=%t version=%s path=%s\n", status.Managed, status.Installed, status.Verified, status.Version, status.Path)
		if status.Detail != "" {
			fmt.Println("detail:", status.Detail)
		}
		return nil
	case "verify":
		if cfg.Tunnel.Executable != "" {
			if err := tunnel.ValidateClient(ctx, cfg.Tunnel.Executable, ""); err != nil {
				return err
			}
			fmt.Println("verified explicit tunnel runtime:", cfg.Tunnel.Executable)
			return nil
		}
		status := tunnel.ManagedStatus(ctx, cfg.Tunnel.ManagedDir)
		if !status.Verified {
			return fmt.Errorf("managed tunnel runtime is not verified: %s", status.Detail)
		}
		fmt.Printf("verified managed tunnel runtime v%s: %s\n", status.Version, status.Path)
		return nil
	default:
		return fmt.Errorf("unknown tunnel subcommand %q", sub)
	}
}

func updateCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("update subcommand required: check|apply")
	}
	sub := args[0]
	fs := flag.NewFlagSet("update "+sub, flag.ContinueOnError)
	path := fs.String("config", "config.json", "config file")
	force := fs.Bool("force", false, "force a fresh release check")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch sub {
	case "check":
		inspection := selfupdate.Inspect(ctx, buildinfo.Version, *force)
		fmt.Printf("status=%s current=%s", inspection.Status, inspection.CurrentVersion)
		if inspection.LatestVersion != "" {
			fmt.Printf(" latest=%s", inspection.LatestVersion)
		}
		if inspection.Source != "" {
			fmt.Printf(" source=%s", inspection.Source)
		}
		fmt.Println()
		if inspection.Detail != "" {
			fmt.Println("detail:", inspection.Detail)
		}
		return nil
	case "apply":
		cfg, err := config.Load(*path)
		if err != nil {
			return err
		}
		configAbs := mustAbs(*path)
		stageDir := filepath.Join(filepath.Dir(configAbs), ".codexify-go", "updates")
		prepared, err := selfupdate.Prepare(ctx, buildinfo.Version, stageDir)
		if err != nil {
			return err
		}
		target, err := os.Executable()
		if err != nil {
			return err
		}
		target, err = filepath.Abs(target)
		if err != nil {
			return err
		}
		restartService := false
		if status, queryErr := service.Query(cfg.Service.Name); queryErr == nil && service.IsRunning(status) {
			if err := service.Stop(cfg.Service.Name); err != nil {
				return fmt.Errorf("stop service before update: %w", err)
			}
			restartService = true
		}
		helper, err := selfupdate.ScheduleApply(prepared, target, cfg.Service.Name, restartService)
		if err != nil {
			if restartService {
				_ = service.Start(cfg.Service.Name)
			}
			return err
		}
		fmt.Printf("scheduled update %s -> %s\n", prepared.CurrentVersion, prepared.TargetVersion)
		if helper != "" && helper != target {
			fmt.Println("update helper:", helper)
		}
		if restartService && helper == target {
			if err := service.Start(cfg.Service.Name); err != nil {
				return fmt.Errorf("restart service after update: %w", err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown update subcommand %q", sub)
	}
}

func updateWorkerCommand(args []string) error {
	fs := flag.NewFlagSet("update-worker", flag.ContinueOnError)
	source := fs.String("source", "", "prepared replacement executable")
	target := fs.String("target", "", "installed executable to replace")
	waitPID := fs.String("wait-pid", "", "process id that must exit before replacement")
	serviceName := fs.String("service", "", "optional service name to restart")
	restartService := fs.Bool("restart-service", false, "restart the service after replacement")
	helper := fs.String("helper", "", "helper executable path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pid, err := strconv.Atoi(*waitPID)
	if err != nil || pid <= 0 {
		return errors.New("update-worker requires a valid --wait-pid")
	}
	return selfupdate.RunApplyWorker(selfupdate.ApplyOptions{
		Source:      *source,
		Target:      *target,
		WaitPID:     pid,
		ServiceName: *serviceName,
		Restart:     *restartService,
		HelperPath:  *helper,
	})
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
  codexify-go tunnel install|status|verify --config config.json
  codexify-go update check [--force]
  codexify-go update apply --config config.json
  codexify-go worker run --config config.json
  codexify-go version
`)
}
