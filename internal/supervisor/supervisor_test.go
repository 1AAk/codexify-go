package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeProcess struct {
	pid   int
	done  chan error
	stops atomic.Int32
}

func (p *fakeProcess) PID() int           { return p.pid }
func (p *fakeProcess) Done() <-chan error { return p.done }
func (p *fakeProcess) Stop(context.Context) error {
	p.stops.Add(1)
	select {
	case p.done <- errors.New("stopped"):
	default:
	}
	return nil
}

type fakeFactory struct {
	mu      sync.Mutex
	starts  int
	process []*fakeProcess
}

func (f *fakeFactory) Start(context.Context) (Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	p := &fakeProcess{pid: f.starts, done: make(chan error, 1)}
	f.process = append(f.process, p)
	if f.starts == 1 {
		go func() {
			time.Sleep(5 * time.Millisecond)
			p.done <- errors.New("boom")
		}()
	}
	return p, nil
}

func TestSupervisorRestartsExitedChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	f := &fakeFactory{}
	s := &Supervisor{
		Factory: f,
		Policy: Policy{
			MinBackoff:             5 * time.Millisecond,
			MaxBackoff:             10 * time.Millisecond,
			StableWindow:           time.Second,
			HealthInterval:         time.Second,
			HealthFailureThreshold: 3,
			ShutdownTimeout:        10 * time.Millisecond,
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts < 2 {
		t.Fatalf("expected restart, starts=%d", f.starts)
	}
}
