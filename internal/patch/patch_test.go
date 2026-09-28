package patch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/workspace"
)

func TestApplyAddUpdateMoveDelete(t *testing.T) {
	dir := t.TempDir()
	root, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "move.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "delete.txt"), []byte("bye\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := "*** Begin Patch\n" +
		"*** Update File: a.txt\n" +
		"@@ two\n" +
		"-three\n" +
		"+THREE\n" +
		"*** Add File: new.txt\n" +
		"+hello\n" +
		"*** Update File: move.txt\n" +
		"*** Move to: moved.txt\n" +
		"@@\n" +
		"-hello\n" +
		"+moved\n" +
		"*** Delete File: delete.txt\n" +
		"*** End Patch"
	result, err := Apply(root, input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "M a.txt") || !strings.Contains(result, "R move.txt -> moved.txt") || !strings.Contains(result, "D delete.txt") {
		t.Fatalf("result=%q", result)
	}
	a, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(a) != "one\ntwo\nTHREE\n" {
		t.Fatalf("a=%q", a)
	}
	newFile, _ := os.ReadFile(filepath.Join(dir, "new.txt"))
	if string(newFile) != "hello\n" {
		t.Fatalf("new=%q", newFile)
	}
	moved, _ := os.ReadFile(filepath.Join(dir, "moved.txt"))
	if string(moved) != "moved\n" {
		t.Fatalf("moved=%q", moved)
	}
	if _, err := os.Stat(filepath.Join(dir, "move.txt")); !os.IsNotExist(err) {
		t.Fatalf("move.txt should be moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "delete.txt")); !os.IsNotExist(err) {
		t.Fatalf("delete.txt should be deleted: %v", err)
	}
}

func TestValidationHappensBeforeFirstWrite(t *testing.T) {
	dir := t.TempDir()
	root, _ := workspace.New(dir)
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := "*** Begin Patch\n" +
		"*** Update File: first.txt\n" +
		"-original\n" +
		"+changed\n" +
		"*** Update File: missing.txt\n" +
		"-x\n" +
		"+y\n" +
		"*** End Patch"
	if _, err := Apply(root, input); err == nil {
		t.Fatal("expected patch validation to fail")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "first.txt"))
	if string(data) != "original\n" {
		t.Fatalf("first file changed before validation completed: %q", data)
	}
}

func TestCRLFPreservedAndFuzzyPunctuationMatches(t *testing.T) {
	dir := t.TempDir()
	root, _ := workspace.New(dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("header\r\nsay “hello”\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := "*** Begin Patch\n*** Update File: a.txt\n-say \"hello\"\n+say \"updated\"\n*** End Patch"
	if _, err := Apply(root, input); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(data) != "header\r\nsay \"updated\"\r\n" {
		t.Fatalf("data=%q", data)
	}
}

func TestRejectsTraversalAndDuplicateSources(t *testing.T) {
	dir := t.TempDir()
	root, _ := workspace.New(dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	traversal := "*** Begin Patch\n*** Add File: ../escape.txt\n+bad\n*** End Patch"
	if _, err := Apply(root, traversal); err == nil {
		t.Fatal("expected traversal rejection")
	}
	duplicate := "*** Begin Patch\n*** Delete File: a.txt\n*** Update File: a.txt\n-x\n+y\n*** End Patch"
	if _, err := Apply(root, duplicate); err == nil {
		t.Fatal("expected duplicate source rejection")
	}
}

func TestEndOfFileAnchorsChunk(t *testing.T) {
	dir := t.TempDir()
	root, _ := workspace.New(dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("same\nmiddle\nsame\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := "*** Begin Patch\n*** Update File: a.txt\n-same\n+last\n*** End of File\n*** End Patch"
	if _, err := Apply(root, input); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(data) != "same\nmiddle\nlast\n" {
		t.Fatalf("data=%q", data)
	}
}
