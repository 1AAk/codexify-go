package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benice2me11/codexify-go/internal/artifacts"
	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testRuntime(t *testing.T, auth bool) *Runtime {
	t.Helper()
	cfg := config.Default()
	cfg.MCP.WorkspaceRoot = t.TempDir()
	cfg.MCP.AuthEnabled = auth
	cfg.Tunnel.MCPServerURL = "http://127.0.0.1:0/mcp"
	cfg.Tunnel.Executable = filepath.Join(t.TempDir(), "unused.exe")
	cfg.Tunnel.TunnelID = "tunnel_test"
	cfg.Tunnel.APIKeyRef = "env:TEST"
	r, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.listener.Close()
		r.exec.Close()
	})
	return r
}

func TestAuthMiddleware(t *testing.T) {
	r := testRuntime(t, true)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewBufferString("{}"))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without auth status=%d", rec.Code)
	}
	_, env := r.TunnelEnvironment()
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", env[InternalAuthEnv])
	rec = httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("generated tunnel authorization was rejected")
	}
}

func TestMCPInitializeListAndFileTools(t *testing.T) {
	r := testRuntime(t, false)

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	initResp := postJSON(t, r.Handler(), initBody)
	if initResp.Code != http.StatusOK {
		t.Fatalf("initialize status=%d body=%s", initResp.Code, initResp.Body.String())
	}
	sessionID := initResp.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("stateful initialize did not return Mcp-Session-Id")
	}
	initializedBody := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`
	_ = postJSON(t, r.Handler(), initializedBody, sessionID)

	listBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	listResp := postJSON(t, r.Handler(), listBody, sessionID)
	if listResp.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", listResp.Code, listResp.Body.String())
	}
	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listResp.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	foundRead := false
	foundExec := false
	for _, tool := range listed.Result.Tools {
		foundRead = foundRead || tool.Name == "read_file"
		foundExec = foundExec || tool.Name == "exec_command"
	}
	if !foundRead || !foundExec {
		t.Fatalf("expected tools missing: %+v", listed.Result.Tools)
	}

	writeBody := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write_file","arguments":{"path":"hello.txt","content":"hello\n"}}}`
	writeResp := postJSON(t, r.Handler(), writeBody, sessionID)
	if writeResp.Code != http.StatusOK {
		t.Fatalf("write_file status=%d body=%s", writeResp.Code, writeResp.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(r.root.Path(), "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("written data=%q", data)
	}

	readBody := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"hello.txt"}}}`
	readResp := postJSON(t, r.Handler(), readBody, sessionID)
	if readResp.Code != http.StatusOK {
		t.Fatalf("read_file status=%d body=%s", readResp.Code, readResp.Body.String())
	}
	if !bytes.Contains(readResp.Body.Bytes(), []byte("1\\thello")) {
		t.Fatalf("read response=%s", readResp.Body.String())
	}
}

func TestOfficialClientNegotiatesCurrentProtocol(t *testing.T) {
	r := testRuntime(t, false)
	httpServer := httptest.NewServer(r.Handler())
	defer httpServer.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "codexify-go-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/mcp",
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated protocol version = %q, want 2026-07-28", got)
	}

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) < 8 {
		t.Fatalf("unexpected tool count: %d", len(listed.Tools))
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "write_file",
		Arguments: map[string]any{"path": "sdk.txt", "content": "sdk works\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}
	data, err := os.ReadFile(filepath.Join(r.root.Path(), "sdk.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "sdk works\n" {
		t.Fatalf("written data=%q", data)
	}
}

func TestLegacyTransportSessionBindingIsTransient(t *testing.T) {
	accessRoot := t.TempDir()
	projectRoot := filepath.Join(accessRoot, "project-a")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module example/project-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "hello.txt"), []byte("transport bound\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.MCP.WorkspaceRoot = accessRoot
	cfg.MCP.MultiProject = true
	cfg.MCP.BindingsDir = filepath.Join(accessRoot, ".state", "bindings")
	cfg.MCP.Worktrees = config.WorktreeConfig{Mode: "never", Root: filepath.Join(accessRoot, ".state", "worktrees")}
	cfg.MCP.AuthEnabled = false
	cfg.Tunnel.MCPServerURL = "http://127.0.0.1:0/mcp"
	cfg.Tunnel.Executable = filepath.Join(t.TempDir(), "unused.exe")
	cfg.Tunnel.TunnelID = "tunnel_test"
	cfg.Tunnel.APIKeyRef = "env:TEST"
	r, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.listener.Close()
		r.exec.Close()
		if r.bridge != nil {
			r.bridge.Close()
		}
	})

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"legacy-test","version":"1"}}}`
	initResp := postJSON(t, r.Handler(), initialize)
	if initResp.Code != http.StatusOK {
		t.Fatalf("initialize: %d %s", initResp.Code, initResp.Body.String())
	}
	sessionID := initResp.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("missing stateful session id")
	}
	_ = postJSON(t, r.Handler(), `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`, sessionID)

	selected := postJSON(t, r.Handler(), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"set_project_root","arguments":{"path":"project-a"}}}`, sessionID)
	if selected.Code != http.StatusOK || !strings.Contains(selected.Body.String(), `"bindingScope":"transport_session"`) {
		t.Fatalf("transport selection failed: %d %s", selected.Code, selected.Body.String())
	}
	read := postJSON(t, r.Handler(), `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"hello.txt"}}}`, sessionID)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), "transport bound") {
		t.Fatalf("transport-bound read failed: %d %s", read.Code, read.Body.String())
	}
	if entries, err := os.ReadDir(cfg.MCP.BindingsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
				t.Fatalf("transient binding persisted to disk: %s", entry.Name())
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/mcp", nil)
	deleteReq.Header.Set("Mcp-Session-Id", sessionID)
	deleteReq.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	deleteRec := httptest.NewRecorder()
	r.Handler().ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code < 200 || deleteRec.Code >= 300 {
		t.Fatalf("session delete failed: %d %s", deleteRec.Code, deleteRec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := r.sessions.Load(sessionID); !ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := r.sessions.Load(sessionID); ok {
		t.Fatal("transport session cleanup watcher did not finish")
	}

	secondInit := postJSON(t, r.Handler(), initialize)
	secondID := secondInit.Header().Get("Mcp-Session-Id")
	if secondID == "" || secondID == sessionID {
		t.Fatalf("invalid second session id %q", secondID)
	}
	_ = postJSON(t, r.Handler(), `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`, secondID)
	unbound := postJSON(t, r.Handler(), `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"hello.txt"}}}`, secondID)
	if unbound.Code != http.StatusOK || !strings.Contains(unbound.Body.String(), "no project selected") {
		t.Fatalf("new transport session inherited old binding: %d %s", unbound.Code, unbound.Body.String())
	}
}

func TestMultiProjectConversationBindingOverMCP(t *testing.T) {
	accessRoot := t.TempDir()
	projectRoot := filepath.Join(accessRoot, "project-a")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module example/project-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "hello.txt"), []byte("bound workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.MCP.WorkspaceRoot = accessRoot
	cfg.MCP.MultiProject = true
	cfg.MCP.ProjectScanDepth = 2
	cfg.MCP.BindingsDir = filepath.Join(accessRoot, ".state", "bindings")
	cfg.MCP.Worktrees = config.WorktreeConfig{Mode: "never", Root: filepath.Join(accessRoot, ".state", "worktrees")}
	cfg.Tunnel.MCPServerURL = "http://127.0.0.1:0/mcp"
	cfg.Tunnel.Executable = filepath.Join(t.TempDir(), "unused.exe")
	cfg.Tunnel.TunnelID = "tunnel_test"
	cfg.Tunnel.APIKeyRef = "env:TEST"
	cfg.MCP.AuthEnabled = false

	r, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.listener.Close()
		r.exec.Close()
		if r.bridge != nil {
			r.bridge.Close()
		}
	})
	httpServer := httptest.NewServer(r.Handler())
	defer httpServer.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "binding-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/mcp",
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	listed, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_projects",
		Arguments: map[string]any{"query": "project-a"},
	})
	if err != nil || listed.IsError {
		t.Fatalf("list_projects failed: err=%v result=%+v", err, listed)
	}

	metaA := mcp.Meta{"openai/session": "conversation-a"}
	selected, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Meta:      metaA,
		Name:      "set_project_root",
		Arguments: map[string]any{"path": "project-a", "createWorktree": false},
	})
	if err != nil || selected.IsError {
		t.Fatalf("set_project_root failed: err=%v result=%+v", err, selected)
	}

	read, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Meta:      metaA,
		Name:      "read_file",
		Arguments: map[string]any{"path": "hello.txt"},
	})
	if err != nil || read.IsError {
		t.Fatalf("bound read_file failed: err=%v result=%+v", err, read)
	}
	if !strings.Contains(fmt.Sprint(read.StructuredContent), "bound workspace") {
		t.Fatalf("unexpected read result: %#v", read.StructuredContent)
	}

	metaB := mcp.Meta{"openai/session": "conversation-b"}
	unbound, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Meta:      metaB,
		Name:      "read_file",
		Arguments: map[string]any{"path": "hello.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !unbound.IsError {
		t.Fatalf("unbound conversation unexpectedly inherited binding: %+v", unbound)
	}
}

func TestUIResourcesAndToolMetadata(t *testing.T) {
	r := testRuntime(t, false)
	httpServer := httptest.NewServer(r.Handler())
	defer httpServer.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "ui-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/mcp",
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	metaByName := map[string]mcp.Meta{}
	for _, tool := range listed.Tools {
		metaByName[tool.Name] = tool.Meta
	}
	if got := metaByName["list_projects"]["ui/resourceUri"]; got != ui.SetupURI {
		t.Fatalf("list_projects UI resource = %#v", got)
	}
	if got := metaByName["show_diff"]["ui/resourceUri"]; got != ui.DiffURI {
		t.Fatalf("show_diff UI resource = %#v", got)
	}

	setup, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: ui.SetupURI})
	if err != nil {
		t.Fatal(err)
	}
	if len(setup.Contents) != 1 || setup.Contents[0].MIMEType != ui.MIMEType || !strings.Contains(setup.Contents[0].Text, "setup_status") {
		t.Fatalf("unexpected setup UI resource: %+v", setup.Contents)
	}
	diff, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: ui.DiffURI})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Contents) != 1 || !strings.Contains(diff.Contents[0].Text, "toolOutput") {
		t.Fatalf("unexpected diff UI resource: %+v", diff.Contents)
	}
}

func TestShowDiffCheckpointsOverMCP(t *testing.T) {
	workspaceRoot := t.TempDir()
	runTestGit(t, workspaceRoot, "init")
	if err := os.WriteFile(filepath.Join(workspaceRoot, "tracked.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, workspaceRoot, "add", "tracked.txt")
	runTestGit(t, workspaceRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "init")

	cfg := config.Default()
	cfg.MCP.WorkspaceRoot = workspaceRoot
	cfg.MCP.AuthEnabled = false
	cfg.Tunnel.MCPServerURL = "http://127.0.0.1:0/mcp"
	cfg.Tunnel.Executable = filepath.Join(t.TempDir(), "unused.exe")
	cfg.Tunnel.TunnelID = "tunnel_test"
	cfg.Tunnel.APIKeyRef = "env:TEST"
	cfg.ArtifactEgress.Dir = filepath.Join(t.TempDir(), "artifacts")
	r, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.listener.Close()
		r.exec.Close()
		if r.bridge != nil {
			r.bridge.Close()
		}
	})
	httpServer := httptest.NewServer(r.Handler())
	defer httpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "diff-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/mcp",
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	write, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "tracked.txt",
			"content": "changed\n",
		},
	})
	if err != nil || write.IsError {
		t.Fatalf("write_file failed: err=%v result=%+v", err, write)
	}

	shown, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "show_diff",
		Arguments: map[string]any{
			"since":   "project_open",
			"advance": true,
		},
	})
	if err != nil || shown.IsError {
		t.Fatalf("show_diff failed: err=%v result=%+v", err, shown)
	}
	structured, ok := shown.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("show_diff structured type=%T", shown.StructuredContent)
	}
	if strings.Contains(fmt.Sprint(structured), "diff --git") {
		t.Fatalf("model-visible structured output leaked patch: %#v", structured)
	}
	if advanced, _ := structured["checkpointAdvanced"].(bool); !advanced {
		t.Fatalf("checkpoint did not advance: %#v", structured)
	}
	metaPayload, ok := shown.Meta["io.github.devnoname120/codexify/diff"].(map[string]any)
	if !ok {
		t.Fatalf("missing diff metadata: %#v", shown.Meta)
	}
	if patchText, _ := metaPayload["patch"].(string); !strings.Contains(patchText, "tracked.txt") {
		t.Fatalf("diff metadata missing patch: %#v", metaPayload)
	}

	incremental, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "show_diff",
		Arguments: map[string]any{"since": "last_diff", "advance": false},
	})
	if err != nil || incremental.IsError {
		t.Fatalf("incremental show_diff failed: err=%v result=%+v", err, incremental)
	}
	if !strings.Contains(fmt.Sprint(incremental.StructuredContent), "No changes since last diff") {
		t.Fatalf("expected empty incremental diff: %#v", incremental.StructuredContent)
	}
}

func TestArtifactMemoryAndSkillsOverMCP(t *testing.T) {
	workspaceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspaceRoot, "report.txt"), []byte("immutable report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(workspaceRoot, ".agents", "skills", "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo\ndescription: Use for demo work\n---\n\nDo demo work.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.MCP.WorkspaceRoot = workspaceRoot
	cfg.MCP.AuthEnabled = false
	cfg.Tunnel.MCPServerURL = "http://127.0.0.1:0/mcp"
	cfg.Tunnel.Executable = filepath.Join(t.TempDir(), "unused.exe")
	cfg.Tunnel.TunnelID = "tunnel_test"
	cfg.Tunnel.APIKeyRef = "env:TEST"
	cfg.Memory.Dir = filepath.Join(t.TempDir(), "memory")
	cfg.Skills.IncludeUser = false
	cfg.ArtifactEgress.Dir = filepath.Join(t.TempDir(), "artifacts")
	cfg.ArtifactEgress.MaxFileBytes = 1 << 20
	cfg.ArtifactEgress.SnapshotMaxFileBytes = 1 << 20
	cfg.ArtifactEgress.MaxSnapshotBytes = 4 << 20
	cfg.ArtifactEgress.MaxReferences = 8

	r, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.listener.Close()
		r.exec.Close()
		if r.bridge != nil {
			r.bridge.Close()
		}
	})
	httpServer := httptest.NewServer(r.Handler())
	defer httpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "feature-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/mcp",
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	remembered, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "remember",
		Arguments: map[string]any{"key": "approach", "value": "use the SDK"},
	})
	if err != nil || remembered.IsError {
		t.Fatalf("remember failed: err=%v result=%+v", err, remembered)
	}
	recalled, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "recall", Arguments: map[string]any{}})
	if err != nil || recalled.IsError || !strings.Contains(fmt.Sprint(recalled.StructuredContent), "use the SDK") {
		t.Fatalf("recall failed: err=%v result=%+v", err, recalled)
	}

	skillsResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "skills_list", Arguments: map[string]any{}})
	if err != nil || skillsResult.IsError || !strings.Contains(fmt.Sprint(skillsResult.StructuredContent), "demo") {
		t.Fatalf("skills_list failed: err=%v result=%+v", err, skillsResult)
	}
	readSkill, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "skills_read", Arguments: map[string]any{"name": "demo"}})
	if err != nil || readSkill.IsError || !strings.Contains(fmt.Sprint(readSkill.StructuredContent), "Do demo work") {
		t.Fatalf("skills_read failed: err=%v result=%+v", err, readSkill)
	}

	exported, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "export_host_file",
		Arguments: map[string]any{"path": "report.txt"},
	})
	if err != nil || exported.IsError {
		t.Fatalf("export_host_file failed: err=%v result=%+v", err, exported)
	}
	var link *mcp.ResourceLink
	for _, content := range exported.Content {
		if candidate, ok := content.(*mcp.ResourceLink); ok {
			link = candidate
			break
		}
	}
	if link == nil || !strings.HasPrefix(link.URI, artifacts.Prefix) {
		t.Fatalf("missing artifact resource link: %+v", exported.Content)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "report.txt"), []byte("changed after export\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resource, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: link.URI})
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.Contents) != 1 || string(resource.Contents[0].Blob) != "immutable report\n" {
		t.Fatalf("artifact snapshot changed: %+v", resource.Contents)
	}
}

func postJSON(t *testing.T, handler http.Handler, body string, sessionID ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	if len(sessionID) > 0 && sessionID[0] != "" {
		req.Header.Set("Mcp-Session-Id", sessionID[0])
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
