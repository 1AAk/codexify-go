package markdownchat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/projects"
	"github.com/benice2me11/codexify-go/internal/workspace"
)

const (
	header         = "# Codexify Chat\n\nAppend user messages at the bottom and save the file. Do not change earlier content\nwhile the agent is active. The agent sends messages only through chat_write.\n"
	agentStart     = "\n\n<!-- codexify-agent-message:v1:start id=\""
	userStart      = "\n\n<!-- codexify-user-message:v1:start id=\""
	warningStart   = "\n\n<!-- codexify-warning-message:v1:start id=\""
	anchorBytes    = 64
	maxUnreadBytes = 16 * 1024 * 1024
)

var messageCounter atomic.Uint64

type Store struct {
	cfg      config.AgentChatConfig
	mu       sync.Mutex
	channels map[string]*channel
}

type channel struct {
	path       string
	cursorPath string
	persistent bool
	mu         sync.Mutex
	cursor     *cursor
}

type cursor struct {
	Offset int64  `json:"offset"`
	Anchor []byte `json:"anchor"`
}

type Result struct {
	Status                 string `json:"status"`
	Content                string `json:"content"`
	ChatFile               string `json:"chat_file,omitempty"`
	Notification           string `json:"notification"`
	RequiredNextAction     string `json:"required_next_action"`
	AssistantTurnMayEnd    bool   `json:"assistant_turn_may_end"`
	NewChatMessageFromUser string `json:"new_chat_message_from_user,omitempty"`
}

func New(cfg config.AgentChatConfig) *Store {
	return &Store{cfg: cfg, channels: map[string]*channel{}}
}

func (s *Store) Enabled() bool { return s.cfg.Enabled }

func (s *Store) Path(workspace string, identity *projects.Identity) (string, error) {
	if !s.cfg.Enabled {
		return "", errors.New("Markdown chat is disabled")
	}
	if identity == nil || strings.TrimSpace(identity.Key) == "" {
		return "", errors.New("Markdown chat requires a conversation or MCP transport-session identity")
	}
	if strings.TrimSpace(workspace) == "" {
		return "", errors.New("Markdown chat requires an active workspace")
	}
	base, err := filepath.Abs(s.cfg.Dir)
	if err != nil {
		return "", err
	}
	workspaceSum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(workspace))))
	workspaceKey := hex.EncodeToString(workspaceSum[:12])
	return filepath.Join(base, workspaceKey, identity.Key, "CHAT.md"), nil
}

func (s *Store) Ensure(workspace string, identity *projects.Identity) (string, error) {
	ch, err := s.get(workspace, identity)
	if err != nil {
		return "", err
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if err := ch.ensureLocked(); err != nil {
		return "", err
	}
	return ch.path, nil
}

func (s *Store) Read(workspace string, identity *projects.Identity, consume bool) (Result, error) {
	ch, err := s.get(workspace, identity)
	if err != nil {
		return Result{}, err
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if err := ch.ensureLocked(); err != nil {
		return Result{}, err
	}
	text, end, err := ch.snapshotLocked()
	if err != nil {
		return Result{}, err
	}
	if consume && end != ch.cursor.Offset {
		if err := ch.advanceLocked(end); err != nil {
			return Result{}, err
		}
	}
	if strings.TrimSpace(text) == "" {
		return baseResult("empty", "The user has not written in CHAT.md since the last chat operation. Continue useful work if any remains; otherwise call chat_await.", ch.path, "continue_or_chat_await"), nil
	}
	out := baseResult("message", "New user text is included in new_chat_message_from_user. Answer or acknowledge it ASAP using chat_write.", ch.path, "chat_write")
	out.NewChatMessageFromUser = text
	return out, nil
}

func (s *Store) Write(workspace string, identity *projects.Identity, message string) (Result, error) {
	if strings.TrimSpace(message) == "" {
		return Result{}, errors.New("chat_write.message must not be empty")
	}
	if len(message) > maxUnreadBytes {
		return Result{}, errors.New("agent message exceeds the 16 MiB safety ceiling")
	}
	ch, err := s.get(workspace, identity)
	if err != nil {
		return Result{}, err
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if err := ch.ensureLocked(); err != nil {
		return Result{}, err
	}
	before, beforeEnd, err := ch.snapshotLocked()
	if err != nil {
		return Result{}, err
	}

	file, err := openAppendRegular(ch.path)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	id := fmt.Sprintf("%d-%d", time.Now().UTC().UnixMicro(), messageCounter.Add(1))
	block := fmt.Sprintf("%s%s\" created_at_ms=\"%d\" -->\n\n## Agent\n\n%s\n\n<!-- codexify-agent-message:v1:end id=\"%s\" -->\n", agentStart, id, time.Now().UTC().UnixMilli(), message, id)
	if _, err := file.WriteString(block); err != nil {
		return Result{}, err
	}
	end, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return Result{}, err
	}
	if err := file.Sync(); err != nil {
		return Result{}, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return Result{}, err
	}
	currentInfo, err := os.Stat(ch.path)
	if err != nil || !os.SameFile(openedInfo, currentInfo) {
		return Result{}, errors.New("CHAT.md was replaced during chat_write; the cursor was not advanced")
	}
	start := end - int64(len(block))
	if start < beforeEnd {
		return Result{}, errors.New("CHAT.md changed during the agent append; no user messages were consumed")
	}
	if err := ch.checkLocked(); err != nil {
		return Result{}, err
	}
	gap, err := readRange(file, beforeEnd, start, maxUnreadBytes-len(before))
	if err != nil {
		return Result{}, err
	}
	gapText, err := userText(string(gap))
	if err != nil {
		return Result{}, err
	}
	pending := before + gapText
	if err := ch.advanceLocked(end); err != nil {
		return Result{}, err
	}

	out := baseResult("written", "Message appended to CHAT.md. Notifications are not configured in codexify-go.", ch.path, "continue_or_chat_await")
	if strings.TrimSpace(pending) != "" {
		out.RequiredNextAction = "chat_write"
		out.NewChatMessageFromUser = pending
	}
	return out, nil
}

func (s *Store) Await(ctx context.Context, workspace func() (string, *projects.Identity, bool, error)) (Result, error) {
	wait := time.Duration(s.cfg.MaxWaitMS) * time.Millisecond
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	initialWorkspace, identity, selected, err := workspace()
	if err != nil {
		return Result{}, err
	}
	if selected {
		result, err := s.Read(initialWorkspace, identity, true)
		if err != nil {
			return Result{}, err
		}
		if result.Status == "message" {
			return result, nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			if selected {
				path, _ := s.Path(initialWorkspace, identity)
				return baseResult("cancelled", "Markdown chat operation was cancelled. Call chat_await again when appropriate.", path, "chat_await"), nil
			}
			return baseResult("cancelled", "Markdown chat operation was cancelled while no workspace was selected.", "", "chat_await"), nil
		case <-deadline.C:
			if selected {
				path, _ := s.Path(initialWorkspace, identity)
				return baseResult("timeout", "No new user text arrived before the Markdown chat wait deadline. Call chat_await again.", path, "chat_await"), nil
			}
			return baseResult("timeout", "No workspace was selected before the wait deadline. Keep workspace selection open and call chat_await again.", "", "chat_await"), nil
		case <-ticker.C:
			currentWorkspace, currentIdentity, currentSelected, err := workspace()
			if err != nil {
				return Result{}, err
			}
			if !selected && currentSelected {
				path, _ := s.Ensure(currentWorkspace, currentIdentity)
				return baseResult("workspace_selected", "The user selected a workspace. Call get_agent_brief now before project work.", path, "get_agent_brief"), nil
			}
			if selected && currentSelected && filepath.Clean(currentWorkspace) != filepath.Clean(initialWorkspace) {
				path, _ := s.Ensure(currentWorkspace, currentIdentity)
				return baseResult("workspace_selected", "The active workspace changed. Call get_agent_brief now before project work.", path, "get_agent_brief"), nil
			}
			if currentSelected {
				result, readErr := s.Read(currentWorkspace, currentIdentity, true)
				if readErr != nil {
					return Result{}, readErr
				}
				if result.Status == "message" {
					return result, nil
				}
			}
		}
	}
}

func (s *Store) get(workspace string, identity *projects.Identity) (*channel, error) {
	path, err := s.Path(workspace, identity)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.channels[path]; existing != nil {
		return existing, nil
	}
	ch := &channel{path: path, persistent: identity.Persistent}
	if identity.Persistent {
		ch.cursorPath = filepath.Join(filepath.Dir(path), "cursor.json")
	}
	s.channels[path] = ch
	return ch, nil
}

func (c *channel) ensureLocked() error {
	if err := privateDir(filepath.Dir(c.path)); err != nil {
		return err
	}
	info, err := os.Lstat(c.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.WriteFile(c.path, []byte(header), 0o600); err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0:
		return errors.New("Markdown chat file must be a regular file, not a symlink or device")
	}
	if c.cursor != nil {
		return nil
	}
	if c.persistent && c.cursorPath != "" {
		if data, err := os.ReadFile(c.cursorPath); err == nil {
			if len(data) > 4096 {
				return errors.New("Markdown chat cursor is oversized")
			}
			var persisted cursor
			if json.Unmarshal(data, &persisted) != nil || len(persisted.Anchor) > anchorBytes || int64(len(persisted.Anchor)) > persisted.Offset {
				return errors.New("Markdown chat cursor is invalid")
			}
			c.cursor = &persisted
			return c.checkLocked()
		}
	}
	file, err := os.Open(c.path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return err
	}
	start := int64(0)
	prefix := make([]byte, minInt64(info.Size(), int64(len(header))))
	n, _ := file.ReadAt(prefix, 0)
	if string(prefix[:n]) == header {
		start = int64(len(header))
	}
	c.cursor = &cursor{}
	return c.advanceLocked(start)
}

func (c *channel) snapshotLocked() (string, int64, error) {
	if err := c.checkLocked(); err != nil {
		return "", 0, err
	}
	file, err := os.Open(c.path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	end := info.Size()
	length := end - c.cursor.Offset
	if length < 0 {
		return "", 0, errors.New("CHAT.md was truncated; restore its earlier content")
	}
	if length > maxUnreadBytes {
		return "", 0, errors.New("unread CHAT.md text exceeds the 16 MiB safety ceiling; nothing was consumed")
	}
	data := make([]byte, length)
	if length > 0 {
		if _, err := file.ReadAt(data, c.cursor.Offset); err != nil && !errors.Is(err, io.EOF) {
			return "", 0, err
		}
	}
	if !utf8Valid(data) {
		return "", 0, errors.New("CHAT.md contains incomplete UTF-8; finish saving and retry")
	}
	text, err := userText(string(data))
	return text, end, err
}

func (c *channel) checkLocked() error {
	if c.cursor == nil {
		return errors.New("Markdown chat cursor is unavailable")
	}
	file, err := os.Open(c.path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < c.cursor.Offset {
		return errors.New("CHAT.md is no longer append-only at the read cursor")
	}
	if len(c.cursor.Anchor) == 0 {
		return nil
	}
	start := c.cursor.Offset - int64(len(c.cursor.Anchor))
	actual := make([]byte, len(c.cursor.Anchor))
	if _, err := file.ReadAt(actual, start); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !bytes.Equal(actual, c.cursor.Anchor) {
		return errors.New("CHAT.md is no longer append-only at the read cursor; restore earlier content before continuing")
	}
	return nil
}

func (c *channel) advanceLocked(end int64) error {
	if end < 0 {
		return errors.New("invalid Markdown chat cursor offset")
	}
	file, err := os.Open(c.path)
	if err != nil {
		return err
	}
	defer file.Close()
	start := end - anchorBytes
	if start < 0 {
		start = 0
	}
	anchor := make([]byte, end-start)
	if len(anchor) > 0 {
		if _, err := file.ReadAt(anchor, start); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	next := &cursor{Offset: end, Anchor: anchor}
	if c.persistent && c.cursorPath != "" {
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := atomicWrite(c.cursorPath, data, 0o600); err != nil {
			return err
		}
	}
	c.cursor = next
	return nil
}

func userText(text string) (string, error) {
	var out strings.Builder
	remaining := text
	for {
		start, role, prefix := nextMarker(remaining)
		if start < 0 {
			break
		}
		out.WriteString(remaining[:start])
		marker := remaining[start+len(prefix):]
		heading := "Agent"
		if role == "user" {
			heading = "User"
		} else if role == "warning" {
			heading = "Warning"
		}
		opening := "\" -->\n\n## " + heading + "\n\n"
		fields, body, ok := strings.Cut(marker, opening)
		if !ok {
			return "", errors.New("CHAT.md has an incomplete message; finish saving before retrying")
		}
		id, _, ok := strings.Cut(fields, "\"")
		if !ok || strings.TrimSpace(id) == "" {
			return "", errors.New("CHAT.md has a malformed message marker")
		}
		ending := "\n\n<!-- codexify-" + role + "-message:v1:end id=\"" + id + "\" -->\n"
		message, tail, ok := strings.Cut(body, ending)
		if !ok {
			return "", errors.New("CHAT.md has an incomplete message; finish saving before retrying")
		}
		if role == "user" {
			out.WriteString(message)
			out.WriteString("\n\n")
		}
		remaining = tail
	}
	out.WriteString(remaining)
	return out.String(), nil
}

func nextMarker(text string) (int, string, string) {
	best := -1
	role := ""
	prefix := ""
	for _, candidate := range []struct{ role, prefix string }{
		{"agent", agentStart},
		{"user", userStart},
		{"warning", warningStart},
	} {
		if idx := strings.Index(text, candidate.prefix); idx >= 0 && (best < 0 || idx < best) {
			best, role, prefix = idx, candidate.role, candidate.prefix
		}
	}
	return best, role, prefix
}

func baseResult(status, content, path, next string) Result {
	return Result{
		Status:              status,
		Content:             content,
		ChatFile:            path,
		Notification:        "not_configured",
		RequiredNextAction:  next,
		AssistantTurnMayEnd: false,
	}
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Markdown chat directory must be a real directory, not a symlink")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	resolved, err := workspace.Canonical(abs)
	if err != nil {
		return err
	}
	// Canonical OS aliases such as macOS /var -> /private/var are accepted.
	// The directory itself is still rejected above when it is a symlink.
	_ = resolved
	return nil
}

func openAppendRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Markdown chat file must be a regular file")
	}
	return os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
}

func readRange(file *os.File, start, end int64, limit int) ([]byte, error) {
	if end < start {
		return nil, errors.New("CHAT.md was truncated; restore its earlier content")
	}
	length := end - start
	if length > int64(limit) {
		return nil, errors.New("unread CHAT.md text exceeds the 16 MiB safety ceiling; nothing was consumed")
	}
	data := make([]byte, length)
	if length == 0 {
		return data, nil
	}
	_, err := file.ReadAt(data, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func utf8Valid(data []byte) bool {
	return utf8.Valid(data)
}
