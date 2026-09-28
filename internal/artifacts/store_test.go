package artifacts

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/workspace"
)

func TestDurableExportKeepsImmutableSnapshot(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "report.txt"), []byte("original report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.New(project)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(config.ArtifactEgressConfig{
		Enabled:              true,
		Dir:                  t.TempDir(),
		MaxFileBytes:         1024,
		SnapshotMaxFileBytes: 1024,
		MaxSnapshotBytes:     4096,
		FallbackToSource:     true,
		MaxReferences:        8,
		ReferenceTTL:         config.Duration(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	link, receipt, err := store.Export(root, "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Durable || receipt.Path != "report.txt" || receipt.ChatLink != link.URI {
		t.Fatalf("unexpected receipt: %+v link=%+v", receipt, link)
	}
	if strings.Contains(receipt.ChatLink, project) {
		t.Fatal("artifact URI leaked host path")
	}
	if err := os.WriteFile(filepath.Join(project, "report.txt"), []byte("changed later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := store.Read(link.URI)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || !bytes.Equal(read.Contents[0].Blob, []byte("original report\n")) {
		t.Fatalf("snapshot changed: %+v", read.Contents)
	}
}

func TestFallbackResourceReadsLatestSafeSource(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "large.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.New(project)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(config.ArtifactEgressConfig{
		Enabled:              true,
		Dir:                  t.TempDir(),
		MaxFileBytes:         1024,
		SnapshotMaxFileBytes: 0,
		MaxSnapshotBytes:     0,
		FallbackToSource:     true,
		MaxReferences:        8,
		ReferenceTTL:         config.Duration(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	link, receipt, err := store.Export(root, "large.txt")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Durable {
		t.Fatal("expected fallback-to-source reference")
	}
	if err := os.WriteFile(filepath.Join(project, "large.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := store.Read(link.URI)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || string(read.Contents[0].Blob) != "second\n" {
		t.Fatalf("fallback did not read latest source: %+v", read.Contents)
	}
}

func TestExportRejectsWorkspaceEscape(t *testing.T) {
	project := t.TempDir()
	root, err := workspace.New(project)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(config.ArtifactEgressConfig{
		Enabled:              true,
		Dir:                  t.TempDir(),
		MaxFileBytes:         1024,
		SnapshotMaxFileBytes: 1024,
		MaxSnapshotBytes:     4096,
		FallbackToSource:     true,
		MaxReferences:        8,
		ReferenceTTL:         config.Duration(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Export(root, "../outside.txt"); err == nil {
		t.Fatal("expected workspace escape to be rejected")
	}
}

func TestExportRejectsNonSnapshotWhenFallbackDisabled(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "report.txt"), []byte("report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.New(project)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(config.ArtifactEgressConfig{
		Enabled:              true,
		Dir:                  t.TempDir(),
		MaxFileBytes:         1024,
		SnapshotMaxFileBytes: 0,
		MaxSnapshotBytes:     0,
		FallbackToSource:     false,
		MaxReferences:        8,
		ReferenceTTL:         config.Duration(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Export(root, "report.txt"); err == nil {
		t.Fatal("expected export to fail when snapshot is unavailable and fallback is disabled")
	}
}
