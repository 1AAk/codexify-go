//go:build !windows

package supervisor

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestStopProcessTreeKillsGrandchild(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & child=$!; echo $child; wait")
	configureProcess(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, err := out.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(string(trimSpace(buf[:n])))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := stopProcessTree(ctx, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := syscall.Kill(child, 0); err == nil {
		t.Fatalf("grandchild %d survived process-group stop", child)
	}
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}
