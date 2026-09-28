package projects

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/workspace"
)

type Manager struct {
	cfg    config.MCPConfig
	access *workspace.Root
	mu     sync.Mutex
}

type WorkspaceInfo struct {
	AccessRoot        string `json:"accessRoot"`
	SourceProjectRoot string `json:"sourceProjectRoot"`
	ProjectRoot       string `json:"projectRoot"`
	ManagedWorktree   bool   `json:"managedWorktree"`
	WorktreeGitRoot   string `json:"worktreeGitRoot,omitempty"`
	WorktreesRoot     string `json:"worktreesRoot,omitempty"`
	WorktreeMode      string `json:"worktreeMode"`
	NewlySelected     bool   `json:"newlySelected"`
	BindingScope      string `json:"bindingScope"`
}

func New(cfg config.MCPConfig) (*Manager, error) {
	access, err := workspace.New(cfg.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg, access: access}, nil
}

func (m *Manager) AccessRoot() *workspace.Root {
	return m.access
}

func (m *Manager) Workspace(meta map[string]any) (*workspace.Root, WorkspaceInfo, error) {
	if !m.cfg.MultiProject {
		return m.access, WorkspaceInfo{
			AccessRoot:        m.access.Path(),
			SourceProjectRoot: m.access.Path(),
			ProjectRoot:       m.access.Path(),
			WorktreeMode:      strings.ToLower(m.cfg.Worktrees.Mode),
			BindingScope:      "single_project",
		}, nil
	}
	identity := IdentityFromMeta(meta)
	if identity == nil {
		return nil, WorkspaceInfo{}, errors.New("no ChatGPT conversation identity in request metadata; call list_projects/set_project_root from a ChatGPT conversation or disable multiProject")
	}
	binding, err := m.readBinding(identity)
	if err != nil {
		return nil, WorkspaceInfo{}, err
	}
	if binding == nil {
		return nil, WorkspaceInfo{}, errors.New("no project selected for this conversation; call list_projects then set_project_root")
	}
	root, err := workspace.New(binding.ProjectRoot)
	if err != nil {
		return nil, WorkspaceInfo{}, err
	}
	return root, infoFromBinding(m, *binding, false), nil
}

func (m *Manager) Select(meta map[string]any, selector string, createWorktree *bool) (WorkspaceInfo, error) {
	identity := IdentityFromMeta(meta)
	if m.cfg.MultiProject && identity == nil {
		return WorkspaceInfo{}, errors.New("set_project_root requires _meta[openai/session] in multi-project mode")
	}
	if !m.cfg.MultiProject {
		return WorkspaceInfo{
			AccessRoot:        m.access.Path(),
			SourceProjectRoot: m.access.Path(),
			ProjectRoot:       m.access.Path(),
			WorktreeMode:      strings.ToLower(m.cfg.Worktrees.Mode),
			BindingScope:      "single_project",
		}, nil
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return WorkspaceInfo{}, errors.New("selector is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, err := m.readBinding(identity)
	if err != nil {
		return WorkspaceInfo{}, err
	}

	source, err := m.access.Resolve(filepath.FromSlash(selector), false)
	if err != nil {
		return WorkspaceInfo{}, fmt.Errorf("select project: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return WorkspaceInfo{}, errors.New("selected project must be an existing directory beneath the access root")
	}
	if existing != nil {
		if cleanComparable(existing.SourceProjectRoot) != cleanComparable(source) {
			return WorkspaceInfo{}, fmt.Errorf("this conversation is already bound to %s; project bindings are immutable", existing.SourceProjectRoot)
		}
		return infoFromBinding(m, *existing, false), nil
	}

	gitRoot, gitErr := gitTopLevel(source)
	hasGit := gitErr == nil
	mode := strings.ToLower(m.cfg.Worktrees.Mode)
	wantWorktree := false
	if createWorktree != nil {
		wantWorktree = *createWorktree
	} else {
		switch mode {
		case "always":
			wantWorktree = hasGit
		case "auto":
			wantWorktree = hasGit && m.sourceInUse(source, identity)
		}
	}
	if wantWorktree && !hasGit {
		return WorkspaceInfo{}, errors.New("managed worktree requested but selected project is not inside a Git repository")
	}

	activeRoot := source
	managed := false
	worktreeGitRoot := ""
	if wantWorktree {
		created, err := m.createManagedWorktree(identity, source, gitRoot)
		if err != nil {
			return WorkspaceInfo{}, err
		}
		activeRoot = created.ProjectRoot
		managed = true
		worktreeGitRoot = created.GitRoot
	}

	binding := Binding{
		Version:           bindingVersion,
		IdentityHash:      identity.Key,
		SourceProjectRoot: source,
		ProjectRoot:       activeRoot,
		ManagedWorktree:   managed,
		WorktreeGitRoot:   worktreeGitRoot,
		WorktreesRoot:     m.cfg.Worktrees.Root,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := m.writeBinding(binding); err != nil {
		if managed {
			_ = removeManagedWorktree(gitRoot, worktreeGitRoot)
		}
		return WorkspaceInfo{}, err
	}
	return infoFromBinding(m, binding, true), nil
}

func infoFromBinding(m *Manager, binding Binding, newly bool) WorkspaceInfo {
	return WorkspaceInfo{
		AccessRoot:        m.access.Path(),
		SourceProjectRoot: binding.SourceProjectRoot,
		ProjectRoot:       binding.ProjectRoot,
		ManagedWorktree:   binding.ManagedWorktree,
		WorktreeGitRoot:   binding.WorktreeGitRoot,
		WorktreesRoot:     binding.WorktreesRoot,
		WorktreeMode:      strings.ToLower(m.cfg.Worktrees.Mode),
		NewlySelected:     newly,
		BindingScope:      "chatgpt_conversation",
	}
}
