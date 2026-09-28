//go:build !windows

package execsession

import (
	"os"
	"os/exec"
)

func configureProcess(cmd *exec.Cmd) {}

func stopProcessTree(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
