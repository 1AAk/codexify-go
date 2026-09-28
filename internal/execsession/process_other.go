//go:build !windows

package execsession

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopProcessTree(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
