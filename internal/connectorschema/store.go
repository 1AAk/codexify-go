package connectorschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/benice2me11/codexify-go/internal/config"
)

const BaseVersion = "0.6.0-dev"

type Store struct {
	dir string
	mu  sync.Mutex
}

func Version(cfg config.Config) string {
	version := BaseVersion
	if cfg.MCP.MultiProject {
		version += "+workspace-v1"
	}
	if cfg.ArtifactIngress.Enabled {
		version += "+artifact-ingress-v1"
	}
	if cfg.MCP.Upstreams != nil {
		for _, upstream := range cfg.MCP.Upstreams {
			if strings.EqualFold(strings.TrimSpace(upstream.Mode), "gateway") {
				version += "+gateway-v1"
				break
			}
		}
	}
	if len(version) > 64 {
		sum := sha256.Sum256([]byte(version))
		version = BaseVersion + "+" + hex.EncodeToString(sum[:6])
	}
	return version
}

func NewForTunnel(tunnelID string) (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, errors.New("cannot resolve user home for connector schema state")
	}
	sum := sha256.Sum256([]byte("codexify-go/connector-schema/tunnel/v1\x00" + tunnelID))
	scope := hex.EncodeToString(sum[:16])
	return &Store{dir: filepath.Join(home, ".codexify-go", "connector-schemas", scope)}, nil
}

func (s *Store) RecordConnector(version string) error {
	return s.write("connector", version)
}

func (s *Store) ConnectorVersion() string {
	return s.read("connector")
}

func (s *Store) ConversationVersion(identityHash string) string {
	if strings.TrimSpace(identityHash) == "" {
		return ""
	}
	return s.read("conversation-" + identityHash)
}

func (s *Store) RememberConversationVersion(identityHash, version string) error {
	if strings.TrimSpace(identityHash) == "" || strings.TrimSpace(version) == "" {
		return nil
	}
	key := "conversation-" + identityHash
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.readUnlocked(key); existing != "" {
		return nil
	}
	return s.writeUnlocked(key, version)
}

func (s *Store) write(key, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeUnlocked(key, version)
}

func (s *Store) writeUnlocked(key, version string) error {
	if strings.TrimSpace(version) == "" || len(version) > 64 {
		return errors.New("connector schema version is empty or too long")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, key+".tmp")
	if err := os.WriteFile(tmp, []byte(version), 0o600); err != nil {
		return err
	}
	target := filepath.Join(s.dir, key)
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Store) read(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readUnlocked(key)
}

func (s *Store) readUnlocked(key string) string {
	data, err := os.ReadFile(filepath.Join(s.dir, key))
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(data))
	if value == "" || len(value) > 64 {
		return ""
	}
	return value
}
