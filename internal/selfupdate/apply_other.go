//go:build !windows

package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
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
	data, err := os.ReadFile(sourceAbs)
	if err != nil {
		return "", err
	}
	tmp := targetAbs + ".update"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, targetAbs); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return targetAbs, nil
}

func RunApplyWorker(ApplyOptions) error {
	return errors.New("update-worker is only used on Windows")
}
