package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }

type Config struct {
	Version    int              `json:"version"`
	Log        LogConfig        `json:"log"`
	Service    ServiceConfig    `json:"service"`
	Tunnel     TunnelConfig     `json:"tunnel"`
	Supervisor SupervisorConfig `json:"supervisor"`
}

type LogConfig struct {
	File  string `json:"file"`
	Level string `json:"level"`
}

type ServiceConfig struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

type TunnelConfig struct {
	Executable          string            `json:"executable"`
	TunnelID            string            `json:"tunnelId"`
	APIKeyRef           string            `json:"apiKeyRef"`
	OrganizationID      string            `json:"organizationId,omitempty"`
	MCPServerURL        string            `json:"mcpServerUrl"`
	MCPAuthorizationRef string            `json:"mcpAuthorizationRef,omitempty"`
	StartupWaitTimeout  Duration          `json:"startupWaitTimeout"`
	HealthURLFile       string            `json:"healthUrlFile"`
	Environment         map[string]string `json:"environment,omitempty"`
	ExtraArgs           []string          `json:"extraArgs,omitempty"`
}

type SupervisorConfig struct {
	MinBackoff             Duration `json:"minBackoff"`
	MaxBackoff             Duration `json:"maxBackoff"`
	StableWindow           Duration `json:"stableWindow"`
	HealthInterval         Duration `json:"healthInterval"`
	HealthFailureThreshold int      `json:"healthFailureThreshold"`
	ShutdownTimeout        Duration `json:"shutdownTimeout"`
}

func Default() Config {
	return Config{
		Version: 1,
		Log: LogConfig{
			File:  "codexify-go.log",
			Level: "info",
		},
		Service: ServiceConfig{
			Name:        "CodexifyGo",
			DisplayName: "Codexify Go",
			Description: "Native Windows supervisor for Codexify-compatible MCP and OpenAI tunnel runtime.",
		},
		Tunnel: TunnelConfig{
			StartupWaitTimeout: Duration(15 * time.Second),
			HealthURLFile:      filepath.Join(os.TempDir(), "codexify-go-tunnel-health.url"),
		},
		Supervisor: SupervisorConfig{
			MinBackoff:             Duration(2 * time.Second),
			MaxBackoff:             Duration(60 * time.Second),
			StableWindow:           Duration(60 * time.Second),
			HealthInterval:         Duration(5 * time.Second),
			HealthFailureThreshold: 3,
			ShutdownTimeout:        Duration(10 * time.Second),
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve config path: %w", err)
	}
	base := filepath.Dir(absPath)
	cfg.expand(base)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) expand(base string) {
	expand := func(v string) string {
		v = os.ExpandEnv(v)
		if v == "" || filepath.IsAbs(v) {
			return v
		}
		return filepath.Clean(filepath.Join(base, v))
	}
	c.Log.File = expand(c.Log.File)
	c.Tunnel.Executable = expand(c.Tunnel.Executable)
	c.Tunnel.HealthURLFile = expand(c.Tunnel.HealthURLFile)
	c.Tunnel.APIKeyRef = expandReference(c.Tunnel.APIKeyRef, base)
	c.Tunnel.MCPAuthorizationRef = os.ExpandEnv(c.Tunnel.MCPAuthorizationRef)
	c.Tunnel.MCPServerURL = os.ExpandEnv(c.Tunnel.MCPServerURL)
	for k, v := range c.Tunnel.Environment {
		c.Tunnel.Environment[k] = os.ExpandEnv(v)
	}
}

func expandReference(v, base string) string {
	v = os.ExpandEnv(v)
	if !strings.HasPrefix(v, "file:") {
		return v
	}
	path := strings.TrimPrefix(v, "file:")
	if path == "" {
		return v
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return "file:" + filepath.Clean(path)
}

func (c Config) Validate() error {
	var errs []error
	if c.Version != 1 {
		errs = append(errs, fmt.Errorf("unsupported config version %d", c.Version))
	}
	if strings.TrimSpace(c.Service.Name) == "" {
		errs = append(errs, errors.New("service.name is required"))
	}
	if strings.TrimSpace(c.Tunnel.Executable) == "" {
		errs = append(errs, errors.New("tunnel.executable is required"))
	}
	if strings.TrimSpace(c.Tunnel.TunnelID) == "" {
		errs = append(errs, errors.New("tunnel.tunnelId is required"))
	}
	if strings.TrimSpace(c.Tunnel.APIKeyRef) == "" {
		errs = append(errs, errors.New("tunnel.apiKeyRef is required"))
	}
	if strings.TrimSpace(c.Tunnel.MCPServerURL) == "" {
		errs = append(errs, errors.New("tunnel.mcpServerUrl is required"))
	}
	if c.Supervisor.MinBackoff.Duration() <= 0 {
		errs = append(errs, errors.New("supervisor.minBackoff must be > 0"))
	}
	if c.Supervisor.MaxBackoff.Duration() < c.Supervisor.MinBackoff.Duration() {
		errs = append(errs, errors.New("supervisor.maxBackoff must be >= minBackoff"))
	}
	if c.Supervisor.StableWindow.Duration() <= 0 {
		errs = append(errs, errors.New("supervisor.stableWindow must be > 0"))
	}
	if c.Supervisor.HealthInterval.Duration() <= 0 {
		errs = append(errs, errors.New("supervisor.healthInterval must be > 0"))
	}
	if c.Supervisor.HealthFailureThreshold <= 0 {
		errs = append(errs, errors.New("supervisor.healthFailureThreshold must be > 0"))
	}
	if c.Supervisor.ShutdownTimeout.Duration() <= 0 {
		errs = append(errs, errors.New("supervisor.shutdownTimeout must be > 0"))
	}
	return errors.Join(errs...)
}

func (c Config) TunnelArgs() []string {
	args := []string{
		"run",
		"--control-plane.tunnel-id", c.Tunnel.TunnelID,
		"--control-plane.api-key", c.Tunnel.APIKeyRef,
		"--mcp.server-url", c.Tunnel.MCPServerURL,
	}
	if c.Tunnel.MCPAuthorizationRef != "" {
		header := "Authorization: " + c.Tunnel.MCPAuthorizationRef
		args = append(args,
			"--mcp.extra-headers", header,
			"--mcp.discovery-extra-headers", header,
		)
	}
	if c.Tunnel.OrganizationID != "" {
		args = append(args, "--control-plane.organization-id", c.Tunnel.OrganizationID)
	}
	args = append(args,
		"--mcp.startup-wait-timeout", c.Tunnel.StartupWaitTimeout.Duration().String(),
		"--health.listen-addr", "127.0.0.1:0",
		"--health.url-file", c.Tunnel.HealthURLFile,
		"--log.format", "json",
	)
	return append(args, c.Tunnel.ExtraArgs...)
}
