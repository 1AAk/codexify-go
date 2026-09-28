package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAcceptsUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := []byte(`{"version":1,"tunnel":{"executable":"tunnel.exe","tunnelId":"tunnel_test","apiKeyRef":"env:KEY","mcpServerUrl":"http://127.0.0.1:3300/mcp/test"},"supervisor":{"minBackoff":"1s","maxBackoff":"2s","stableWindow":"1s","healthInterval":"1s","healthFailureThreshold":1,"shutdownTimeout":"1s"}}`)
	data := append([]byte{0xEF, 0xBB, 0xBF}, body...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMakesRelativePathsConfigRelativeAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := []byte(`{"version":1,"log":{"file":"logs/app.log"},"tunnel":{"executable":"bin/tunnel.exe","tunnelId":"tunnel_test","apiKeyRef":"file:secrets/key.txt","mcpServerUrl":"http://127.0.0.1:3300/mcp/test","healthUrlFile":"run/health.url"},"supervisor":{"minBackoff":"1s","maxBackoff":"2s","stableWindow":"1s","healthInterval":"1s","healthFailureThreshold":1,"shutdownTimeout":"1s"}}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"log":    cfg.Log.File,
		"tunnel": cfg.Tunnel.Executable,
		"health": cfg.Tunnel.HealthURLFile,
	} {
		if !filepath.IsAbs(got) {
			t.Fatalf("%s path is not absolute: %q", name, got)
		}
	}
	wantRef := "file:" + filepath.Join(dir, "secrets", "key.txt")
	if cfg.Tunnel.APIKeyRef != wantRef {
		t.Fatalf("api key ref got %q want %q", cfg.Tunnel.APIKeyRef, wantRef)
	}
}
