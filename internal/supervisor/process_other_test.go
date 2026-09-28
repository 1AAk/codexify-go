//go:build !windows

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestForceKillProcessTreeKillsGrandchild(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & child=$!; echo $child; wait")
	configureProcess(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := readPID(t, out)
	if err := forceKillProcessTree(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitProcessNotLive(t, child, 2*time.Second)
}

func TestCommandStopForceKillsSignalIgnoringProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := fmt.Sprintf("trap '' INT; sleep 300 & child=$!; echo $child > %s; wait", shellQuote(pidFile))
	factory := &CommandFactory{Spec: CommandSpec{Path: "/bin/sh", Args: []string{"-c", script}}}
	process, err := factory.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	child := waitPIDFile(t, pidFile, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err = process.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop error=%v, want context deadline after forced group kill", err)
	}
	waitProcessNotLive(t, child, 2*time.Second)
}

func readPID(t *testing.T, out interface{ Read([]byte) (int, error) }) int {
	t.Helper()
	buf := make([]byte, 32)
	n, err := out.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PID file %s was not populated", path)
	return 0
}

func waitProcessNotLive(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processLive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d is still live after %s", pid, timeout)
}

func processLive(pid int) bool {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			return false
		}
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) >= 3 && fields[2] == "Z" {
				return false
			}
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
