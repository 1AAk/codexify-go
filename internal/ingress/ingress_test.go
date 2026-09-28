package ingress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/workspace"
)

func TestImportExplicitHTTPSHostAndNoOverwrite(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("attached body\n"))
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.AllowedHosts = []string{u.Hostname()}
	downloader := New(cfg)
	downloader.baseTransport = server.Client().Transport.(*http.Transport).Clone()

	dir := t.TempDir()
	root, err := workspace.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	file := FileParam{
		DownloadURL: server.URL + "/object?signature=secret",
		FileID:      "file-123",
		MIMEType:    "text/plain",
		FileName:    "payload.txt",
	}
	receipt, err := downloader.Import(context.Background(), root, file, "imports/payload.txt")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Path != "imports/payload.txt" || receipt.Source != "openai_file" || receipt.Bytes != int64(len("attached body\n")) {
		t.Fatalf("receipt=%+v", receipt)
	}
	if !strings.HasPrefix(receipt.SHA256, "sha256:") || len(receipt.SHA256) != len("sha256:")+64 {
		t.Fatalf("sha=%q", receipt.SHA256)
	}
	data, err := os.ReadFile(filepath.Join(dir, "imports", "payload.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "attached body\n" {
		t.Fatalf("data=%q", data)
	}

	if _, err := downloader.Import(context.Background(), root, file, "imports/payload.txt"); err == nil || !strings.Contains(err.Error(), "destination_exists") {
		t.Fatalf("expected no-overwrite rejection, got %v", err)
	}
}

func TestWildcardRejectsInternalHosts(t *testing.T) {
	for _, raw := range []string{
		"https://127.0.0.1/file",
		"https://10.0.0.1/file",
		"https://169.254.169.254/latest/meta-data",
		"https://localhost/file",
		"https://[::1]/file",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateDownloadURL(context.Background(), u, []string{"*"}, false); err == nil {
			t.Fatalf("expected %s to be blocked", raw)
		}
	}
}

func TestExplicitHostCanIntentionallyAllowInternal(t *testing.T) {
	u, _ := url.Parse("https://127.0.0.1:9443/file")
	if err := validateDownloadURL(context.Background(), u, []string{"127.0.0.1"}, false); err != nil {
		t.Fatalf("explicit internal host should be allowed: %v", err)
	}
}

func TestRedirectIsRevalidated(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(server.URL, "127.0.0.1", "localhost", 1)+"/next", http.StatusFound)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	cfg := testConfig()
	cfg.AllowedHosts = []string{u.Hostname()}
	downloader := New(cfg)
	downloader.baseTransport = server.Client().Transport.(*http.Transport).Clone()

	root, _ := workspace.New(t.TempDir())
	_, err := downloader.Import(context.Background(), root, FileParam{
		DownloadURL: server.URL + "/start",
		FileID:      "file-redirect",
	}, "payload.bin")
	if err == nil || !strings.Contains(err.Error(), "untrusted_file_url") {
		t.Fatalf("expected redirect policy rejection, got %v", err)
	}
}

func TestSizeLimitAndTraversal(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "64")
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	cfg := testConfig()
	cfg.MaxFileBytes = 16
	cfg.AllowedHosts = []string{u.Hostname()}
	downloader := New(cfg)
	downloader.baseTransport = server.Client().Transport.(*http.Transport).Clone()
	root, _ := workspace.New(t.TempDir())

	file := FileParam{DownloadURL: server.URL + "/large", FileID: "file-large"}
	if _, err := downloader.Import(context.Background(), root, file, "large.bin"); err == nil || !strings.Contains(err.Error(), "file_too_large") {
		t.Fatalf("expected size rejection, got %v", err)
	}
	if _, err := downloader.Import(context.Background(), root, file, "../escape.bin"); err == nil || !strings.Contains(err.Error(), "invalid_destination") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
}

func testConfig() config.ArtifactIngressConfig {
	return config.ArtifactIngressConfig{
		Enabled:                true,
		MaxFileBytes:           1 << 20,
		RequestTimeout:         config.Duration(5 * time.Second),
		IdleTimeout:            config.Duration(2 * time.Second),
		MaxRedirects:           3,
		MaxConcurrentDownloads: 2,
		AllowedHosts:           []string{"*"},
	}
}
