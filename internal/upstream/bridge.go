package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Bridge struct {
	mu       sync.RWMutex
	sources  map[string]*source
	sessions []*mcp.ClientSession
	report   []string
	log      *slog.Logger
}

type source struct {
	name      string
	mode      string
	transport string
	timeout   time.Duration
	session   *mcp.ClientSession
	info      *mcp.Implementation
	tools     map[string]*mcp.Tool
}

type SourceInfo struct {
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	Transport  string `json:"transport"`
	ServerName string `json:"serverName,omitempty"`
	Version    string `json:"version,omitempty"`
	ToolCount  int    `json:"toolCount"`
}

type ListSourcesInput struct{}

type ListSourcesOutput struct {
	Sources []SourceInfo `json:"sources"`
}

type SearchToolsInput struct {
	Query  string `json:"query,omitempty" jsonschema:"case-insensitive substring over source, tool name, title, and description"`
	Source string `json:"source,omitempty" jsonschema:"optional upstream source name"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum results; default 50 and maximum 200"`
}

type ToolInfo struct {
	Source       string `json:"source"`
	Name         string `json:"name"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	InputSchema  any    `json:"inputSchema,omitempty"`
	OutputSchema any    `json:"outputSchema,omitempty"`
}

type SearchToolsOutput struct {
	Tools []ToolInfo `json:"tools"`
	Total int        `json:"total"`
}

type GetToolInput struct {
	Source string `json:"source"`
	Name   string `json:"name"`
}

type CallToolInput struct {
	Source    string         `json:"source"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

func ConnectAndRegister(ctx context.Context, specs []config.UpstreamMCPConfig, server *mcp.Server, logger *slog.Logger, used map[string]struct{}) (*Bridge, error) {
	if logger == nil {
		logger = slog.Default()
	}
	b := &Bridge{
		sources: make(map[string]*source),
		log:     logger,
	}
	for _, spec := range specs {
		src, err := connectSource(ctx, spec, logger)
		if err != nil {
			line := fmt.Sprintf("%s -> FAILED: %v", spec.Name, err)
			b.report = append(b.report, line)
			if spec.Required {
				b.Close()
				return nil, errors.New(line)
			}
			logger.Warn("upstream MCP unavailable", "source", spec.Name, "error", err)
			continue
		}
		b.sources[src.name] = src
		b.sessions = append(b.sessions, src.session)
		b.report = append(b.report, fmt.Sprintf("%s -> %s (%d tool(s))", src.name, src.mode, len(src.tools)))
		if src.mode == "direct" {
			registerDirect(server, src, used)
		}
	}

	if b.hasCatalogSources() {
		b.registerCatalog(server, used)
	}
	return b, nil
}

func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, session := range b.sessions {
		_ = session.Close()
	}
	b.sessions = nil
}

func (b *Bridge) Report() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]string(nil), b.report...)
}

func connectSource(ctx context.Context, spec config.UpstreamMCPConfig, logger *slog.Logger) (*source, error) {
	transportKind := normalizedTransport(spec)
	mode := strings.ToLower(strings.TrimSpace(spec.Mode))
	if mode == "" {
		mode = "catalog"
	}
	timeout := spec.Timeout.Duration()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "codexify-go-bridge", Version: "0.4.0-dev"}, nil)
	var transport mcp.Transport
	switch transportKind {
	case "stdio":
		cmd := exec.Command(spec.Command, spec.Args...)
		if spec.Workdir != "" {
			cmd.Dir = spec.Workdir
		}
		cmd.Env = append([]string{}, os.Environ()...)
		for key, value := range spec.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		transport = &mcp.CommandTransport{
			Command:           cmd,
			TerminateDuration: 3 * time.Second,
		}
	case "streamable_http":
		transport = &mcp.StreamableClientTransport{
			Endpoint:             spec.URL,
			DisableStandaloneSSE: true,
			MaxRetries:           -1,
			HTTPClient: &http.Client{
				Transport: &headerRoundTripper{
					base:    http.DefaultTransport,
					headers: spec.Headers,
				},
			},
		}
	default:
		return nil, fmt.Errorf("unsupported transport %q", transportKind)
	}

	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, err
	}
	tools, err := listAllTools(connectCtx, session)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	toolMap := make(map[string]*mcp.Tool, len(tools))
	for _, tool := range tools {
		if tool == nil || strings.TrimSpace(tool.Name) == "" {
			continue
		}
		toolMap[tool.Name] = cloneTool(tool)
	}
	var info *mcp.Implementation
	if init := session.InitializeResult(); init != nil && init.ServerInfo != nil {
		copyInfo := *init.ServerInfo
		info = &copyInfo
	}
	logger.Info("upstream MCP connected",
		"source", spec.Name,
		"mode", mode,
		"transport", transportKind,
		"tools", len(toolMap),
	)
	return &source{
		name:      spec.Name,
		mode:      mode,
		transport: transportKind,
		timeout:   timeout,
		session:   session,
		info:      info,
		tools:     toolMap,
	}, nil
}

func listAllTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	cursor := ""
	for {
		result, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return tools, nil
		}
		cursor = result.NextCursor
	}
}

func registerDirect(server *mcp.Server, src *source, used map[string]struct{}) {
	names := make([]string, 0, len(src.tools))
	for name := range src.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, original := range names {
		upstreamTool := src.tools[original]
		downstreamName := uniqueToolName(sanitizeToolName(src.name+"__"+original), used)
		tool := cloneTool(upstreamTool)
		tool.Name = downstreamName
		if strings.TrimSpace(tool.Title) == "" {
			tool.Title = src.name + ": " + original
		}
		server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			callCtx, cancel := context.WithTimeout(ctx, src.timeout)
			defer cancel()
			var args any = map[string]any{}
			if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, fmt.Errorf("decode bridged arguments: %w", err)
				}
			}
			return src.session.CallTool(callCtx, &mcp.CallToolParams{
				Name:      original,
				Arguments: args,
			})
		})
	}
}

func (b *Bridge) registerCatalog(server *mcp.Server, used map[string]struct{}) {
	listName := uniqueToolName("mcp_list_sources", used)
	searchName := uniqueToolName("mcp_search_tools", used)
	getName := uniqueToolName("mcp_get_tool", used)
	callName := uniqueToolName("mcp_call_tool", used)

	mcp.AddTool(server, &mcp.Tool{Name: listName, Description: "List privately indexed upstream MCP servers."},
		func(context.Context, *mcp.CallToolRequest, ListSourcesInput) (*mcp.CallToolResult, ListSourcesOutput, error) {
			return nil, b.listSources(), nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: searchName, Description: "Search tools exposed by privately indexed upstream MCP servers."},
		func(_ context.Context, _ *mcp.CallToolRequest, in SearchToolsInput) (*mcp.CallToolResult, SearchToolsOutput, error) {
			return nil, b.searchTools(in), nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: getName, Description: "Get one upstream MCP tool definition without exposing every upstream tool in the main catalog."},
		func(_ context.Context, _ *mcp.CallToolRequest, in GetToolInput) (*mcp.CallToolResult, ToolInfo, error) {
			info, err := b.getTool(in.Source, in.Name)
			return nil, info, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: callName, Description: "Call one tool on a privately indexed upstream MCP server."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in CallToolInput) (*mcp.CallToolResult, any, error) {
			result, err := b.callTool(ctx, in.Source, in.Name, in.Arguments)
			if err != nil {
				return nil, nil, err
			}
			return result, result.StructuredContent, nil
		})
}

func (b *Bridge) listSources() ListSourcesOutput {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var sources []SourceInfo
	for _, src := range b.sources {
		if src.mode != "catalog" {
			continue
		}
		row := SourceInfo{
			Name:      src.name,
			Mode:      src.mode,
			Transport: src.transport,
			ToolCount: len(src.tools),
		}
		if src.info != nil {
			row.ServerName = src.info.Name
			row.Version = src.info.Version
		}
		sources = append(sources, row)
	}
	sort.Slice(sources, func(i, j int) bool { return strings.ToLower(sources[i].Name) < strings.ToLower(sources[j].Name) })
	return ListSourcesOutput{Sources: sources}
}

func (b *Bridge) searchTools(in SearchToolsInput) SearchToolsOutput {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	query := strings.ToLower(strings.TrimSpace(in.Query))
	sourceFilter := strings.ToLower(strings.TrimSpace(in.Source))

	b.mu.RLock()
	defer b.mu.RUnlock()
	var all []ToolInfo
	for _, src := range b.sources {
		if src.mode != "catalog" {
			continue
		}
		if sourceFilter != "" && strings.ToLower(src.name) != sourceFilter {
			continue
		}
		for _, tool := range src.tools {
			info := toolInfo(src.name, tool)
			if query != "" {
				haystack := strings.ToLower(info.Source + "\n" + info.Name + "\n" + info.Title + "\n" + info.Description)
				if !strings.Contains(haystack, query) {
					continue
				}
			}
			all = append(all, info)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if strings.EqualFold(all[i].Source, all[j].Source) {
			return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name)
		}
		return strings.ToLower(all[i].Source) < strings.ToLower(all[j].Source)
	})
	total := len(all)
	if len(all) > limit {
		all = all[:limit]
	}
	return SearchToolsOutput{Tools: all, Total: total}
}

func (b *Bridge) getTool(sourceName, toolName string) (ToolInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	src := b.sources[sourceName]
	if src == nil || src.mode != "catalog" {
		return ToolInfo{}, fmt.Errorf("unknown catalog MCP source %q", sourceName)
	}
	tool := src.tools[toolName]
	if tool == nil {
		return ToolInfo{}, fmt.Errorf("unknown MCP tool %q on source %q", toolName, sourceName)
	}
	return toolInfo(src.name, tool), nil
}

func (b *Bridge) callTool(ctx context.Context, sourceName, toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	b.mu.RLock()
	src := b.sources[sourceName]
	b.mu.RUnlock()
	if src == nil || src.mode != "catalog" {
		return nil, fmt.Errorf("unknown catalog MCP source %q", sourceName)
	}
	if src.tools[toolName] == nil {
		return nil, fmt.Errorf("unknown MCP tool %q on source %q", toolName, sourceName)
	}
	callCtx, cancel := context.WithTimeout(ctx, src.timeout)
	defer cancel()
	return src.session.CallTool(callCtx, &mcp.CallToolParams{Name: toolName, Arguments: args})
}

func (b *Bridge) hasCatalogSources() bool {
	for _, src := range b.sources {
		if src.mode == "catalog" {
			return true
		}
	}
	return false
}

func toolInfo(sourceName string, tool *mcp.Tool) ToolInfo {
	return ToolInfo{
		Source:       sourceName,
		Name:         tool.Name,
		Title:        tool.Title,
		Description:  tool.Description,
		InputSchema:  tool.InputSchema,
		OutputSchema: tool.OutputSchema,
	}
}

func normalizedTransport(spec config.UpstreamMCPConfig) string {
	value := strings.ToLower(strings.TrimSpace(spec.Transport))
	if value == "" {
		if spec.URL != "" {
			return "streamable_http"
		}
		return "stdio"
	}
	switch value {
	case "streamable-http", "http":
		return "streamable_http"
	default:
		return value
	}
}

func uniqueToolName(base string, used map[string]struct{}) string {
	if _, exists := used[base]; !exists {
		used[base] = struct{}{}
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", base, i)
		if _, exists := used[candidate]; !exists {
			used[candidate] = struct{}{}
			return candidate
		}
	}
}

func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "upstream_tool"
	}
	if len(out) > 96 {
		out = out[:96]
	}
	return out
}

func cloneTool(in *mcp.Tool) *mcp.Tool {
	if in == nil {
		return nil
	}
	out := *in
	if out.InputSchema == nil {
		out.InputSchema = map[string]any{"type": "object"}
	}
	if in.Annotations != nil {
		annotations := *in.Annotations
		out.Annotations = &annotations
	}
	out.Icons = append([]mcp.Icon(nil), in.Icons...)
	return &out
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	copyReq := req.Clone(req.Context())
	copyReq.Header = req.Header.Clone()
	for key, value := range t.headers {
		copyReq.Header.Set(key, value)
	}
	return t.base.RoundTrip(copyReq)
}
