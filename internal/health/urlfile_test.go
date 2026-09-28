package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestURLFileChecker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "health.url")
	if err := os.WriteFile(path, []byte(server.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	checker := NewURLFileChecker(path)
	if err := checker.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestURLFileCheckerRejectsRemoteHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.url")
	if err := os.WriteFile(path, []byte("http://example.com"), 0o600); err != nil {
		t.Fatal(err)
	}
	checker := NewURLFileChecker(path)
	if err := checker.Check(context.Background()); err == nil {
		t.Fatal("expected remote health URL to be rejected")
	}
}
