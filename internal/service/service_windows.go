//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/benice2me11/codexify-go/internal/app"
	"github.com/benice2me11/codexify-go/internal/config"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type Status struct {
	Installed bool
	State     svc.State
}

func IsRunning(status Status) bool {
	return status.Installed && status.State == svc.Running
}

func StateString(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start-pending"
	case svc.StopPending:
		return "stop-pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue-pending"
	case svc.PausePending:
		return "pause-pending"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown(%d)", state)
	}
}

func Install(exePath, configPath string, cfg config.Config) error {
	exePath, err := filepath.Abs(exePath)
	if err != nil {
		return err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect service manager: %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(cfg.Service.Name); err == nil {
		existing.Close()
		return fmt.Errorf("service %q already exists", cfg.Service.Name)
	}

	s, err := m.CreateService(
		cfg.Service.Name,
		exePath,
		mgr.Config{
			StartType:    mgr.StartAutomatic,
			ErrorControl: mgr.ErrorNormal,
			DisplayName:  cfg.Service.DisplayName,
			Description:  cfg.Service.Description,
		},
		"service", "run", "--config", configPath,
	)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if err := s.SetRecoveryActions(actions, 24*60*60); err != nil {
		_ = s.Delete()
		return fmt.Errorf("set recovery actions: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		_ = s.Delete()
		return fmt.Errorf("enable recovery for non-crash failures: %w", err)
	}
	return nil
}

func Start(name string) error {
	s, disconnect, err := open(name)
	if err != nil {
		return err
	}
	defer disconnect()
	if err := s.Start(); err != nil {
		return err
	}
	return waitState(s, svc.Running, 15*time.Second)
}

func Stop(name string) error {
	s, disconnect, err := open(name)
	if err != nil {
		return err
	}
	defer disconnect()
	status, err := s.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return err
	}
	return waitState(s, svc.Stopped, 20*time.Second)
}

func Restart(name string) error {
	if err := Stop(name); err != nil {
		return err
	}
	return Start(name)
}

func Remove(name string) error {
	s, disconnect, err := open(name)
	if err != nil {
		return err
	}
	defer disconnect()
	status, qerr := s.Query()
	if qerr == nil && status.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		_ = waitState(s, svc.Stopped, 20*time.Second)
	}
	return s.Delete()
}

func Query(name string) (Status, error) {
	m, err := mgr.Connect()
	if err != nil {
		return Status{}, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return Status{Installed: false}, nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return Status{}, err
	}
	return Status{Installed: true, State: st.State}, nil
}

func Run(name string, cfg config.Config, configPath string) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isService {
		return errors.New("service run must be started by Windows Service Control Manager")
	}
	return svc.Run(name, &handler{cfg: cfg, configPath: configPath})
}

type handler struct {
	cfg        config.Config
	configPath string
}

func (h *handler) Execute(_ []string, changes <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, h.cfg, h.configPath, false)
	}()

	status <- svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown,
	}

	for {
		select {
		case err := <-done:
			if err != nil {
				return false, 1
			}
			return false, 0
		case req := <-changes:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case err := <-done:
					if err != nil {
						return false, 1
					}
				case <-time.After(h.cfg.Supervisor.ShutdownTimeout.Duration() + 2*time.Second):
					return false, 2
				}
				return false, 0
			}
		}
	}
}

func open(name string) (*mgr.Service, func(), error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(name)
	if err != nil {
		m.Disconnect()
		return nil, nil, err
	}
	return s, func() {
		s.Close()
		m.Disconnect()
	}, nil
}

func waitState(s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("service did not reach state %v within %s", want, timeout)
}

func ExecutablePath() (string, error) {
	return os.Executable()
}
