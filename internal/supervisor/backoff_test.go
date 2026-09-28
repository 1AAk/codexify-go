package supervisor

import (
	"testing"
	"time"
)

func TestBackoffCapsAndResets(t *testing.T) {
	b := NewBackoff(time.Second, 4*time.Second)
	got := []time.Duration{b.Failure(), b.Failure(), b.Failure(), b.Failure()}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("failure %d: got %s want %s", i, got[i], want[i])
		}
	}
	b.Stable()
	if got := b.Failure(); got != time.Second {
		t.Fatalf("after reset got %s want 1s", got)
	}
}
