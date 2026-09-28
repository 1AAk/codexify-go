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

const bindingVersion = 1

type Binding struct {
	Version           int    `json:"version"`
	IdentityHash      string `json:"identityHash"`
	SourceProjectRoot string `json:"sourceProjectRoot"`
	ProjectRoot       string `json:"projectRoot"`
	ManagedWorktree   bool   `json:"managedWorktree"`
	WorktreeGitRoot   string `json:"worktreeGitRoot,omitempty"`
	WorktreesRoot     string `json:"worktreesRoot,omitempty"`
	CreatedAt         string `json:"createdAt"`
}

func (m *Manager) readBinding(identity *Identity) (*Binding, error) {
	if identity == nil {
		return nil, nil
	}
	path := m.bindingPath(identity)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var binding Binding
	if err := json.Unmarshal(data, &binding); err != nil {
		return nil, fmt.Errorf("decode project binding: %w", err)
	}
	if binding.Version != bindingVersion || binding.IdentityHash != identity.Key {
		return nil, errors.New("project binding identity/version mismatch")
	}
	if _, err := os.Stat(binding.ProjectRoot); err != nil {
		return nil, fmt.Errorf("bound project is no longer available: %w", err)
	}
	return &binding, nil
}

func (m *Manager) writeBinding(binding Binding) error {
	if err := os.MkdirAll(m.cfg.BindingsDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(m.cfg.BindingsDir, binding.IdentityHash+".json")
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

func (m *Manager) bindingPath(identity *Identity) string {
	return filepath.Join(m.cfg.BindingsDir, identity.Key+".json")
}

func (m *Manager) sourceInUse(source string, identity *Identity) bool {
	entries, err := os.ReadDir(m.cfg.BindingsDir)
	if err != nil {
		return false
	}
	source = cleanComparable(source)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.cfg.BindingsDir, entry.Name()))
		if err != nil {
			continue
		}
		var binding Binding
		if json.Unmarshal(data, &binding) != nil {
			continue
		}
		if identity != nil && binding.IdentityHash == identity.Key {
			continue
		}
		if cleanComparable(binding.SourceProjectRoot) == source {
			if _, err := os.Stat(binding.ProjectRoot); err == nil {
				return true
			}
		}
	}
	return false
}

func cleanComparable(path string) string {
	return strings.ToLower(filepath.Clean(path))
}
