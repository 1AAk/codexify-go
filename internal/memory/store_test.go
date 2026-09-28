package memory

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/config"
)

func TestMemoryCreateUpdateDelete(t *testing.T) {
	store := New(config.MemoryConfig{Enabled: true, Dir: t.TempDir(), MaxBytes: 1024}, false)
	workDir := filepath.Join(t.TempDir(), "project")

	if _, err := store.Create(workDir, "approach", "use the API"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(workDir, "approach", "overwrite"); err == nil {
		t.Fatal("expected duplicate create to fail")
	}
	recalled, err := store.Recall(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recalled, "approach: use the API") {
		t.Fatalf("recall = %q", recalled)
	}
	if _, err := store.Update(workDir, "approach", "use the SDK"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(workDir, "missing", "value"); err == nil {
		t.Fatal("expected missing update to fail")
	}
	if _, err := store.Delete(workDir, "approach"); err != nil {
		t.Fatal(err)
	}
	recalled, err = store.Recall(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recalled, "Nothing remembered") {
		t.Fatalf("recall after delete = %q", recalled)
	}
}

func TestMemoryBudget(t *testing.T) {
	store := New(config.MemoryConfig{Enabled: true, Dir: t.TempDir(), MaxBytes: 64}, false)
	if _, err := store.Create(t.TempDir(), "large", strings.Repeat("x", 256)); err == nil {
		t.Fatal("expected budget rejection")
	}
}
