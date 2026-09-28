package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/config"
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

	listBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	listResp := postJSON(t, r.Handler(), listBody)
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
	writeResp := postJSON(t, r.Handler(), writeBody)
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
	readResp := postJSON(t, r.Handler(), readBody)
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

func postJSON(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
