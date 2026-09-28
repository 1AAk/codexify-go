//go:build windows

package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

type ApplyOptions struct {
	Source      string
	Target      string
	WaitPID     int
	ServiceName string
	Restart     bool
	HelperPath  string
}

func ScheduleApply(prepared Prepared, target, serviceName string, restart bool) (string, error) {
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	sourceAbs, err := filepath.Abs(prepared.BinaryPath)
	if err != nil {
		return "", err
	}
	if filepath.Clean(sourceAbs) == filepath.Clean(targetAbs) {
		return "", errors.New("prepared update source equals target executable")
	}
	helper := filepath.Join(filepath.Dir(sourceAbs), fmt.Sprintf("codexify-go-update-helper-%d.exe", time.Now().UnixNano()))
	if err := copyFile(sourceAbs, helper, 0o700); err != nil {
		return "", fmt.Errorf("create update helper: %w", err)
	}
	args := []string{
		"update-worker",
		"--source", sourceAbs,
		"--target", targetAbs,
		"--wait-pid", strconv.Itoa(os.Getpid()),
		"--helper", helper,
	}
	if serviceName != "" {
		args = append(args, "--service", serviceName)
	}
	if restart {
		args = append(args, "--restart-service")
	}
	cmd := exec.Command(helper, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(helper)
		return "", err
	}
	return helper, nil
}

func RunApplyWorker(options ApplyOptions) error {
	if options.Source == "" || options.Target == "" || options.WaitPID <= 0 {
		return errors.New("update worker requires source, target, and wait-pid")
	}
	if err := waitForProcessExit(uint32(options.WaitPID), 2*time.Minute); err != nil {
		return err
	}
	source, err := windows.UTF16PtrFromString(options.Source)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(options.Target)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}
	if options.Restart && options.ServiceName != "" {
		cmd := exec.Command("sc.exe", "start", options.ServiceName)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("restart service: %w: %s", err, out)
		}
	}
	if options.HelperPath != "" {
		helper, err := windows.UTF16PtrFromString(options.HelperPath)
		if err == nil {
			_ = windows.MoveFileEx(helper, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
		}
	}
	return nil
}

func waitForProcessExit(pid uint32, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(handle)
	ms := uint32(timeout / time.Millisecond)
	result, err := windows.WaitForSingleObject(handle, ms)
	if err != nil {
		return err
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return nil
	case uint32(windows.WAIT_TIMEOUT):
		return errors.New("timed out waiting for current codexify-go process to exit")
	default:
		return fmt.Errorf("unexpected process wait result %#x", result)
	}
}

func copyFile(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := out.ReadFrom(in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
