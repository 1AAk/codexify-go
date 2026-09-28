package projects

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/config"
)

func TestConversationBindingPersistsWithoutRawSession(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	initGitRepo(t, project)
	state := filepath.Join(root, ".state")

	manager, err := New(config.MCPConfig{
		WorkspaceRoot:    root,
		MultiProject:     true,
		BindingsDir:      filepath.Join(state, "bindings"),
		ProjectScanDepth: 2,
		Worktrees: config.WorktreeConfig{
			Mode: "never",
			Root: filepath.Join(state, "worktrees"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	meta := map[string]any{"openai/session": "raw-secret-conversation-id"}
	selection, err := manager.Select(meta, "project-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.NewlySelected || selection.ManagedWorktree {
		t.Fatalf("unexpected selection: %+v", selection)
	}
	resolved, info, err := manager.Workspace(meta)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Path() != project || info.ProjectRoot != project {
		t.Fatalf("workspace mismatch: %s %+v", resolved.Path(), info)
	}

	identity := IdentityFromMeta(meta)
	data, err := os.ReadFile(manager.bindingPath(identity))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "raw-secret-conversation-id") {
		t.Fatal("raw conversation id leaked into persisted binding")
	}

	manager2, err := New(manager.cfg)
	if err != nil {
		t.Fatal(err)
	}
	resolved2, _, err := manager2.Workspace(meta)
	if err != nil {
		t.Fatal(err)
	}
	if resolved2.Path() != project {
		t.Fatalf("persisted binding resolved to %q", resolved2.Path())
	}
}

func TestBindingIsImmutable(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, filepath.Join(root, "one"))
	initGitRepo(t, filepath.Join(root, "two"))
	manager := testManager(t, root, "never")
	meta := map[string]any{"openai/session": "chat-one"}

	if _, err := manager.Select(meta, "one", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Select(meta, "two", nil); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("expected immutable binding error, got %v", err)
	}
}

func TestAutoModeCreatesWorktreeWhenProjectAlreadyInUse(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initGitRepo(t, source)
	manager := testManager(t, root, "auto")

	firstMeta := map[string]any{"openai/session": "chat-one"}
	first, err := manager.Select(firstMeta, "repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ManagedWorktree {
		t.Fatal("first binding should use source checkout in auto mode")
	}

	secondMeta := map[string]any{"openai/session": "chat-two"}
	second, err := manager.Select(secondMeta, "repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ManagedWorktree {
		t.Fatalf("second binding should be isolated: %+v", second)
	}
	if second.ProjectRoot == source || second.WorktreeGitRoot == "" {
		t.Fatalf("invalid managed worktree selection: %+v", second)
	}
	if _, err := os.Stat(filepath.Join(second.ProjectRoot, "README.md")); err != nil {
		t.Fatalf("managed worktree missing repository content: %v", err)
	}

	listed, err := manager.ListWorktrees(secondMeta)
	if err != nil {
		t.Fatal(err)
	}
	foundManaged := false
	for _, wt := range listed.Worktrees {
		if filepath.Clean(wt.Path) == filepath.Clean(second.WorktreeGitRoot) && wt.Managed {
			foundManaged = true
		}
	}
	if !foundManaged {
		t.Fatalf("managed worktree not listed: %+v", listed.Worktrees)
	}
}

func TestProjectCatalogFiltersAndSkipsNestedRepositoryContents(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, filepath.Join(root, "alpha"))
	beta := filepath.Join(root, "group", "beta")
	if err := os.MkdirAll(beta, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta, "go.mod"), []byte("module example/beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, root, "never")

	out, err := manager.List("beta", 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || len(out.Projects) != 1 || out.Projects[0].Selector != "group/beta" {
		t.Fatalf("unexpected catalog: %+v", out)
	}
}

func testManager(t *testing.T, root, mode string) *Manager {
	t.Helper()
	manager, err := New(config.MCPConfig{
		WorkspaceRoot:    root,
		MultiProject:     true,
		BindingsDir:      filepath.Join(root, ".state", "bindings"),
		ProjectScanDepth: 3,
		Worktrees: config.WorktreeConfig{
			Mode: mode,
			Root: filepath.Join(root, ".state", "worktrees"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func initGitRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "init")
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "add", "README.md")
	runGit(t, path, "-c", "user.name=Codexify Test", "-c", "user.email=test@example.invalid", "commit", "-m", "init")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
