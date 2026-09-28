package agenttickets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/benice2me11/codexify-go/internal/projects"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	OfflineAfter = 5 * time.Minute
	OutputField  = "new_codexify_ticket"
	InputField   = "codexify_ticket"

	Rejected     = "Ticket rejected; this call did not run. Another or duplicated agent may have claimed or advanced this conversation, or a response was lost. Stop this agent branch now, including chat_write/chat_await. Do not retry, guess, repeat setup, or retrieve tickets from logs or state. Inform the user of this warning without another tool call. Only the user may recover."
	Instructions = "## Agent tickets\n\nAgent tickets are enabled. Omit codexify_ticket until the first response supplies new_codexify_ticket. Then pass the latest new_codexify_ticket as codexify_ticket on every Codexify model-facing tool call, including setup, chat, polling, and discovery tools. Call tools serially and wait for each result; accepted tool-error responses also return a new ticket. A transport failure supplies no replacement: keep the previous ticket, and do not blindly repeat an operation that may already have run. A ticket rejection means another branch may have claimed or advanced the chain, or a response was lost. Inform the user without another tool call and stop this agent branch. After five minutes without a completed ticketed call and with no call in flight, the next user-requested call may reclaim the chain with no ticket or a stale ticket."
)

type Manager struct {
	dir      string
	mu       sync.Mutex
	memory   map[string]*memoryState
	policies sync.Map
}

type memoryState struct {
	mu           sync.Mutex
	current      string
	lastActivity time.Time
}

type Policy struct {
	Ticketed       bool
	InputEnvelope  bool
	OutputEnvelope bool
	OutputSchema   bool
}

type Permit struct {
	next       string
	acceptance string
	file       *os.File
	memory     *memoryState
	done       bool
}

type Failure struct {
	Reason  string
	Message string
}

func (f *Failure) Error() string { return f.Message }

func NewForTunnel(tunnelID string) (*Manager, error) {
	root := strings.TrimSpace(os.Getenv("CODEXIFY_GO_STATE_DIR"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return nil, errors.New("agent tickets require a user state directory")
		}
		root = filepath.Join(home, ".codexify-go")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte("codexify-go/agent-tickets/tunnel/v1\x00" + tunnelID))
	scope := hex.EncodeToString(sum[:16])
	return New(filepath.Join(root, "agent-tickets", scope)), nil
}

func New(directory string) *Manager {
	return &Manager{dir: directory, memory: map[string]*memoryState{}}
}

func (m *Manager) Reserve(identity *projects.Identity, supplied *string) (*Permit, error) {
	if identity == nil || strings.TrimSpace(identity.Key) == "" {
		return nil, &Failure{Reason: "state_unavailable", Message: "Ticket state unavailable; this call did not run. A stable conversation or stateful MCP transport session is required."}
	}
	if identity.Persistent {
		return m.reserveFile(identity.Key, supplied)
	}
	return m.reserveMemory(identity.Key, supplied)
}

func (m *Manager) reserveMemory(key string, supplied *string) (*Permit, error) {
	m.mu.Lock()
	state := m.memory[key]
	if state == nil {
		state = &memoryState{}
		m.memory[key] = state
	}
	m.mu.Unlock()
	if !state.mu.TryLock() {
		return nil, rejected("in_flight")
	}
	offline := state.current != "" && !state.lastActivity.IsZero() && time.Since(state.lastActivity) >= OfflineAfter
	next, acceptance, err := successor(optionalString(state.current), supplied, offline)
	if err != nil {
		state.mu.Unlock()
		return nil, err
	}
	return &Permit{next: next, acceptance: acceptance, memory: state}, nil
}

func (m *Manager) reserveFile(key string, supplied *string) (*Permit, error) {
	if err := privateDir(m.dir); err != nil {
		return nil, stateFailure(err)
	}
	path := filepath.Join(m.dir, key+".ticket")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, stateFailure("ticket state is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, stateFailure(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, stateFailure(err)
	}
	if err := tryLockFile(file); err != nil {
		file.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, rejected("in_flight")
		}
		return nil, stateFailure(err)
	}
	release := func() {
		_ = unlockFile(file)
		_ = file.Close()
	}
	opened, err := file.Stat()
	if err != nil {
		release()
		return nil, stateFailure(err)
	}
	currentInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(opened, currentInfo) {
		release()
		return nil, stateFailure("ticket state changed while opening")
	}
	data := make([]byte, 9)
	n, readErr := file.ReadAt(data, 0)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		release()
		return nil, stateFailure(readErr)
	}
	current := string(data[:n])
	if len(current) > 8 || (current != "" && !validTicket(current)) {
		release()
		return nil, stateFailure("invalid stored ticket")
	}
	offline := current != "" && time.Since(opened.ModTime()) >= OfflineAfter
	next, acceptance, err := successor(optionalString(current), supplied, offline)
	if err != nil {
		release()
		return nil, err
	}
	return &Permit{next: next, acceptance: acceptance, file: file}, nil
}

func (p *Permit) Acceptance() string { return p.acceptance }

func (p *Permit) Commit(ctx context.Context) (string, error) {
	if p == nil || p.done {
		return "", errors.New("ticket permit is unavailable")
	}
	if ctx.Err() != nil {
		p.Release()
		return "", errors.New("Call interrupted before ticket handoff. The original codexify_ticket is unchanged. Work may already have run; verify its outcome before retrying.")
	}
	if p.file != nil {
		if _, err := p.file.Seek(0, 0); err != nil {
			p.Release()
			return "", handoffFailure(err)
		}
		if err := p.file.Truncate(0); err != nil {
			p.Release()
			return "", handoffFailure(err)
		}
		if _, err := p.file.WriteString(p.next); err != nil {
			p.Release()
			return "", handoffFailure(err)
		}
		if err := p.file.Sync(); err != nil {
			p.Release()
			return "", handoffFailure(err)
		}
		_ = unlockFile(p.file)
		_ = p.file.Close()
		p.done = true
		return p.next, nil
	}
	if p.memory != nil {
		p.memory.current = p.next
		p.memory.lastActivity = time.Now()
		p.memory.mu.Unlock()
		p.done = true
		return p.next, nil
	}
	p.done = true
	return "", errors.New("ticket permit has no backing state")
}

func (p *Permit) Release() {
	if p == nil || p.done {
		return
	}
	if p.file != nil {
		_ = unlockFile(p.file)
		_ = p.file.Close()
	}
	if p.memory != nil {
		p.memory.mu.Unlock()
	}
	p.done = true
}

func successor(current, supplied *string, offline bool) (string, string, error) {
	acceptance := ""
	switch {
	case current == nil && supplied == nil:
		acceptance = "initial"
	case current != nil && supplied != nil && *current == *supplied:
		acceptance = "matched"
	case current != nil && supplied == nil && offline:
		acceptance = "reclaimed_missing"
	case current != nil && supplied != nil && offline:
		acceptance = "reclaimed_stale"
	case current != nil && supplied == nil:
		return "", "", rejected("missing")
	default:
		return "", "", rejected("mismatch")
	}
	for {
		var raw [6]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", "", stateFailure(err)
		}
		next := base64.RawURLEncoding.EncodeToString(raw[:])
		if current == nil || next != *current {
			return next, acceptance, nil
		}
	}
}

func rejected(reason string) error {
	return &Failure{Reason: reason, Message: Rejected}
}

func stateFailure(err any) error {
	return &Failure{Reason: "state_unavailable", Message: fmt.Sprintf("Ticket state unavailable; this call did not run. Ask the user to resolve it: %v", err)}
}

func handoffFailure(err error) error {
	return fmt.Errorf("Ticket handoff failed after dispatch; work may have run. Do not retry blindly: %w", err)
}

func validTicket(value string) bool {
	if len(value) != 8 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	copyValue := value
	return &copyValue
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
		return errors.New("ticket state directory must be a real directory")
	}
	return nil
}

func (m *Manager) SetPolicy(name string, policy Policy) {
	m.policies.Store(name, policy)
}

func (m *Manager) Policy(name string) (Policy, bool) {
	value, ok := m.policies.Load(name)
	if !ok {
		return Policy{}, false
	}
	policy, ok := value.(Policy)
	return policy, ok
}

func AugmentTool(tool *mcp.Tool, ticketed bool) (*mcp.Tool, Policy, error) {
	if tool == nil {
		return nil, Policy{}, errors.New("nil tool")
	}
	copyTool := *tool
	policy := Policy{Ticketed: ticketed}
	if !ticketed {
		return &copyTool, policy, nil
	}
	input, envelope, err := augmentInputSchema(tool.InputSchema)
	if err != nil {
		return nil, Policy{}, err
	}
	output, outputEnvelope, outputSchema, err := augmentOutputSchema(tool.OutputSchema)
	if err != nil {
		return nil, Policy{}, err
	}
	copyTool.InputSchema = input
	copyTool.OutputSchema = output
	policy.InputEnvelope = envelope
	policy.OutputEnvelope = outputEnvelope
	policy.OutputSchema = outputSchema
	return &copyTool, policy, nil
}

func ExtractArguments(raw json.RawMessage, policy Policy) (json.RawMessage, *string, error) {
	object, supplied, err := TakeTicket(raw)
	if err != nil {
		return nil, nil, err
	}
	clean, err := CleanArguments(object, policy)
	return clean, supplied, err
}

func TakeTicket(raw json.RawMessage) (map[string]json.RawMessage, *string, error) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, nil, errors.New("Ticket rejected; tool arguments must be a JSON object.")
	}
	var supplied *string
	if ticketRaw, exists := object[InputField]; exists {
		var value string
		if err := json.Unmarshal(ticketRaw, &value); err != nil {
			return nil, nil, errors.New(Rejected)
		}
		supplied = &value
		delete(object, InputField)
	}
	return object, supplied, nil
}

func CleanArguments(object map[string]json.RawMessage, policy Policy) (json.RawMessage, error) {
	if policy.InputEnvelope {
		if len(object) != 1 {
			return nil, errors.New("Pass original tool arguments inside the advertised arguments object.")
		}
		inner, ok := object["arguments"]
		if !ok {
			return nil, errors.New("Pass original tool arguments inside the advertised arguments object.")
		}
		var check map[string]json.RawMessage
		if json.Unmarshal(inner, &check) != nil {
			return nil, errors.New("Pass original tool arguments inside the advertised arguments object.")
		}
		return inner, nil
	}
	clean, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return clean, nil
}

func AttachTicket(result *mcp.CallToolResult, ticket string, policy Policy) {
	if result == nil || ticket == "" {
		return
	}
	ticketText := "new_codexify_ticket: " + ticket + "\nPass this as codexify_ticket on the next tool call, even after an error."
	result.Content = append(result.Content, &mcp.TextContent{Text: ticketText})
	if policy.OutputEnvelope {
		if result.StructuredContent == nil {
			result.StructuredContent = map[string]any{OutputField: ticket}
		} else {
			result.StructuredContent = map[string]any{"result": result.StructuredContent, OutputField: ticket}
		}
		return
	}
	if object, ok := toObject(result.StructuredContent); ok {
		object[OutputField] = ticket
		result.StructuredContent = object
		return
	}
	if result.StructuredContent == nil {
		result.StructuredContent = map[string]any{OutputField: ticket}
		return
	}
	result.StructuredContent = map[string]any{"result": result.StructuredContent, OutputField: ticket}
}

func RejectionResult(message string) *mcp.CallToolResult {
	if strings.TrimSpace(message) == "" {
		message = Rejected
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: message}},
	}
}

func augmentInputSchema(schema any) (any, bool, error) {
	original, err := schemaObject(schema)
	if err != nil {
		return nil, false, err
	}
	ticket := map[string]any{
		"type":        "string",
		"description": "Latest new_codexify_ticket; omit only for the first agent call.",
	}
	if needsEnvelope(original) {
		original = cloneObject(original)
		if _, exists := original["$id"]; !exists {
			original["$id"] = "urn:codexify-go:ticket-arguments"
		}
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				InputField:  ticket,
				"arguments": original,
			},
			"required":             []string{"arguments"},
			"additionalProperties": false,
		}, true, nil
	}
	out := cloneObject(original)
	props, _ := out["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props[InputField] = ticket
	out["properties"] = props
	return out, false, nil
}

func augmentOutputSchema(schema any) (any, bool, bool, error) {
	ticket := map[string]any{
		"type":        "string",
		"description": "Ticket to pass as codexify_ticket on the next model-facing tool call.",
	}
	if schema == nil {
		return map[string]any{
			"type":                 "object",
			"properties":           map[string]any{OutputField: ticket},
			"required":             []string{OutputField},
			"additionalProperties": true,
		}, false, false, nil
	}
	original, err := schemaObjectAny(schema)
	if err != nil {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				OutputField: ticket,
				"result":    schema,
			},
			"required":             []string{OutputField, "result"},
			"additionalProperties": false,
		}, true, true, nil
	}
	if needsOutputEnvelope(original) {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				OutputField: ticket,
				"result":    original,
			},
			"required":             []string{OutputField, "result"},
			"additionalProperties": false,
		}, true, true, nil
	}
	out := cloneObject(original)
	props, _ := out["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props[OutputField] = ticket
	out["properties"] = props
	required := toStringSlice(out["required"])
	if !containsString(required, OutputField) {
		required = append(required, OutputField)
	}
	out["required"] = required
	return out, false, true, nil
}

func needsEnvelope(schema map[string]any) bool {
	if schema["additionalProperties"] != false {
		return true
	}
	props, _ := schema["properties"].(map[string]any)
	if _, collision := props[InputField]; collision {
		return true
	}
	if containsLocalReference(schema) {
		return true
	}
	for key := range schema {
		switch key {
		case "$schema", "$id", "$defs", "definitions", "$comment", "title", "description", "type", "properties", "required", "additionalProperties":
		default:
			return true
		}
	}
	return false
}

func needsOutputEnvelope(schema map[string]any) bool {
	if schema["type"] != "object" || schema["additionalProperties"] != false || containsLocalReference(schema) {
		return true
	}
	props, _ := schema["properties"].(map[string]any)
	_, collision := props[OutputField]
	return collision
}

func containsLocalReference(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$ref" || key == "$dynamicRef" || key == "$recursiveRef" {
				if ref, ok := child.(string); ok && strings.HasPrefix(ref, "#") {
					return true
				}
			}
			if containsLocalReference(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsLocalReference(child) {
				return true
			}
		}
	}
	return false
}

func schemaObject(schema any) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}, nil
	}
	object, err := schemaObjectAny(schema)
	if err != nil {
		return nil, err
	}
	if object["type"] != "object" {
		return nil, errors.New("ticketed tool input schema must be an object")
	}
	return object, nil
}

func schemaObjectAny(schema any) (map[string]any, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("schema is not a JSON object")
	}
	return object, nil
}

func cloneObject(in map[string]any) map[string]any {
	data, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func toObject(value any) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var object map[string]any
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

func toStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		var out []string
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
