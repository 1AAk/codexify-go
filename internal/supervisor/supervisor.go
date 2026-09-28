package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type Process interface {
	PID() int
	Done() <-chan error
	Stop(context.Context) error
}

type Factory interface {
	Start(context.Context) (Process, error)
}

type Checker interface {
	Check(context.Context) error
}

type Policy struct {
	MinBackoff             time.Duration
	MaxBackoff             time.Duration
	StableWindow           time.Duration
	StartupGrace           time.Duration
	HealthInterval         time.Duration
	HealthFailureThreshold int
	ShutdownTimeout        time.Duration
}

type Supervisor struct {
	Factory Factory
	Health  Checker
	Policy  Policy
	Log     *slog.Logger
}

func (s *Supervisor) Run(ctx context.Context) error {
	if s.Factory == nil {
		return errors.New("supervisor factory is nil")
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	backoff := NewBackoff(s.Policy.MinBackoff, s.Policy.MaxBackoff)

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		proc, err := s.Factory.Start(ctx)
		if err != nil {
			delay := backoff.Failure()
			s.Log.Error("child start failed", "error", err, "restart_in", delay.String())
			if !sleepContext(ctx, delay) {
				return nil
			}
			continue
		}

		started := time.Now()
		s.Log.Info("child started", "pid", proc.PID())
		err = s.monitor(ctx, proc)

		if ctx.Err() != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), s.Policy.ShutdownTimeout)
			stopErr := proc.Stop(stopCtx)
			cancel()
			if stopErr != nil {
				s.Log.Warn("child stop failed", "pid", proc.PID(), "error", stopErr)
			}
			return nil
		}

		runtime := time.Since(started)
		if runtime >= s.Policy.StableWindow {
			backoff.Stable()
		}
		delay := backoff.Failure()
		s.Log.Warn("child exited or became unhealthy",
			"pid", proc.PID(),
			"runtime", runtime.String(),
			"error", err,
			"restart_in", delay.String(),
		)
		if !sleepContext(ctx, delay) {
			return nil
		}
	}
}

func (s *Supervisor) monitor(ctx context.Context, proc Process) error {
	if s.Health == nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-proc.Done():
			return err
		}
	}
	if s.Policy.StartupGrace > 0 {
		timer := time.NewTimer(s.Policy.StartupGrace)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-proc.Done():
			timer.Stop()
			if err == nil {
				return errors.New("child exited")
			}
			return err
		case <-timer.C:
		}
	}

	ticker := time.NewTicker(s.Policy.HealthInterval)
	defer ticker.Stop()
	failures := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-proc.Done():
			if err == nil {
				return errors.New("child exited")
			}
			return err
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, minDuration(2*time.Second, s.Policy.HealthInterval))
			err := s.Health.Check(checkCtx)
			cancel()
			if err == nil {
				failures = 0
				continue
			}
			failures++
			s.Log.Warn("child health check failed", "pid", proc.PID(), "failures", failures, "error", err)
			if failures < s.Policy.HealthFailureThreshold {
				continue
			}

			stopCtx, cancel := context.WithTimeout(context.Background(), s.Policy.ShutdownTimeout)
			stopErr := proc.Stop(stopCtx)
			cancel()
			if stopErr != nil {
				s.Log.Warn("failed to stop unhealthy child", "pid", proc.PID(), "error", stopErr)
			}
			return fmt.Errorf("health threshold reached: %w", err)
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
