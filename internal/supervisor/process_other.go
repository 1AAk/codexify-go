//go:build !windows

package supervisor

import (
	"context"
	"os"
	"os/exec"
)

func configureProcess(cmd *exec.Cmd) {}

func stopProcessTree(ctx context.Context, pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(os.Interrupt)
}

func forceKillProcessTree(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
