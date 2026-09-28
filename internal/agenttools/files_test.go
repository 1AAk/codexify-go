package agenttools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/benice2me11/codexify-go/internal/workspace"
)

func TestFilesRoundTripAndSearch(t *testing.T) {
	dir := t.TempDir()
	root, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &Files{Root: root}
	if _, err := f.WriteFile(WriteFileInput{Path: "src/a.go", Content: "package a\n// needle\n"}); err != nil {
		t.Fatal(err)
	}
	read, err := f.ReadFile(ReadFileInput{Path: "src/a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if read.Content == "" {
		t.Fatal("empty read")
	}
	glob, err := f.Glob(GlobInput{Pattern: "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(glob.Matches) != 1 || glob.Matches[0] != "src/a.go" {
		t.Fatalf("glob = %#v", glob.Matches)
	}
	grep, err := f.Grep(GrepInput{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(grep.Matches) != 1 || grep.Matches[0].Line != 2 {
		t.Fatalf("grep = %#v", grep.Matches)
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "a.go")); err != nil {
		t.Fatal(err)
	}
}
