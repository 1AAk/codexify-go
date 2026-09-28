//go:build windows

package supervisor

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

func stopProcessTree(ctx context.Context, pid int) error {
	cmd := exec.CommandContext(ctx, "taskkill.exe", "/T", "/PID", strconv.Itoa(pid))
	configureProcess(cmd)
	return cmd.Run()
}

func forceKillProcessTree(pid int) error {
	cmd := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(pid))
	configureProcess(cmd)
	return cmd.Run()
}

func waitProcessTreeExit(context.Context, int) error { return nil }
