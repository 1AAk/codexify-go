package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"runtime"
	"strings"
	"time"

	"github.com/benice2me11/codexify-go/internal/agenttools"
	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/execsession"
	"github.com/benice2me11/codexify-go/internal/projects"
	"github.com/benice2me11/codexify-go/internal/upstream"
	"github.com/benice2me11/codexify-go/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const InternalAuthEnv = "CODEXIFY_GO_INTERNAL_MCP_AUTHORIZATION"

type Runtime struct {
	cfg      config.Config
	root     *workspace.Root
	projects *projects.Manager
	bridge   *upstream.Bridge
	exec     *execsession.Manager
	server   *mcp.Server
	http     *http.Server
	listener net.Listener
	token    string
	log      *slog.Logger
}

func New(cfg config.Config, logger *slog.Logger) (*Runtime, error) {
	return NewWithToken(cfg, logger, "")
}

func NewWithToken(cfg config.Config, logger *slog.Logger, token string) (*Runtime, error) {
	projectManager, err := projects.New(cfg.MCP)
	if err != nil {
		return nil, err
	}
	root := projectManager.AccessRoot()
	endpoint, listen, err := endpointFromURL(cfg.Tunnel.MCPServerURL)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("listen MCP: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}

	if cfg.MCP.AuthEnabled {
		if token == "" {
			token, err = GenerateToken()
			if err != nil {
				ln.Close()
				return nil, err
			}
		}
	}

	r := &Runtime{
		cfg:      cfg,
		root:     root,
		projects: projectManager,
		exec:     execsession.NewManager(),
		listener: ln,
		token:    token,
		log:      logger,
	}
	r.server = mcp.NewServer(&mcp.Implementation{
		Name:    "codexify-go",
		Version: "0.4.0-dev",
	}, &mcp.ServerOptions{Logger: logger})
	r.registerTools()
	bridge, err := upstream.ConnectAndRegister(context.Background(), cfg.MCP.Upstreams, r.server, logger, builtInToolNames())
	if err != nil {
		ln.Close()
		r.exec.Close()
		return nil, fmt.Errorf("connect upstream MCP servers: %w", err)
	}
	r.bridge = bridge

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return r.server
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		MaxRequestBodyBytes:          cfg.MCP.MaxRequestBodyBytes,
		Logger:                       logger,
		PropagateRequestCancellation: true,
	})

	mux := http.NewServeMux()
	mux.Handle(endpoint, r.auth(mcpHandler))
	mux.HandleFunc("/health", r.health)
	r.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return r, nil
}

func (r *Runtime) Serve() error {
	r.log.Info("MCP server listening",
		"addr", r.listener.Addr().String(),
		"workspace", r.root.Path(),
		"auth", r.cfg.MCP.AuthEnabled,
	)
	err := r.http.Serve(r.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	err := r.http.Shutdown(ctx)
	r.exec.Close()
	if r.bridge != nil {
		r.bridge.Close()
	}
	return err
}

func (r *Runtime) TunnelEnvironment() (string, map[string]string) {
	if !r.cfg.MCP.AuthEnabled {
		return "", nil
	}
	return "env:" + InternalAuthEnv, map[string]string{
		InternalAuthEnv: "Bearer " + r.token,
	}
}

func AuthEnvironment(token string) (string, map[string]string) {
	if token == "" {
		return "", nil
	}
	return "env:" + InternalAuthEnv, map[string]string{
		InternalAuthEnv: "Bearer " + token,
	}
}

func (r *Runtime) Handler() http.Handler {
	return r.http.Handler
}

func (r *Runtime) registerTools() {
	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "list_projects",
		Description: "List selectable projects below the configured access root before binding this ChatGPT conversation.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in ListProjectsInput) (*mcp.CallToolResult, projects.ListOutput, error) {
		out, err := r.projects.List(in.Query, in.Limit)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "set_project_root",
		Description: "Bind this ChatGPT conversation to one existing project below the access root. Repeating the same selection is idempotent; switching to another project is rejected.",
	}, func(_ context.Context, req *mcp.CallToolRequest, in SetProjectRootInput) (*mcp.CallToolResult, projects.WorkspaceInfo, error) {
		out, err := r.projects.Select(requestMeta(req), in.Path, in.CreateWorktree)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "list_worktrees",
		Description: "List Git worktrees belonging to the project selected for this conversation.",
	}, func(_ context.Context, req *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, projects.WorktreeListOutput, error) {
		out, err := r.projects.ListWorktrees(requestMeta(req))
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "get_environment",
		Description: "Return the active workspace root, platform, and default shell.",
	}, func(_ context.Context, req *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, EnvironmentOutput, error) {
		root, selection, err := r.workspaceFor(req)
		if err != nil {
			return nil, EnvironmentOutput{}, err
		}
		shell := "/bin/sh"
		if runtime.GOOS == "windows" {
			shell = "powershell"
		}
		username := "unknown"
		if current, err := user.Current(); err == nil {
			username = current.Username
		}
		return nil, EnvironmentOutput{
			Platform:        runtime.GOOS + "/" + runtime.GOARCH,
			WorkspaceRoot:   root.Path(),
			AccessRoot:      selection.AccessRoot,
			ManagedWorktree: selection.ManagedWorktree,
			BindingScope:    selection.BindingScope,
			DefaultShell:    shell,
			Username:        username,
		}, nil
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a UTF-8 text file inside the workspace with line numbers.",
	}, func(_ context.Context, req *mcp.CallToolRequest, in agenttools.ReadFileInput) (*mcp.CallToolResult, agenttools.ReadFileOutput, error) {
		files, err := r.filesFor(req)
		if err != nil {
			return nil, agenttools.ReadFileOutput{}, err
		}
		out, err := files.ReadFile(in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "write_file",
		Description: "Create or replace a UTF-8 text file inside the workspace. Parent directories are created automatically.",
	}, func(_ context.Context, req *mcp.CallToolRequest, in agenttools.WriteFileInput) (*mcp.CallToolResult, agenttools.WriteFileOutput, error) {
		files, err := r.filesFor(req)
		if err != nil {
			return nil, agenttools.WriteFileOutput{}, err
		}
		out, err := files.WriteFile(in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "glob",
		Description: "Find files inside the workspace using glob patterns including double-star recursion.",
	}, func(_ context.Context, req *mcp.CallToolRequest, in agenttools.GlobInput) (*mcp.CallToolResult, agenttools.GlobOutput, error) {
		files, err := r.filesFor(req)
		if err != nil {
			return nil, agenttools.GlobOutput{}, err
		}
		out, err := files.Glob(in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "grep",
		Description: "Search text files inside the workspace with an RE2 regular expression.",
	}, func(_ context.Context, req *mcp.CallToolRequest, in agenttools.GrepInput) (*mcp.CallToolResult, agenttools.GrepOutput, error) {
		files, err := r.filesFor(req)
		if err != nil {
			return nil, agenttools.GrepOutput{}, err
		}
		out, err := files.Grep(in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "exec_command",
		Description: "Run a command in a workspace-relative directory. Long-running commands return a session id for write_stdin.",
	}, r.execCommand)

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "write_stdin",
		Description: "Send input to or poll a running exec_command session.",
	}, r.writeStdin)

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "git_status",
		Description: "Show concise Git working-tree and branch status for the workspace.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in agenttools.GitStatusInput) (*mcp.CallToolResult, agenttools.GitOutput, error) {
		git, err := r.gitFor(req)
		if err != nil {
			return nil, agenttools.GitOutput{}, err
		}
		out, err := git.Status(ctx, in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "git_diff",
		Description: "Show the workspace Git diff without external diff helpers or color.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in agenttools.GitDiffInput) (*mcp.CallToolResult, agenttools.GitOutput, error) {
		git, err := r.gitFor(req)
		if err != nil {
			return nil, agenttools.GitOutput{}, err
		}
		out, err := git.Diff(ctx, in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "git_log",
		Description: "Show recent Git commits for the workspace.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in agenttools.GitLogInput) (*mcp.CallToolResult, agenttools.GitOutput, error) {
		git, err := r.gitFor(req)
		if err != nil {
			return nil, agenttools.GitOutput{}, err
		}
		out, err := git.Log(ctx, in)
		return nil, out, err
	})
}

type EmptyInput struct{}

type ListProjectsInput struct {
	Query string `json:"query,omitempty" jsonschema:"optional case-insensitive filter over project name, selector, and description"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum projects to return, default 50 and maximum 200"`
}

type SetProjectRootInput struct {
	Path           string `json:"path" jsonschema:"project path relative to the configured access root; use a selector returned by list_projects"`
	CreateWorktree *bool  `json:"createWorktree,omitempty" jsonschema:"explicitly force or disable managed-worktree creation; omit to follow configured worktree mode"`
}

type EnvironmentOutput struct {
	Platform        string `json:"platform"`
	WorkspaceRoot   string `json:"workspaceRoot"`
	AccessRoot      string `json:"accessRoot"`
	ManagedWorktree bool   `json:"managedWorktree"`
	BindingScope    string `json:"bindingScope"`
	DefaultShell    string `json:"defaultShell"`
	Username        string `json:"username"`
}

type ExecCommandInput struct {
	Command     string `json:"cmd" jsonschema:"shell command to execute"`
	Shell       string `json:"shell,omitempty" jsonschema:"powershell, pwsh, cmd, sh, bash, or zsh depending on platform"`
	Workdir     string `json:"workdir,omitempty" jsonschema:"workspace-relative working directory"`
	YieldTimeMS int    `json:"yield_time_ms,omitempty" jsonschema:"wait before returning; maximum 30000 milliseconds"`
}

type ExecCommandOutput struct {
	Output    string `json:"output"`
	SessionID string `json:"session_id,omitempty"`
	Running   bool   `json:"running"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

func (r *Runtime) execCommand(_ context.Context, req *mcp.CallToolRequest, in ExecCommandInput) (*mcp.CallToolResult, ExecCommandOutput, error) {
	root, _, err := r.workspaceFor(req)
	if err != nil {
		return nil, ExecCommandOutput{}, err
	}
	workdir, err := root.Resolve(in.Workdir, false)
	if err != nil {
		return nil, ExecCommandOutput{}, err
	}
	info, err := os.Stat(workdir)
	if err != nil {
		return nil, ExecCommandOutput{}, err
	}
	if !info.IsDir() {
		return nil, ExecCommandOutput{}, errors.New("workdir is not a directory")
	}
	res, err := r.exec.Start(execsession.StartInput{
		Command: in.Command,
		Shell:   in.Shell,
		Workdir: workdir,
		Yield:   time.Duration(in.YieldTimeMS) * time.Millisecond,
	})
	if err != nil {
		return nil, ExecCommandOutput{}, err
	}
	return nil, ExecCommandOutput{
		Output:    res.Output,
		SessionID: res.SessionID,
		Running:   res.Running,
		ExitCode:  res.ExitCode,
	}, nil
}

type WriteStdinInput struct {
	SessionID   string `json:"session_id" jsonschema:"session id returned by exec_command"`
	Chars       string `json:"chars,omitempty" jsonschema:"characters to write; include a newline when needed"`
	YieldTimeMS int    `json:"yield_time_ms,omitempty" jsonschema:"wait before returning; maximum 30000 milliseconds"`
}

func (r *Runtime) writeStdin(_ context.Context, _ *mcp.CallToolRequest, in WriteStdinInput) (*mcp.CallToolResult, ExecCommandOutput, error) {
	res, err := r.exec.Write(in.SessionID, in.Chars, time.Duration(in.YieldTimeMS)*time.Millisecond)
	if err != nil {
		return nil, ExecCommandOutput{}, err
	}
	return nil, ExecCommandOutput{
		Output:    res.Output,
		SessionID: res.SessionID,
		Running:   res.Running,
		ExitCode:  res.ExitCode,
	}, nil
}

func (r *Runtime) workspaceFor(req *mcp.CallToolRequest) (*workspace.Root, projects.WorkspaceInfo, error) {
	return r.projects.Workspace(requestMeta(req))
}

func (r *Runtime) filesFor(req *mcp.CallToolRequest) (*agenttools.Files, error) {
	root, _, err := r.workspaceFor(req)
	if err != nil {
		return nil, err
	}
	return &agenttools.Files{Root: root}, nil
}

func (r *Runtime) gitFor(req *mcp.CallToolRequest) (*agenttools.Git, error) {
	root, _, err := r.workspaceFor(req)
	if err != nil {
		return nil, err
	}
	return &agenttools.Git{Root: root}, nil
}

func requestMeta(req *mcp.CallToolRequest) map[string]any {
	if req == nil || req.Params == nil || req.Params.Meta == nil {
		return nil
	}
	return req.Params.Meta
}

func builtInToolNames() map[string]struct{} {
	names := []string{
		"list_projects", "set_project_root", "list_worktrees", "get_environment",
		"read_file", "write_file", "glob", "grep", "exec_command", "write_stdin",
		"git_status", "git_diff", "git_log",
	}
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[name] = struct{}{}
	}
	return out
}

func (r *Runtime) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.cfg.MCP.AuthEnabled {
			if req.Header.Get("Authorization") != "Bearer "+r.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Runtime) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"workspace": r.root.Path(),
		"platform":  runtime.GOOS + "/" + runtime.GOARCH,
	})
}

func endpointFromURL(raw string) (endpoint, listen string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse tunnel MCP server URL: %w", err)
	}
	if u.Scheme != "http" {
		return "", "", errors.New("tunnel.mcpServerUrl must use http")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", "", fmt.Errorf("tunnel.mcpServerUrl must use a loopback host, got %q", host)
	}
	if u.Port() == "" {
		return "", "", errors.New("tunnel.mcpServerUrl must include an explicit port")
	}
	endpoint = u.EscapedPath()
	if endpoint == "" {
		endpoint = "/mcp"
	}
	return endpoint, u.Host, nil
}

func GenerateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate MCP auth token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func MergeTunnelEnvironment(cfg *config.Config, authRef string, env map[string]string) {
	if cfg.Tunnel.Environment == nil {
		cfg.Tunnel.Environment = make(map[string]string)
	}
	for k, v := range env {
		cfg.Tunnel.Environment[k] = v
	}
	cfg.Tunnel.MCPAuthorizationRef = authRef
}

func RedactedEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimSuffix(u.String(), "/")
}

func HealthURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" {
		return "", errors.New("MCP URL must use http")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("MCP URL must use loopback, got %q", host)
	}
	u.Path = "/health"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
