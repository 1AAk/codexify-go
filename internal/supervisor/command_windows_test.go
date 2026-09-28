//go:build windows

package supervisor

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestCommandProcessStopsHiddenChild(t *testing.T) {
	factory := &CommandFactory{
		Spec: CommandSpec{
			Path: "cmd.exe",
			Args: []string{"/d", "/s", "/c", "ping -t 127.0.0.1 >nul"},
		},
		Output: io.Discard,
	}
	proc, err := factory.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if proc.PID() <= 0 {
		t.Fatalf("invalid pid %d", proc.PID())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := proc.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proc.Done():
	case <-time.After(time.Second):
		t.Fatal("child did not exit")
	}
}

type countingFactory struct {
	inner  Factory
	starts atomic.Int32
}

func (f *countingFactory) Start(ctx context.Context) (Process, error) {
	f.starts.Add(1)
	return f.inner.Start(ctx)
}

func TestSupervisorRestartsRealExitedProcess(t *testing.T) {
	inner := &CommandFactory{
		Spec: CommandSpec{
			Path: "cmd.exe",
			Args: []string{"/d", "/s", "/c", "exit 17"},
		},
		Output: io.Discard,
	}
	factory := &countingFactory{inner: inner}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	s := &Supervisor{
		Factory: factory,
		Policy: Policy{
			MinBackoff:             5 * time.Millisecond,
			MaxBackoff:             20 * time.Millisecond,
			StableWindow:           time.Second,
			HealthInterval:         time.Second,
			HealthFailureThreshold: 3,
			ShutdownTimeout:        100 * time.Millisecond,
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := factory.starts.Load(); got < 3 {
		t.Fatalf("expected multiple restarts, got %d starts", got)
	}
}
