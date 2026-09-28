package tunnel

import (
	"context"
	"io"
	"os"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/supervisor"
)

type Factory struct {
	Config config.Config
	Output io.Writer
}

func (f *Factory) Start(ctx context.Context) (supervisor.Process, error) {
	_ = os.Remove(f.Config.Tunnel.HealthURLFile)
	cf := supervisor.CommandFactory{
		Spec: supervisor.CommandSpec{
			Path: f.Config.Tunnel.Executable,
			Args: f.Config.TunnelArgs(),
			Env:  f.Config.Tunnel.Environment,
		},
		Output: f.Output,
	}
	return cf.Start(ctx)
}
