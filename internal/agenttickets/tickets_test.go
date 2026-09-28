package agenttickets

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benice2me11/codexify-go/internal/projects"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPersistentTicketChainAndCrossManagerLock(t *testing.T) {
	dir := t.TempDir()
	identity := &projects.Identity{Key: strings.Repeat("a", 64), Scope: "chatgpt_conversation", Persistent: true}
	first := New(dir)
	second := New(dir)

	permit, err := first.Reserve(identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Reserve(identity, nil); err == nil {
		t.Fatal("expected concurrent persistent reservation to be rejected")
	} else {
		var failure *Failure
		if !errors.As(err, &failure) || failure.Reason != "in_flight" {
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	ticket, err := permit.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !validTicket(ticket) {
		t.Fatalf("invalid ticket %q", ticket)
	}

	reopened := New(dir)
	permit2, err := reopened.Reserve(identity, &ticket)
	if err != nil {
		t.Fatal(err)
	}
	next, err := permit2.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next == ticket || !validTicket(next) {
		t.Fatalf("next=%q previous=%q", next, ticket)
	}

	if _, err := reopened.Reserve(identity, &ticket); err == nil {
		t.Fatal("stale ticket unexpectedly accepted")
	}
}

func TestOfflineReclaim(t *testing.T) {
	dir := t.TempDir()
	identity := &projects.Identity{Key: strings.Repeat("b", 64), Scope: "chatgpt_conversation", Persistent: true}
	manager := New(dir)
	permit, err := manager.Reserve(identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := permit.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, identity.Key+".ticket")
	old := time.Now().Add(-OfflineAfter - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	reclaim, err := manager.Reserve(identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reclaim.Acceptance() != "reclaimed_missing" {
		t.Fatalf("acceptance=%q", reclaim.Acceptance())
	}
	recovered, err := reclaim.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovered == ticket {
		t.Fatal("offline reclaim did not advance ticket")
	}
}

func TestTransientTicketsAreMemoryScoped(t *testing.T) {
	identity := &projects.Identity{Key: strings.Repeat("c", 64), Scope: "transport_session", Persistent: false}
	manager := New(t.TempDir())
	permit, err := manager.Reserve(identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := permit.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Reserve(identity, nil); err == nil {
		t.Fatal("missing transient ticket unexpectedly accepted")
	}
	fresh := New(t.TempDir())
	if _, err := fresh.Reserve(identity, nil); err != nil {
		t.Fatalf("fresh transient manager should have no ticket state: %v", err)
	}
	if ticket == "" {
		t.Fatal("empty ticket")
	}
}

func TestSchemaAugmentationAndArgumentEnvelope(t *testing.T) {
	simple := &mcp.Tool{
		Name: "simple",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"path": map[string]any{"type": "string"}},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
			"required":             []string{"ok"},
			"additionalProperties": false,
		},
	}
	augmented, policy, err := AugmentTool(simple, true)
	if err != nil {
		t.Fatal(err)
	}
	if policy.InputEnvelope || policy.OutputEnvelope {
		t.Fatalf("unexpected envelope: %+v", policy)
	}
	input := mustMap(t, augmented.InputSchema)
	props := mustMap(t, input["properties"])
	if _, ok := props[InputField]; !ok {
		t.Fatal("ticket field missing from simple schema")
	}
	output := mustMap(t, augmented.OutputSchema)
	outProps := mustMap(t, output["properties"])
	if _, ok := outProps[OutputField]; !ok {
		t.Fatal("successor field missing from output schema")
	}

	complex := &mcp.Tool{
		Name: "complex",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"value": map[string]any{"$ref": "#/$defs/value"}},
			"$defs":                map[string]any{"value": map[string]any{"type": "string"}},
			"additionalProperties": false,
		},
	}
	_, complexPolicy, err := AugmentTool(complex, true)
	if err != nil {
		t.Fatal(err)
	}
	if !complexPolicy.InputEnvelope {
		t.Fatal("local-ref schema should use arguments envelope")
	}
	raw := json.RawMessage(`{"codexify_ticket":"12345678","arguments":{"value":"x"}}`)
	clean, supplied, err := ExtractArguments(raw, complexPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if supplied == nil || *supplied != "12345678" || string(clean) != `{"value":"x"}` {
		t.Fatalf("clean=%s supplied=%v", clean, supplied)
	}
}

func TestAttachTicketOnToolError(t *testing.T) {
	result := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "expected error"}}}
	policy := Policy{Ticketed: true}
	AttachTicket(result, "AbCd12_-", policy)
	if !result.IsError {
		t.Fatal("tool error flag changed")
	}
	object := mustMap(t, result.StructuredContent)
	if object[OutputField] != "AbCd12_-" {
		t.Fatalf("structured=%#v", object)
	}
	found := false
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok && strings.Contains(text.Text, "AbCd12_-") {
			found = true
		}
	}
	if !found {
		t.Fatal("ticket not included in model-visible content")
	}
}

func mustMap(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
