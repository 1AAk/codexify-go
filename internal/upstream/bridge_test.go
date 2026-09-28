package upstream

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Text string `json:"text"`
}

type echoOutput struct {
	Text string `json:"text"`
}

type emptyInput struct{}

type resourceOutput struct {
	OK bool `json:"ok"`
}

func TestBridgeCatalogAndDirectModes(t *testing.T) {
	upstreamServer := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0.0"}, nil)
	mcp.AddTool(upstreamServer, &mcp.Tool{Name: "echo", Description: "echo text"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
			return nil, echoOutput{Text: in.Text}, nil
		})
	upstreamServer.AddResource(&mcp.Resource{URI: "fixture://document/1", Name: "fixture-document", MIMEType: "text/plain"},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
				URI: "fixture://document/1", MIMEType: "text/plain", Text: "bridged resource body",
			}}}, nil
		})
	mcp.AddTool(upstreamServer, &mcp.Tool{Name: "resource_link", Description: "return a resource link"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, resourceOutput, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ResourceLink{
				URI: "fixture://document/1", Name: "fixture-document", MIMEType: "text/plain",
			}}}, resourceOutput{OK: true}, nil
		})
	upstreamHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return upstreamServer },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	))
	defer upstreamHTTP.Close()

	downstream := mcp.NewServer(&mcp.Implementation{Name: "downstream", Version: "1"}, nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge, err := ConnectAndRegister(context.Background(), []config.UpstreamMCPConfig{
		{Name: "private", URL: upstreamHTTP.URL, Transport: "streamable_http", Mode: "catalog"},
		{Name: "direct", URL: upstreamHTTP.URL, Transport: "streamable_http", Mode: "direct"},
	}, downstream, logger, map[string]struct{}{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()

	downstreamHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return downstream },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	))
	defer downstreamHTTP.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             downstreamHTTP.URL,
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
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	for _, expected := range []string{"direct__echo", "mcp_list_sources", "mcp_search_tools", "mcp_get_tool", "mcp_call_tool"} {
		if !names[expected] {
			t.Fatalf("missing tool %q in %#v", expected, names)
		}
	}

	direct, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "direct__echo",
		Arguments: map[string]any{"text": "direct works"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if direct.IsError {
		t.Fatalf("direct call failed: %+v", direct.Content)
	}
	assertStructuredText(t, direct.StructuredContent, "direct works")

	catalog, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "mcp_call_tool",
		Arguments: map[string]any{
			"source":    "private",
			"name":      "echo",
			"arguments": map[string]any{"text": "catalog works"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.IsError {
		t.Fatalf("catalog call failed: %+v", catalog.Content)
	}
	assertStructuredText(t, catalog.StructuredContent, "catalog works")

	resourceCall, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "mcp_call_tool",
		Arguments: map[string]any{
			"source":    "private",
			"name":      "resource_link",
			"arguments": map[string]any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var link *mcp.ResourceLink
	for _, content := range resourceCall.Content {
		if candidate, ok := content.(*mcp.ResourceLink); ok {
			link = candidate
			break
		}
	}
	if link == nil || !strings.HasPrefix(link.URI, resourcePrefix) {
		t.Fatalf("resource link was not rewritten: %+v", resourceCall.Content)
	}
	read, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: link.URI})
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || read.Contents[0].Text != "bridged resource body" || read.Contents[0].URI != link.URI {
		t.Fatalf("unexpected bridged resource: %+v", read.Contents)
	}
}

func TestOptionalUpstreamFailureIsReported(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "downstream", Version: "1"}, nil)
	bridge, err := ConnectAndRegister(context.Background(), []config.UpstreamMCPConfig{
		{Name: "offline", URL: "http://127.0.0.1:1/mcp", Transport: "streamable_http", Mode: "catalog", Required: false},
	}, server, slog.New(slog.NewTextHandler(io.Discard, nil)), map[string]struct{}{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	report := bridge.Report()
	if len(report) != 1 || !strings.Contains(report[0], "FAILED") {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func assertStructuredText(t *testing.T, value any, want string) {
	t.Helper()
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("structured content type = %T, value=%#v", value, value)
	}
	if got, _ := m["text"].(string); got != want {
		t.Fatalf("text=%q want=%q", got, want)
	}
}
