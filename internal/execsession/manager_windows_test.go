//go:build windows

package execsession

import (
	"strings"
	"testing"
	"time"
)

func TestQuickCommand(t *testing.T) {
	m := NewManager()
	defer m.Close()
	res, err := m.Start(StartInput{
		Command: "Write-Output 'hello'",
		Yield:   2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Running {
		t.Fatal("expected quick command to finish")
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("unexpected exit code: %#v", res.ExitCode)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestLongCommandReturnsSessionAndPolls(t *testing.T) {
	m := NewManager()
	defer m.Close()
	res, err := m.Start(StartInput{
		Command: "Write-Output 'start'; Start-Sleep -Milliseconds 500; Write-Output 'done'",
		Yield:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Running || res.SessionID == "" {
		t.Fatalf("expected running session: %+v", res)
	}
	deadline := time.Now().Add(3 * time.Second)
	for res.Running && time.Now().Before(deadline) {
		res, err = m.Write(res.SessionID, "", 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
	}
	if res.Running {
		t.Fatal("expected session to finish")
	}
	if !strings.Contains(res.Output, "done") {
		t.Fatalf("output = %q", res.Output)
	}
}
