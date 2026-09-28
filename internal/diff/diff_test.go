package diff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/config"
)

func TestPersistentProjectOpenAndIncrementalCheckpoints(t *testing.T) {
	repo := initRepo(t)
	owner := Owner{Key: strings.Repeat("a", 64), Persistent: true}
	manager := New(config.DiffConfig{MaxPatchBytes: 1 << 20})
	if err := manager.Ensure(repo, owner); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := manager.Show(repo, owner, Request{
		Since:        BaselineProjectOpen,
		Advance:      true,
		IncludePatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Summary.Files != 2 || !first.CheckpointAdvanced {
		t.Fatalf("first=%+v", first)
	}
	if !first.PatchIncluded || !strings.Contains(first.Patch, "tracked.txt") || !strings.Contains(first.Patch, "new.txt") {
		t.Fatalf("patch=%q", first.Patch)
	}

	none, err := manager.Show(repo, owner, Request{
		Since:        BaselineLastDiff,
		Advance:      false,
		IncludePatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if none.Summary.Files != 0 {
		t.Fatalf("expected no incremental changes: %+v", none)
	}

	if err := os.WriteFile(filepath.Join(repo, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	incremental, err := manager.Show(repo, owner, Request{
		Since:        BaselineLastDiff,
		Advance:      false,
		IncludePatch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Summary.Files != 1 || incremental.Files[0].Path != "later.txt" {
		t.Fatalf("incremental=%+v", incremental)
	}
	if incremental.CheckpointAdvanced {
		t.Fatal("advance=false moved checkpoint")
	}

	all, err := manager.Show(repo, owner, Request{
		Since:        BaselineProjectOpen,
		Advance:      false,
		IncludePatch: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if all.Summary.Files != 3 {
		t.Fatalf("project-open diff=%+v", all)
	}
	if all.PatchIncluded || all.PatchOmittedReason == "" {
		t.Fatalf("patch flags=%+v", all)
	}
}

func TestTransientCheckpointCanBeForgotten(t *testing.T) {
	repo := initRepo(t)
	owner := Owner{Key: "transport-session-test", Persistent: false}
	manager := New(config.DiffConfig{MaxPatchBytes: 1 << 20})
	if err := manager.Ensure(repo, owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := manager.Show(repo, owner, Request{Since: BaselineLastDiff, Advance: true})
	if err != nil {
		t.Fatal(err)
	}
	if before.Summary.Files != 1 || !before.CheckpointAdvanced {
		t.Fatalf("before=%+v", before)
	}

	manager.Forget(owner)
	after, err := manager.Show(repo, owner, Request{Since: BaselineLastDiff, Advance: false})
	if err != nil {
		t.Fatal(err)
	}
	if after.Summary.Files != 0 {
		t.Fatalf("forgotten transport checkpoint should restart from current state: %+v", after)
	}
}

func TestSubdirectoryScopeExcludesOutsideChanges(t *testing.T) {
	repo := initRepo(t)
	sub := filepath.Join(repo, "service")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inside.txt"), []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "service/inside.txt")
	runTestGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "subdir")

	manager := New(config.DiffConfig{MaxPatchBytes: 1 << 20})
	owner := Owner{Key: strings.Repeat("b", 64), Persistent: true}
	if err := manager.Ensure(sub, owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inside.txt"), []byte("inside changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "outside.txt"), []byte("outside changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Show(sub, owner, Request{Since: BaselineProjectOpen, Advance: false, IncludePatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Scope != "service" || result.Summary.Files != 1 || result.Files[0].Path != "inside.txt" {
		t.Fatalf("result=%+v", result)
	}
	if strings.Contains(result.Patch, "outside.txt") || !strings.Contains(result.Patch, "inside.txt") {
		t.Fatalf("patch=%q", result.Patch)
	}
}

func TestDeletedAndBinaryFilesAreReported(t *testing.T) {
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "delete.txt"), []byte("remove me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "binary.bin"), []byte{0, 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "delete.txt", "binary.bin")
	runTestGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "binary")

	manager := New(config.DiffConfig{MaxPatchBytes: 1 << 20})
	owner := Owner{Key: strings.Repeat("c", 64), Persistent: true}
	if err := manager.Ensure(repo, owner); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "binary.bin"), []byte{0, 1, 9, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Show(repo, owner, Request{Since: BaselineProjectOpen, Advance: false, IncludePatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Files != 2 || result.Summary.BinaryFiles != 1 {
		t.Fatalf("result=%+v", result)
	}
	statuses := map[string]string{}
	for _, file := range result.Files {
		statuses[file.Path] = file.Status
	}
	if statuses["delete.txt"] != "deleted" || statuses["binary.bin"] != "modified" {
		t.Fatalf("statuses=%#v", statuses)
	}
}

func TestPatchBudgetOmitsOversizedPatch(t *testing.T) {
	repo := initRepo(t)
	manager := New(config.DiffConfig{MaxPatchBytes: 32})
	owner := Owner{Key: strings.Repeat("d", 64), Persistent: true}
	if err := manager.Ensure(repo, owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte(strings.Repeat("x", 500)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Show(repo, owner, Request{Since: BaselineProjectOpen, Advance: false, IncludePatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.PatchIncluded || !strings.Contains(result.PatchOmittedReason, "maxPatchBytes") {
		t.Fatalf("result=%+v", result)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runTestGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "tracked.txt")
	runTestGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "init")
	return repo
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
