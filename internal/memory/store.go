package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/benice2me11/codexify-go/internal/config"
)

const fileName = "memory.json"

type Note struct {
	Value     string `json:"value"`
	UpdatedAt string `json:"updated_at"`
}

type State struct {
	WorkDir string          `json:"workDir"`
	Notes   map[string]Note `json:"notes"`
}

type Store struct {
	cfg          config.MemoryConfig
	multiProject bool
	mu           sync.Mutex
}

func New(cfg config.MemoryConfig, multiProject bool) *Store {
	return &Store{cfg: cfg, multiProject: multiProject}
}

func (s *Store) Enabled() bool { return s.cfg.Enabled }

func (s *Store) Recall(workDir string) (string, error) {
	if !s.cfg.Enabled {
		return "Persistent memory is disabled on this server (memory.enabled is false). Nothing is stored between conversations.", nil
	}
	state := s.load(workDir)
	if len(state.Notes) == 0 {
		return "Nothing remembered for this project yet. This is a fresh start, not a lost history.", nil
	}
	keys := make([]string, 0, len(state.Notes))
	for key := range state.Notes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("Project memory:\n")
	for _, key := range keys {
		note := state.Notes[key]
		fmt.Fprintf(&b, "- %s: %s", key, note.Value)
		if note.UpdatedAt != "" {
			fmt.Fprintf(&b, " (updated %s)", note.UpdatedAt)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (s *Store) Create(workDir, key, value string) (string, error) {
	return s.write(workDir, key, value, false)
}

func (s *Store) Update(workDir, key, value string) (string, error) {
	return s.write(workDir, key, value, true)
}

func (s *Store) Delete(workDir, key string) (string, error) {
	if !s.cfg.Enabled {
		return "", errors.New("persistent memory is disabled on this server (memory.enabled is false)")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("key must be a non-empty string")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.load(workDir)
	if _, ok := state.Notes[key]; !ok {
		return "", fmt.Errorf("no note named %q to remove", key)
	}
	delete(state.Notes, key)
	if err := s.save(workDir, state); err != nil {
		return "", err
	}
	return fmt.Sprintf("Removed note %q.", key), nil
}

func (s *Store) write(workDir, key, value string, update bool) (string, error) {
	if !s.cfg.Enabled {
		return "", errors.New("persistent memory is disabled on this server (memory.enabled is false)")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("key must be a non-empty string")
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("value must be a non-empty string")
	}
	if len(key) > 128 {
		return "", errors.New("key must be at most 128 bytes")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.load(workDir)
	existing, exists := state.Notes[key]
	if update {
		if !exists {
			return "", fmt.Errorf("no note named %q exists; use remember to create it", key)
		}
		if existing.Value == value {
			return fmt.Sprintf("Note %q already has that value.", key), nil
		}
	} else if exists {
		return "", fmt.Errorf("note %q already exists; use update_memory_note to replace it", key)
	}

	state.Notes[key] = Note{Value: value, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	encoded, err := json.Marshal(state.Notes)
	if err != nil {
		return "", err
	}
	if len(encoded) > s.cfg.MaxBytes {
		return "", fmt.Errorf("note rejected: notes would total %d bytes, over the %d-byte budget", len(encoded), s.cfg.MaxBytes)
	}
	if err := s.save(workDir, state); err != nil {
		return "", err
	}
	return fmt.Sprintf("Stored note %q (%d/%d bytes used).", key, len(encoded), s.cfg.MaxBytes), nil
}

func (s *Store) load(workDir string) State {
	state := State{WorkDir: workDir, Notes: map[string]Note{}}
	data, err := os.ReadFile(s.path(workDir))
	if err != nil {
		return state
	}
	var decoded State
	if json.Unmarshal(data, &decoded) != nil {
		return state
	}
	if decoded.Notes == nil {
		decoded.Notes = map[string]Note{}
	}
	if decoded.WorkDir == "" {
		decoded.WorkDir = workDir
	}
	return decoded
}

func (s *Store) save(workDir string, state State) error {
	target := s.path(workDir)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	state.WorkDir = workDir
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", target, time.Now().UnixNano())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Store) path(workDir string) string {
	return filepath.Join(s.dir(workDir), fileName)
}

func (s *Store) dir(workDir string) string {
	if s.cfg.Dir != "" {
		if s.multiProject {
			return filepath.Join(s.cfg.Dir, stateDirName(workDir))
		}
		return s.cfg.Dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".codexify-go", "projects", stateDirName(workDir))
}

func stateDirName(workDir string) string {
	clean := filepath.Clean(workDir)
	sum := sha256.Sum256([]byte(strings.ToLower(clean)))
	digest := hex.EncodeToString(sum[:6])
	base := filepath.Base(clean)
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "project"
	}
	return slug + "-" + digest
}
