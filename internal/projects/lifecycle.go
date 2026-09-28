package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type WorkspaceChange struct {
	Revision          string `json:"revision"`
	PreviousRoot      string `json:"previousRoot"`
	AwaitingSelection bool   `json:"awaitingSelection"`
}

func (m *Manager) SelectScratch(meta map[string]any) (WorkspaceInfo, error) {
	if !m.cfg.MultiProject {
		return WorkspaceInfo{}, errors.New("scratch workspace selection requires multi-project mode")
	}
	identity := IdentityFromMeta(meta)
	if identity == nil {
		return WorkspaceInfo{}, errors.New("scratch workspace selection requires _meta[openai/session]")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, err := m.readBinding(identity)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	if existing != nil {
		if existing.Mode == "scratch" {
			return infoFromBinding(m, *existing, false), nil
		}
		return WorkspaceInfo{}, fmt.Errorf("this conversation is already bound to %s; use switch_project_root before selecting scratch", existing.ProjectRoot)
	}

	scratchBase, err := m.pathInsideAccess(m.cfg.ScratchDir)
	if err != nil {
		return WorkspaceInfo{}, fmt.Errorf("scratchDir: %w", err)
	}
	if err := os.MkdirAll(scratchBase, 0o700); err != nil {
		return WorkspaceInfo{}, err
	}
	name := identity.Short()
	if name == "" {
		return WorkspaceInfo{}, errors.New("invalid conversation identity")
	}
	root := filepath.Join(scratchBase, name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return WorkspaceInfo{}, err
	}

	binding := Binding{
		Version:      bindingVersion,
		IdentityHash: identity.Key,
		Mode:         "scratch",
		ProjectRoot:  root,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := m.writeBinding(binding); err != nil {
		return WorkspaceInfo{}, err
	}
	m.finishSwitch(identity)
	return infoFromBinding(m, binding, true), nil
}

func (m *Manager) Switch(meta map[string]any, expectedPath string) (WorkspaceChange, error) {
	if !m.cfg.MultiProject {
		return WorkspaceChange{}, errors.New("workspace switching requires multi-project mode")
	}
	identity := IdentityFromMeta(meta)
	if identity == nil {
		return WorkspaceChange{}, errors.New("workspace switching requires _meta[openai/session]")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	change, err := m.readSwitch(identity)
	if err != nil {
		return WorkspaceChange{}, err
	}
	if change != nil && change.AwaitingSelection {
		if expectedPath == "" || cleanComparable(change.PreviousRoot) == cleanComparable(expectedPath) {
			return *change, nil
		}
	}

	current, err := m.readBinding(identity)
	if err != nil {
		return WorkspaceChange{}, err
	}
	if current == nil {
		return WorkspaceChange{}, errors.New("no workspace is selected")
	}
	if expectedPath != "" && cleanComparable(current.ProjectRoot) != cleanComparable(expectedPath) {
		return WorkspaceChange{}, errors.New("workspace changed since the switch request was prepared")
	}

	revision := fmt.Sprintf("%d-%s", time.Now().UTC().UnixMicro(), randomSuffix())
	archiveDir := filepath.Join(m.cfg.BindingsDir, "previous")
	archivePath := filepath.Join(archiveDir, revision+"-"+identity.Key+".json")
	if err := writeBindingFile(archivePath, *current); err != nil {
		return WorkspaceChange{}, err
	}

	newChange := WorkspaceChange{
		Revision:          revision,
		PreviousRoot:      current.ProjectRoot,
		AwaitingSelection: true,
	}
	if err := m.writeSwitch(identity, newChange); err != nil {
		return WorkspaceChange{}, err
	}
	if err := os.Remove(m.bindingPath(identity)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return WorkspaceChange{}, err
	}
	return newChange, nil
}

func (m *Manager) Resume(meta map[string]any, resumePath string) (WorkspaceInfo, error) {
	if !m.cfg.MultiProject {
		return WorkspaceInfo{}, errors.New("workspace resumption requires multi-project mode")
	}
	identity := IdentityFromMeta(meta)
	if identity == nil {
		return WorkspaceInfo{}, errors.New("workspace resumption requires _meta[openai/session]")
	}
	if !filepath.IsAbs(resumePath) {
		return WorkspaceInfo{}, errors.New("resumePath must be an absolute active workspace path")
	}
	requested, err := filepath.Abs(resumePath)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	requested, err = filepath.EvalSymlinks(requested)
	if err != nil {
		return WorkspaceInfo{}, fmt.Errorf("cannot resume workspace: %w", err)
	}
	if _, err := m.pathInsideAccess(requested); err != nil {
		return WorkspaceInfo{}, errors.New("resumePath is outside the configured access root")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, err := m.readBinding(identity)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	if existing != nil {
		if cleanComparable(existing.ProjectRoot) != cleanComparable(requested) {
			return WorkspaceInfo{}, errors.New("this conversation already has a different workspace; resume in a new conversation or switch first")
		}
		return infoFromBinding(m, *existing, false), nil
	}

	saved, err := m.findSavedBindingByRoot(requested)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	if saved == nil {
		return WorkspaceInfo{}, errors.New("no valid saved workspace matches resumePath under this access root")
	}
	saved.Version = bindingVersion
	saved.IdentityHash = identity.Key
	saved.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := m.writeBinding(*saved); err != nil {
		return WorkspaceInfo{}, err
	}
	m.finishSwitch(identity)
	return infoFromBinding(m, *saved, true), nil
}

func (m *Manager) findSavedBindingByRoot(root string) (*Binding, error) {
	var best *Binding
	err := filepath.WalkDir(m.cfg.BindingsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") || strings.HasSuffix(strings.ToLower(entry.Name()), ".workspace-change.json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		binding, err := decodeBinding(data)
		if err != nil {
			return nil
		}
		if cleanComparable(binding.ProjectRoot) != cleanComparable(root) {
			return nil
		}
		if _, err := os.Stat(binding.ProjectRoot); err != nil {
			return nil
		}
		copyBinding := *binding
		if best == nil || (!best.ManagedWorktree && copyBinding.ManagedWorktree) {
			best = &copyBinding
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return best, err
}

func (m *Manager) switchPath(identity *Identity) string {
	return filepath.Join(m.cfg.BindingsDir, identity.Key+".workspace-change.json")
}

func (m *Manager) readSwitch(identity *Identity) (*WorkspaceChange, error) {
	data, err := os.ReadFile(m.switchPath(identity))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 16*1024 {
		return nil, errors.New("workspace-change record is oversized")
	}
	var change WorkspaceChange
	if err := json.Unmarshal(data, &change); err != nil {
		return nil, err
	}
	return &change, nil
}

func (m *Manager) writeSwitch(identity *Identity, change WorkspaceChange) error {
	data, err := json.MarshalIndent(change, "", "  ")
	if err != nil {
		return err
	}
	path := m.switchPath(identity)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp-" + fmt.Sprint(time.Now().UnixNano())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (m *Manager) finishSwitch(identity *Identity) {
	_ = os.Remove(m.switchPath(identity))
}
