package connectorschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/config"
)

func TestVersionReflectsSchemaFeatures(t *testing.T) {
	cfg := config.Default()
	cfg.MCP.MultiProject = false
	cfg.ArtifactIngress.Enabled = false
	cfg.MCP.Upstreams = nil
	if got := Version(cfg); got != BaseVersion {
		t.Fatalf("version=%q", got)
	}

	cfg.MCP.MultiProject = true
	cfg.ArtifactIngress.Enabled = true
	cfg.MCP.Upstreams = []config.UpstreamMCPConfig{{Name: "gw", Mode: "gateway"}}
	got := Version(cfg)
	for _, want := range []string{BaseVersion, "+workspace-v1", "+artifact-ingress-v1", "+gateway-v1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("version %q missing %q", got, want)
		}
	}
	if len(got) > 64 {
		t.Fatalf("version too long: %d", len(got))
	}
}

func TestStorePersistsConnectorAndFirstConversationVersion(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir}
	if err := store.RecordConnector("0.6.0-dev+workspace-v1"); err != nil {
		t.Fatal(err)
	}
	if err := store.RememberConversationVersion("abc", "0.5.0-dev"); err != nil {
		t.Fatal(err)
	}
	if err := store.RememberConversationVersion("abc", "0.6.0-dev"); err != nil {
		t.Fatal(err)
	}
	reloaded := &Store{dir: dir}
	if got := reloaded.ConnectorVersion(); got != "0.6.0-dev+workspace-v1" {
		t.Fatalf("connector=%q", got)
	}
	if got := reloaded.ConversationVersion("abc"); got != "0.5.0-dev" {
		t.Fatalf("conversation=%q", got)
	}
	info, err := os.Stat(filepath.Join(dir, "connector"))
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatal("connector record is directory")
	}
}
