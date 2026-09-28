package tunnel

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCurrentReleaseAssetIsPinned(t *testing.T) {
	asset, err := currentReleaseAsset()
	if err != nil {
		t.Fatal(err)
	}
	if asset.ArchiveSHA256 == "" || len(asset.ArchiveSHA256) != 64 {
		t.Fatalf("bad pinned hash: %#v", asset)
	}
	if !strings.Contains(asset.ArchiveName, "v"+ClientVersion) {
		t.Fatalf("archive=%q", asset.ArchiveName)
	}
	if runtime.GOOS == "windows" && asset.BinaryName != "tunnel-client-runtime.exe" {
		t.Fatalf("binary=%q", asset.BinaryName)
	}
}

func TestExtractBinaryUsesExactArchiveMember(t *testing.T) {
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	other, _ := writer.Create("nested/tunnel-client-runtime.exe")
	_, _ = other.Write([]byte("wrong"))
	wantName := "tunnel-client-runtime.exe"
	want, _ := writer.Create(wantName)
	_, _ = want.Write([]byte("expected"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := extractBinary(buf.Bytes(), wantName)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "expected" {
		t.Fatalf("got %q", got)
	}
}

func TestManagedInstallRejectsManifestOrBinaryTamperingBeforeExecution(t *testing.T) {
	asset, err := currentReleaseAsset()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	binary, manifestPath, err := managedPaths(base, asset)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("not executable and deliberately tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := installManifest{
		Version:             installManifestVer,
		TunnelClientVersion: ClientVersion,
		Asset:               asset.ArchiveName,
		ArchiveSHA256:       asset.ArchiveSHA256,
		BinarySHA256:        strings.Repeat("0", 64),
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = validateManagedInstall(context.Background(), binary, manifestPath, asset)
	if err == nil || !strings.Contains(err.Error(), "integrity check") {
		t.Fatalf("err=%v", err)
	}
}
