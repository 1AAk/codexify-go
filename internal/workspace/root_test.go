package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("../escape", true); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestResolveMissingChild(t *testing.T) {
	root := t.TempDir()
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve(filepath.Join("a", "b", "file.txt"), true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "a", "b", "file.txt")
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires Windows Developer Mode or elevation")
	}
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(filepath.Join("link", "file.txt"), true); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}
