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
	"github.com/benice2me11/codexify-go/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const InternalAuthEnv = "CODEXIFY_GO_INTERNAL_MCP_AUTHORIZATION"

type Runtime struct {
	cfg      config.Config
	root     *workspace.Root
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
	root, err := workspace.New(cfg.MCP.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
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
		exec:     execsession.NewManager(),
		listener: ln,
		token:    token,
		log:      logger,
	}
	r.server = mcp.NewServer(&mcp.Implementation{
		Name:    "codexify-go",
		Version: "0.3.0-dev",
	}, &mcp.ServerOptions{Logger: logger})
	r.registerTools()

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
	r.exec.Close()
	return r.http.Shutdown(ctx)
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
	files := &agenttools.Files{Root: r.root}
	git := &agenttools.Git{Root: r.root}

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "get_environment",
		Description: "Return the active workspace root, platform, and default shell.",
	}, func(context.Context, *mcp.CallToolRequest, EmptyInput) (*mcp.CallToolResult, EnvironmentOutput, error) {
		shell := "/bin/sh"
		if runtime.GOOS == "windows" {
			shell = "powershell"
		}
		username := "unknown"
		if current, err := user.Current(); err == nil {
			username = current.Username
		}
		return nil, EnvironmentOutput{
			Platform:      runtime.GOOS + "/" + runtime.GOARCH,
			WorkspaceRoot: r.root.Path(),
			DefaultShell:  shell,
			Username:      username,
		}, nil
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a UTF-8 text file inside the workspace with line numbers.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in agenttools.ReadFileInput) (*mcp.CallToolResult, agenttools.ReadFileOutput, error) {
		out, err := files.ReadFile(in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "write_file",
		Description: "Create or replace a UTF-8 text file inside the workspace. Parent directories are created automatically.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in agenttools.WriteFileInput) (*mcp.CallToolResult, agenttools.WriteFileOutput, error) {
		out, err := files.WriteFile(in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "glob",
		Description: "Find files inside the workspace using glob patterns including double-star recursion.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in agenttools.GlobInput) (*mcp.CallToolResult, agenttools.GlobOutput, error) {
		out, err := files.Glob(in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "grep",
		Description: "Search text files inside the workspace with an RE2 regular expression.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in agenttools.GrepInput) (*mcp.CallToolResult, agenttools.GrepOutput, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in agenttools.GitStatusInput) (*mcp.CallToolResult, agenttools.GitOutput, error) {
		out, err := git.Status(ctx, in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "git_diff",
		Description: "Show the workspace Git diff without external diff helpers or color.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in agenttools.GitDiffInput) (*mcp.CallToolResult, agenttools.GitOutput, error) {
		out, err := git.Diff(ctx, in)
		return nil, out, err
	})

	mcp.AddTool(r.server, &mcp.Tool{
		Name:        "git_log",
		Description: "Show recent Git commits for the workspace.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in agenttools.GitLogInput) (*mcp.CallToolResult, agenttools.GitOutput, error) {
		out, err := git.Log(ctx, in)
		return nil, out, err
	})
}

type EmptyInput struct{}

type EnvironmentOutput struct {
	Platform      string `json:"platform"`
	WorkspaceRoot string `json:"workspaceRoot"`
	DefaultShell  string `json:"defaultShell"`
	Username      string `json:"username"`
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

func (r *Runtime) execCommand(_ context.Context, _ *mcp.CallToolRequest, in ExecCommandInput) (*mcp.CallToolResult, ExecCommandOutput, error) {
	workdir, err := r.root.Resolve(in.Workdir, false)
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
