package tunnel

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/benice2me11/codexify-go/internal/config"
)

const (
	ClientVersion      = "0.0.12"
	releaseBase        = "https://github.com/openai/tunnel-client/releases/download/v0.0.12"
	maxDownloadBytes   = 100 * 1024 * 1024
	maxBinaryBytes     = 64 * 1024 * 1024
	installManifestVer = 1
)

var installMu sync.Mutex

type releaseAsset struct {
	ArchiveName   string
	BinaryName    string
	ArchiveSHA256 string
}

type installManifest struct {
	Version             int    `json:"version"`
	TunnelClientVersion string `json:"tunnelClientVersion"`
	Asset               string `json:"asset"`
	ArchiveSHA256       string `json:"archiveSha256"`
	BinarySHA256        string `json:"binarySha256"`
}

type RuntimeStatus struct {
	Managed   bool   `json:"managed"`
	Version   string `json:"version"`
	Path      string `json:"path"`
	Installed bool   `json:"installed"`
	Verified  bool   `json:"verified"`
	Detail    string `json:"detail,omitempty"`
}

func ResolveExecutable(ctx context.Context, cfg config.TunnelConfig) (string, error) {
	if explicit := strings.TrimSpace(cfg.Executable); explicit != "" {
		return explicit, nil
	}
	return EnsureManaged(ctx, cfg.ManagedDir)
}

func EnsureManaged(ctx context.Context, base string) (string, error) {
	installMu.Lock()
	defer installMu.Unlock()

	asset, err := currentReleaseAsset()
	if err != nil {
		return "", err
	}
	binaryPath, manifestPath, err := managedPaths(base, asset)
	if err != nil {
		return "", err
	}
	binaryExists := fileExists(binaryPath)
	manifestExists := fileExists(manifestPath)
	switch {
	case binaryExists && manifestExists:
		if err := validateManagedInstall(ctx, binaryPath, manifestPath, asset); err != nil {
			return "", err
		}
		return binaryPath, nil
	case binaryExists != manifestExists:
		return "", fmt.Errorf("incomplete managed tunnel runtime under %s; remove that version directory and retry", filepath.Dir(binaryPath))
	}

	if err := installManaged(ctx, binaryPath, manifestPath, asset); err != nil {
		return "", err
	}
	return binaryPath, nil
}

func ManagedStatus(ctx context.Context, base string) RuntimeStatus {
	status := RuntimeStatus{Managed: true, Version: ClientVersion}
	asset, err := currentReleaseAsset()
	if err != nil {
		status.Detail = err.Error()
		return status
	}
	binaryPath, manifestPath, err := managedPaths(base, asset)
	if err != nil {
		status.Detail = err.Error()
		return status
	}
	status.Path = binaryPath
	status.Installed = fileExists(binaryPath) && fileExists(manifestPath)
	if !status.Installed {
		status.Detail = "managed tunnel runtime is not installed"
		return status
	}
	if err := validateManagedInstall(ctx, binaryPath, manifestPath, asset); err != nil {
		status.Detail = err.Error()
		return status
	}
	status.Verified = true
	return status
}

func ValidateClient(ctx context.Context, path string, exactVersion string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("tunnel runtime does not exist: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("tunnel runtime is not a regular file")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("tunnel runtime version check failed: %w: %s", err, sanitizeOutput(out))
	}
	versionText := string(out)
	if exactVersion != "" && !strings.Contains(versionText, exactVersion) {
		return fmt.Errorf("managed tunnel runtime reports unexpected version: %s", sanitizeOutput(out))
	}
	helpCtx, helpCancel := context.WithTimeout(ctx, 10*time.Second)
	defer helpCancel()
	help, err := exec.CommandContext(helpCtx, path, "run", "--help").CombinedOutput()
	if err != nil {
		return fmt.Errorf("tunnel runtime compatibility check failed: %w: %s", err, sanitizeOutput(help))
	}
	required := []string{
		"--control-plane.tunnel-id",
		"--mcp.server-url",
		"--mcp.extra-headers",
		"--mcp.discovery-extra-headers",
		"--health.url-file",
	}
	helpText := string(help)
	for _, flag := range required {
		if !strings.Contains(helpText, flag) {
			return fmt.Errorf("tunnel runtime is missing required flag %s", flag)
		}
	}
	return nil
}

func installManaged(ctx context.Context, binaryPath, manifestPath string, asset releaseAsset) error {
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o700); err != nil {
		return err
	}
	archiveURL := releaseBase + "/" + asset.ArchiveName
	archive, err := fetchRelease(ctx, archiveURL)
	if err != nil {
		return err
	}
	archiveHash := sha256Hex(archive)
	if archiveHash != asset.ArchiveSHA256 {
		return errors.New("OpenAI tunnel runtime archive does not match the pinned SHA-256")
	}
	binary, err := extractBinary(archive, asset.BinaryName)
	if err != nil {
		return err
	}
	if err := atomicWrite(binaryPath, binary, 0o700); err != nil {
		return err
	}
	if err := ValidateClient(ctx, binaryPath, ClientVersion); err != nil {
		_ = os.Remove(binaryPath)
		return err
	}
	manifest := installManifest{
		Version:             installManifestVer,
		TunnelClientVersion: ClientVersion,
		Asset:               asset.ArchiveName,
		ArchiveSHA256:       archiveHash,
		BinarySHA256:        sha256Hex(binary),
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = os.Remove(binaryPath)
		return err
	}
	if err := atomicWrite(manifestPath, append(data, '\n'), 0o600); err != nil {
		_ = os.Remove(binaryPath)
		return err
	}
	return nil
}

func validateManagedInstall(ctx context.Context, binaryPath, manifestPath string, asset releaseAsset) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read managed tunnel manifest: %w", err)
	}
	var manifest installManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse managed tunnel manifest: %w", err)
	}
	if manifest.Version != installManifestVer ||
		manifest.TunnelClientVersion != ClientVersion ||
		manifest.Asset != asset.ArchiveName ||
		manifest.ArchiveSHA256 != asset.ArchiveSHA256 {
		return errors.New("managed tunnel manifest does not match this codexify-go build")
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("read managed tunnel runtime: %w", err)
	}
	if sha256Hex(binary) != manifest.BinarySHA256 {
		return errors.New("managed tunnel runtime failed its integrity check")
	}
	return ValidateClient(ctx, binaryPath, ClientVersion)
}

func fetchRelease(ctx context.Context, rawURL string) ([]byte, error) {
	client := &http.Client{
		Timeout: 120 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" || !allowedReleaseHost(req.URL.Hostname()) {
				return errors.New("unexpected tunnel release redirect")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download OpenAI tunnel runtime: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download OpenAI tunnel runtime: HTTP %s", resp.Status)
	}
	if resp.ContentLength > maxDownloadBytes {
		return nil, errors.New("OpenAI tunnel runtime download exceeds 100 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownloadBytes {
		return nil, errors.New("OpenAI tunnel runtime download exceeds 100 MiB")
	}
	return data, nil
}

func allowedReleaseHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "github.com" ||
		host == "release-assets.githubusercontent.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

func extractBinary(archive []byte, binaryName string) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open tunnel runtime ZIP: %w", err)
	}
	for _, entry := range reader.File {
		if entry.Name != binaryName {
			continue
		}
		if entry.FileInfo().IsDir() || entry.UncompressedSize64 > maxBinaryBytes {
			return nil, errors.New("tunnel runtime ZIP binary is not a bounded regular file")
		}
		rc, err := entry.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(rc, maxBinaryBytes+1))
		closeErr := rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(data) > maxBinaryBytes {
			return nil, errors.New("tunnel runtime binary exceeds 64 MiB")
		}
		return data, nil
	}
	return nil, fmt.Errorf("release ZIP does not contain %s", binaryName)
}

func currentReleaseAsset() (releaseAsset, error) {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "darwin"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
	case "arm64":
	default:
		return releaseAsset{}, fmt.Errorf("no pinned tunnel runtime for architecture %s", runtime.GOARCH)
	}
	var hash string
	switch osName + "/" + arch {
	case "darwin/amd64":
		hash = "ca05df2ab5397065fcf4b1e2e8ec330d9ad0d7a880a1f08265b36fc69eddd391"
	case "darwin/arm64":
		hash = "924c7a1e0a2ea2c10f4f72b9c2e2382e7d55443831cdf8d84e394a54e83ccc30"
	case "linux/amd64":
		hash = "31e9ece3f54f87126813fb206d465fd86b23462cc71734a787927b818f60d931"
	case "linux/arm64":
		hash = "f02bc770367e328f21614841eb27393d7f023256224a6dde31c8aa4d6dc763f5"
	case "windows/amd64":
		hash = "0721098f9edda72cc36f938adcb12cd6a0c49c6c0be7ad6ab6e412f966585f2e"
	case "windows/arm64":
		hash = "952a30d469df749c88722e70441e72c541aa9ad878ab082678533f64bd31b2a9"
	default:
		return releaseAsset{}, fmt.Errorf("no pinned tunnel runtime for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	archiveName := fmt.Sprintf("tunnel-client-runtime-v%s-%s-%s.zip", ClientVersion, osName, arch)
	binaryName := "tunnel-client-runtime"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	return releaseAsset{ArchiveName: archiveName, BinaryName: binaryName, ArchiveSHA256: hash}, nil
}

func managedPaths(base string, asset releaseAsset) (string, string, error) {
	if strings.TrimSpace(base) == "" {
		return "", "", errors.New("managed tunnel directory is empty")
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(abs, "v"+ClientVersion)
	return filepath.Join(dir, asset.BinaryName), filepath.Join(dir, "manifest.json"), nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sanitizeOutput(data []byte) string {
	value := strings.TrimSpace(string(data))
	if len(value) > 2000 {
		value = value[:2000]
	}
	return value
}
