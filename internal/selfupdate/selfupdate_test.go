package selfupdate

import (
	"archive/zip"
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizedVersion(t *testing.T) {
	for _, input := range []string{"0.7.0", "v0.7.0", "0.7.0-dev"} {
		if _, err := normalizedVersion(input); err != nil {
			t.Fatalf("%q: %v", input, err)
		}
	}
	if _, err := normalizedVersion("dev"); err == nil {
		t.Fatal("expected invalid version to fail")
	}
}

func TestChecksumFor(t *testing.T) {
	hash := strings.Repeat("a", 64)
	data := []byte(hash + "  artifact.zip\r\n" + strings.Repeat("b", 64) + " *other.zip\n")
	got, err := checksumFor(data, "artifact.zip")
	if err != nil {
		t.Fatal(err)
	}
	if got != hash {
		t.Fatalf("hash=%q", got)
	}
	if _, err := checksumFor(data, "missing.zip"); err == nil {
		t.Fatal("expected missing checksum to fail")
	}
}

func TestReleaseNamesCurrentPlatform(t *testing.T) {
	archive, binary, err := releaseNames("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(archive, "codexify-go-v1.2.3-") || !strings.Contains(archive, runtime.GOARCH) {
		t.Fatalf("archive=%q", archive)
	}
	if runtime.GOOS == "windows" && binary != "codexify-go.exe" {
		t.Fatalf("binary=%q", binary)
	}
}

func TestExtractBinary(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("codexify-go.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("binary")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := extractBinary(buf.Bytes(), "codexify-go.exe")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary" {
		t.Fatalf("got=%q", got)
	}
	if _, err := extractBinary(buf.Bytes(), "missing.exe"); err == nil {
		t.Fatal("expected missing binary to fail")
	}
}

func TestAllowedGitHubDownloadHost(t *testing.T) {
	for _, host := range []string{"github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"} {
		if !allowedGitHubDownloadHost(host) {
			t.Fatalf("%s should be allowed", host)
		}
	}
	for _, host := range []string{"github.com.evil.example", "localhost", "example.com"} {
		if allowedGitHubDownloadHost(host) {
			t.Fatalf("%s should be rejected", host)
		}
	}
}
