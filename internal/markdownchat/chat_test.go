package markdownchat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/projects"
)

func TestReadWriteCursorAndAppendOnlyGuard(t *testing.T) {
	store := New(config.AgentChatConfig{Enabled: true, Dir: t.TempDir(), MaxWaitMS: 1000})
	identity := &projects.Identity{Key: strings.Repeat("a", 64), Scope: "chatgpt_conversation", Persistent: true}
	workspace := t.TempDir()

	path, err := store.Ensure(workspace, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendFile(path, "hello from user\n"); err != nil {
		t.Fatal(err)
	}
	read, err := store.Read(workspace, identity, true)
	if err != nil {
		t.Fatal(err)
	}
	if read.Status != "message" || read.NewChatMessageFromUser != "hello from user\n" {
		t.Fatalf("read=%+v", read)
	}
	empty, err := store.Read(workspace, identity, true)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Status != "empty" {
		t.Fatalf("empty=%+v", empty)
	}

	written, err := store.Write(workspace, identity, "agent answer")
	if err != nil {
		t.Fatal(err)
	}
	if written.Status != "written" {
		t.Fatalf("written=%+v", written)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "## Agent\n\nagent answer") {
		t.Fatalf("chat=%q", data)
	}
	post, err := store.Read(workspace, identity, true)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != "empty" {
		t.Fatalf("agent block was reread as user text: %+v", post)
	}

	cursorPath := filepath.Join(filepath.Dir(path), "cursor.json")
	if _, err := os.Stat(cursorPath); err != nil {
		t.Fatalf("persistent cursor missing: %v", err)
	}
	if err := os.WriteFile(path, []byte("rewritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(workspace, identity, true); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only violation, got %v", err)
	}
}

func TestUserMarkerIsReturnedButAgentMarkerIsFiltered(t *testing.T) {
	id := "123"
	raw := "plain\n" +
		userStart + id + "\" created_at_ms=\"1\" -->\n\n## User\n\nwidget user\n\n<!-- codexify-user-message:v1:end id=\"" + id + "\" -->\n" +
		agentStart + "456\" created_at_ms=\"2\" -->\n\n## Agent\n\nhidden agent\n\n<!-- codexify-agent-message:v1:end id=\"456\" -->\n"
	got, err := userText(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "plain") || !strings.Contains(got, "widget user") || strings.Contains(got, "hidden agent") {
		t.Fatalf("userText=%q", got)
	}
}

func TestAwaitReturnsNewUserText(t *testing.T) {
	store := New(config.AgentChatConfig{Enabled: true, Dir: t.TempDir(), MaxWaitMS: 2000})
	identity := &projects.Identity{Key: strings.Repeat("b", 64), Scope: "chatgpt_conversation", Persistent: true}
	workspace := t.TempDir()
	path, err := store.Ensure(workspace, identity)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = appendFile(path, "async user reply\n")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := store.Await(ctx, func() (string, *projects.Identity, bool, error) {
		return workspace, identity, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "message" || !strings.Contains(result.NewChatMessageFromUser, "async user reply") {
		t.Fatalf("await=%+v", result)
	}
}

func TestAwaitCanObserveWorkspaceSelection(t *testing.T) {
	store := New(config.AgentChatConfig{Enabled: true, Dir: t.TempDir(), MaxWaitMS: 2000})
	identity := &projects.Identity{Key: strings.Repeat("c", 64), Scope: "chatgpt_conversation", Persistent: true}
	workspace := t.TempDir()
	var selected atomic.Bool

	go func() {
		time.Sleep(150 * time.Millisecond)
		selected.Store(true)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := store.Await(ctx, func() (string, *projects.Identity, bool, error) {
		return workspace, identity, selected.Load(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "workspace_selected" || result.RequiredNextAction != "get_agent_brief" {
		t.Fatalf("await=%+v", result)
	}
}

func TestTransientIdentityDoesNotPersistCursor(t *testing.T) {
	store := New(config.AgentChatConfig{Enabled: true, Dir: t.TempDir(), MaxWaitMS: 1000})
	identity := &projects.Identity{Key: strings.Repeat("d", 64), Scope: "transport_session", Persistent: false}
	workspace := t.TempDir()
	path, err := store.Ensure(workspace, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "cursor.json")); !os.IsNotExist(err) {
		t.Fatalf("transient chat should not persist cursor, err=%v", err)
	}
}

func appendFile(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteString(content); err != nil {
		return err
	}
	return file.Sync()
}
