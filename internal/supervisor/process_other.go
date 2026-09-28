//go:build !windows

package supervisor

import (
	"context"
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopProcessTree(ctx context.Context, pid int) error {
	return syscall.Kill(-pid, syscall.SIGINT)
}

func forceKillProcessTree(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
