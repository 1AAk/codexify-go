package projectdoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/config"
)

func TestLoadOrdersOuterToInnerAndPrefersOverride(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "services"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "services", "AGENTS.md"), []byte("ignored normal"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "services", "AGENTS.override.md"), []byte("service override"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "AGENTS.md"), []byte("api rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	doc := Load(nested, config.ProjectDocConfig{MaxBytes: 32768, RootMarkers: []string{".git"}})
	if len(doc.Entries) != 3 {
		t.Fatalf("entries=%#v", doc.Entries)
	}
	if got := []string{doc.Entries[0].Path, doc.Entries[1].Path, doc.Entries[2].Path}; strings.Join(got, ",") != "AGENTS.md,services/AGENTS.override.md,services/api/AGENTS.md" {
		t.Fatalf("paths=%#v", got)
	}
	if doc.Content != "root rules\n\nservice override\n\napi rules" {
		t.Fatalf("content=%q", doc.Content)
	}
}

func TestLoadUsesSharedByteBudget(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "AGENTS.md"), []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}

	doc := Load(nested, config.ProjectDocConfig{MaxBytes: 8, RootMarkers: []string{".git"}})
	if len(doc.Entries) != 2 || !doc.Entries[1].Truncated {
		t.Fatalf("doc=%#v", doc)
	}
	if doc.Content != "12345\n\nabc" {
		t.Fatalf("content=%q", doc.Content)
	}
}
