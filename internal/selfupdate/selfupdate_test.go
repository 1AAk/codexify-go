package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseWorkflowMatchesUpdaterAssetContract(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "release.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		`archive="codexify-go-v${VERSION}-${GOOS_TARGET}-${GOARCH_TARGET}.zip"`,
		`archive="codexify-go-v${VERSION}-${GOOS_TARGET}-${GOARCH_TARGET}.tar.gz"`,
		`tar -czf "../$archive" "$binary"`,
		`legacy_archive="codexify-go-v${VERSION}-${GOOS_TARGET}-${GOARCH_TARGET}.zip"`,
		"checksums.txt",
		`(cd dist && sha256sum "$archive" > "$archive.sha256")`,
		"softprops/action-gh-release@v3",
		"overwrite_files: true",
		"goos: windows",
		"goos: linux",
		"goos: darwin",
		"goarch: amd64",
		"goarch: arm64",
		"dist/*.zip",
		"dist/*.tar.gz",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("release workflow missing %q", want)
		}
	}
	if strings.Contains(text, `sha256sum "dist/$archive"`) {
		t.Fatal("release workflow must not embed the staging directory in checksums.txt")
	}
	if strings.Contains(text, `if [[ "$VERSION" == "0.8.2" ]]`) {
		t.Fatal("Unix ZIP compatibility must not expire after one release because old updaters only inspect releases/latest")
	}
}

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

func TestChecksumForAcceptsStagingDirectoryPrefix(t *testing.T) {
	hash := strings.Repeat("c", 64)
	data := []byte(hash + "  dist/artifact.zip\n")
	got, err := checksumFor(data, "artifact.zip")
	if err != nil {
		t.Fatal(err)
	}
	if got != hash {
		t.Fatalf("hash=%q", got)
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

func TestReleaseNamesByPlatform(t *testing.T) {
	tests := []struct {
		goos    string
		goarch  string
		archive string
		binary  string
	}{
		{"windows", "amd64", "codexify-go-v1.2.3-windows-amd64.zip", "codexify-go.exe"},
		{"windows", "arm64", "codexify-go-v1.2.3-windows-arm64.zip", "codexify-go.exe"},
		{"darwin", "amd64", "codexify-go-v1.2.3-darwin-amd64.tar.gz", "codexify-go"},
		{"darwin", "arm64", "codexify-go-v1.2.3-darwin-arm64.tar.gz", "codexify-go"},
		{"linux", "amd64", "codexify-go-v1.2.3-linux-amd64.tar.gz", "codexify-go"},
		{"linux", "arm64", "codexify-go-v1.2.3-linux-arm64.tar.gz", "codexify-go"},
	}
	for _, tt := range tests {
		t.Run(tt.goos+"_"+tt.goarch, func(t *testing.T) {
			archive, binary, err := releaseNamesFor("1.2.3", tt.goos, tt.goarch)
			if err != nil {
				t.Fatal(err)
			}
			if archive != tt.archive || binary != tt.binary {
				t.Fatalf("archive=%q binary=%q", archive, binary)
			}
		})
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
	got, err := extractBinary(buf.Bytes(), "artifact.zip", "codexify-go.exe")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary" {
		t.Fatalf("got=%q", got)
	}
	if _, err := extractBinary(buf.Bytes(), "artifact.zip", "missing.exe"); err == nil {
		t.Fatal("expected missing binary to fail")
	}
}

func TestExtractBinaryTarGzip(t *testing.T) {
	archive := tarGzipArchive(t, tar.Header{Name: "codexify-go", Mode: 0o755, Size: int64(len("binary")), Typeflag: tar.TypeReg}, []byte("binary"))
	got, err := extractBinary(archive, "artifact.tar.gz", "codexify-go")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary" {
		t.Fatalf("got=%q", got)
	}
}

func TestExtractBinaryTarGzipRejectsNonRegularExpectedEntry(t *testing.T) {
	archive := tarGzipArchive(t, tar.Header{Name: "codexify-go", Linkname: "/tmp/evil", Typeflag: tar.TypeSymlink}, nil)
	if _, err := extractBinary(archive, "artifact.tar.gz", "codexify-go"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected non-regular entry rejection, got %v", err)
	}
}

func tarGzipArchive(t *testing.T, header tar.Header, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if len(data) > 0 {
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
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
