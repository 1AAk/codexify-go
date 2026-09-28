package projects

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ManagedWorktree struct {
	ProjectRoot string
	GitRoot     string
	Branch      string
}

type WorktreeRow struct {
	Path    string `json:"path"`
	Head    string `json:"head,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Managed bool   `json:"managed"`
}

type WorktreeListOutput struct {
	SourceProjectRoot string        `json:"sourceProjectRoot"`
	Worktrees         []WorktreeRow `json:"worktrees"`
}

func gitTopLevel(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return filepath.Clean(strings.TrimSpace(string(out))), nil
}

func (m *Manager) createManagedWorktree(identity *Identity, selected, gitRoot string) (ManagedWorktree, error) {
	if identity == nil {
		return ManagedWorktree{}, errors.New("managed worktree requires conversation identity")
	}
	rel, err := filepath.Rel(gitRoot, selected)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ManagedWorktree{}, errors.New("selected project is not beneath its Git root")
	}
	suffix := randomSuffix()
	projectName := sanitizeName(filepath.Base(gitRoot))
	parent := filepath.Join(m.cfg.Worktrees.Root, projectName)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return ManagedWorktree{}, err
	}
	worktreeGitRoot := filepath.Join(parent, identity.Short()+"-"+suffix)
	branch := "codexify-go/" + identity.Short() + "-" + suffix

	cmd := exec.Command("git", "-C", gitRoot, "worktree", "add", "-b", branch, worktreeGitRoot, "HEAD")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return ManagedWorktree{}, fmt.Errorf("create managed worktree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	projectRoot := worktreeGitRoot
	if rel != "." {
		projectRoot = filepath.Join(worktreeGitRoot, rel)
	}
	if _, err := os.Stat(projectRoot); err != nil {
		_ = removeManagedWorktree(gitRoot, worktreeGitRoot)
		return ManagedWorktree{}, fmt.Errorf("project path missing in managed worktree: %w", err)
	}
	return ManagedWorktree{ProjectRoot: projectRoot, GitRoot: worktreeGitRoot, Branch: branch}, nil
}

func removeManagedWorktree(sourceGitRoot, worktreeGitRoot string) error {
	if worktreeGitRoot == "" {
		return nil
	}
	cmd := exec.Command("git", "-C", sourceGitRoot, "worktree", "remove", "--force", worktreeGitRoot)
	return cmd.Run()
}

func (m *Manager) ListWorktrees(meta map[string]any) (WorktreeListOutput, error) {
	root, info, err := m.Workspace(meta)
	if err != nil {
		return WorktreeListOutput{}, err
	}
	_ = root
	gitRoot, err := gitTopLevel(info.SourceProjectRoot)
	if err != nil {
		return WorktreeListOutput{}, errors.New("selected project is not a Git repository")
	}
	cmd := exec.Command("git", "-C", gitRoot, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return WorktreeListOutput{}, err
	}
	var rows []WorktreeRow
	var current WorktreeRow
	flush := func() {
		if current.Path == "" {
			return
		}
		if rel, err := filepath.Rel(m.cfg.Worktrees.Root, current.Path); err == nil && !strings.HasPrefix(rel, "..") {
			current.Managed = true
		}
		rows = append(rows, current)
		current = WorktreeRow{}
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			current.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			current.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	flush()
	return WorktreeListOutput{SourceProjectRoot: info.SourceProjectRoot, Worktrees: rows}, nil
}

func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return "fallback"
}

func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "project"
	}
	return out
}
