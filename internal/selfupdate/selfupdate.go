package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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

	"golang.org/x/mod/semver"
)

const (
	repository       = "benice2me11/codexify-go"
	latestReleaseURL = "https://api.github.com/repos/" + repository + "/releases/latest"
	maxMetadataBytes = 1 * 1024 * 1024
	maxArchiveBytes  = 100 * 1024 * 1024
	maxBinaryBytes   = 64 * 1024 * 1024
)

type Inspection struct {
	Status         string `json:"status"`
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion,omitempty"`
	Source         string `json:"source,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

type Prepared struct {
	CurrentVersion string
	TargetVersion  string
	BinaryPath     string
	ArchiveName    string
	SHA256         string
}

type release struct {
	TagName    string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type cachedInspection struct {
	at     time.Time
	result Inspection
}

var (
	cacheMu sync.Mutex
	cache   *cachedInspection
)

func Inspect(ctx context.Context, current string, force bool) Inspection {
	now := time.Now()
	cacheMu.Lock()
	if !force && cache != nil {
		ttl := 5 * time.Minute
		if cache.result.Status == "unknown" {
			ttl = 30 * time.Second
		}
		if now.Sub(cache.at) < ttl && cache.result.CurrentVersion == current {
			result := cache.result
			cacheMu.Unlock()
			return result
		}
	}
	cacheMu.Unlock()

	result := inspectFresh(ctx, current)
	cacheMu.Lock()
	cache = &cachedInspection{at: now, result: result}
	cacheMu.Unlock()
	return result
}

func inspectFresh(ctx context.Context, current string) Inspection {
	result := Inspection{Status: "unknown", CurrentVersion: current, Source: "github_api"}
	currentSemver, err := normalizedVersion(current)
	if err != nil {
		result.Detail = "running version is not valid semver: " + err.Error()
		return result
	}
	rel, err := latestRelease(ctx)
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	latest, err := normalizedVersion(rel.TagName)
	if err != nil {
		result.Detail = "latest release tag is invalid: " + err.Error()
		return result
	}
	result.LatestVersion = strings.TrimPrefix(latest, "v")
	switch semver.Compare(latest, currentSemver) {
	case 1:
		result.Status = "update_available"
	case 0:
		result.Status = "up_to_date"
	default:
		result.Status = "ahead_of_latest"
	}
	return result
}

func Prepare(ctx context.Context, current, baseDir string) (Prepared, error) {
	currentSemver, err := normalizedVersion(current)
	if err != nil {
		return Prepared{}, err
	}
	rel, err := latestRelease(ctx)
	if err != nil {
		return Prepared{}, err
	}
	latest, err := normalizedVersion(rel.TagName)
	if err != nil {
		return Prepared{}, err
	}
	if semver.Compare(latest, currentSemver) <= 0 {
		return Prepared{}, fmt.Errorf("no newer release: current=%s latest=%s", strings.TrimPrefix(currentSemver, "v"), strings.TrimPrefix(latest, "v"))
	}
	target := strings.TrimPrefix(latest, "v")
	archiveName, binaryName, err := releaseNames(target)
	if err != nil {
		return Prepared{}, err
	}
	archiveURL := assetURL(rel, archiveName)
	checksumsURL := assetURL(rel, "checksums.txt")
	if archiveURL == "" || checksumsURL == "" {
		return Prepared{}, fmt.Errorf("release %s is missing %s or checksums.txt", rel.TagName, archiveName)
	}

	checksums, err := fetch(ctx, checksumsURL, maxMetadataBytes)
	if err != nil {
		return Prepared{}, fmt.Errorf("download release checksums: %w", err)
	}
	expected, err := checksumFor(checksums, archiveName)
	if err != nil {
		return Prepared{}, err
	}
	archive, err := fetch(ctx, archiveURL, maxArchiveBytes)
	if err != nil {
		return Prepared{}, fmt.Errorf("download release archive: %w", err)
	}
	actual := sha256Hex(archive)
	if actual != expected {
		return Prepared{}, errors.New("release archive SHA-256 does not match checksums.txt")
	}
	binary, err := extractBinary(archive, archiveName, binaryName)
	if err != nil {
		return Prepared{}, err
	}
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil || home == "" {
			return Prepared{}, errors.New("cannot resolve user home for update staging")
		}
		baseDir = filepath.Join(home, ".codexify-go", "updates")
	}
	dir := filepath.Join(baseDir, "v"+target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Prepared{}, err
	}
	targetPath := filepath.Join(dir, binaryName)
	if err := atomicWrite(targetPath, binary, 0o700); err != nil {
		return Prepared{}, err
	}
	if err := validatePrepared(ctx, targetPath, target); err != nil {
		_ = os.Remove(targetPath)
		return Prepared{}, err
	}
	return Prepared{
		CurrentVersion: strings.TrimPrefix(currentSemver, "v"),
		TargetVersion:  target,
		BinaryPath:     targetPath,
		ArchiveName:    archiveName,
		SHA256:         sha256Hex(binary),
	}, nil
}

func latestRelease(ctx context.Context) (release, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "codexify-go")
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return release{}, fmt.Errorf("latest release check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return release{}, errors.New("no GitHub release is published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("latest release check returned HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes+1))
	if err != nil {
		return release{}, err
	}
	if len(data) > maxMetadataBytes {
		return release{}, errors.New("latest release metadata exceeded 1 MiB")
	}
	var rel release
	if err := json.Unmarshal(data, &rel); err != nil {
		return release{}, err
	}
	if rel.Draft || rel.Prerelease || strings.TrimSpace(rel.TagName) == "" {
		return release{}, errors.New("latest release metadata is not a stable release")
	}
	return rel, nil
}

func fetch(ctx context.Context, rawURL string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "codexify-go")
	client := &http.Client{
		Timeout: 120 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" || !allowedGitHubDownloadHost(req.URL.Hostname()) {
				return errors.New("unexpected update download redirect")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	if resp.ContentLength > max {
		return nil, errors.New("download exceeds configured size limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("download exceeds configured size limit")
	}
	return data, nil
}

func releaseNames(version string) (string, string, error) {
	return releaseNamesFor(version, runtime.GOOS, runtime.GOARCH)
}

func releaseNamesFor(version, osName, arch string) (string, string, error) {
	switch osName {
	case "windows", "linux":
	case "darwin":
	default:
		return "", "", fmt.Errorf("self-update release has no target for OS %s", osName)
	}
	if arch != "amd64" && arch != "arm64" {
		return "", "", fmt.Errorf("self-update release has no target for architecture %s", arch)
	}
	binary := "codexify-go"
	if osName == "windows" {
		binary += ".exe"
		return fmt.Sprintf("codexify-go-v%s-%s-%s.zip", version, osName, arch), binary, nil
	}
	return fmt.Sprintf("codexify-go-v%s-%s-%s.tar.gz", version, osName, arch), binary, nil
}

func assetURL(rel release, name string) string {
	for _, asset := range rel.Assets {
		if asset.Name == name {
			return asset.BrowserDownloadURL
		}
	}
	return ""
}

func checksumFor(data []byte, filename string) (string, error) {
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hash := strings.ToLower(strings.TrimSpace(fields[0]))
		name := strings.TrimPrefix(strings.TrimSpace(fields[len(fields)-1]), "*")
		normalizedName := strings.ReplaceAll(name, "\\", "/")
		if (normalizedName == filename || strings.HasSuffix(normalizedName, "/"+filename)) && len(hash) == 64 && isHex(hash) {
			return hash, nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no SHA-256 for %s", filename)
}

func extractBinary(archive []byte, archiveName, binaryName string) ([]byte, error) {
	switch {
	case strings.HasSuffix(archiveName, ".zip"):
		return extractZipBinary(archive, binaryName)
	case strings.HasSuffix(archiveName, ".tar.gz"):
		return extractTarGzipBinary(archive, binaryName)
	default:
		return nil, fmt.Errorf("unsupported release archive format: %s", archiveName)
	}
}

func extractZipBinary(archive []byte, binaryName string) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, file := range reader.File {
		if file.Name != binaryName {
			continue
		}
		if file.FileInfo().IsDir() || file.UncompressedSize64 > maxBinaryBytes {
			return nil, errors.New("release binary is not a bounded regular file")
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(rc, maxBinaryBytes+1))
		_ = rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		if len(data) > maxBinaryBytes {
			return nil, errors.New("release binary exceeds 64 MiB")
		}
		return data, nil
	}
	return nil, fmt.Errorf("release archive does not contain %s", binaryName)
}

func extractTarGzipBinary(archive []byte, binaryName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Name != binaryName {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, errors.New("release binary is not a regular file")
		}
		if header.Size < 0 || header.Size > maxBinaryBytes {
			return nil, errors.New("release binary is not a bounded regular file")
		}
		data, err := io.ReadAll(io.LimitReader(reader, maxBinaryBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxBinaryBytes {
			return nil, errors.New("release binary exceeds 64 MiB")
		}
		return data, nil
	}
	return nil, fmt.Errorf("release archive does not contain %s", binaryName)
}

func validatePrepared(ctx context.Context, path, version string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("updated binary version probe failed: %w", err)
	}
	if !strings.Contains(string(output), version) {
		return fmt.Errorf("updated binary reported unexpected version: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func normalizedVersion(value string) (string, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "v"))
	if value == "" {
		return "", errors.New("empty version")
	}
	v := "v" + value
	if !semver.IsValid(v) {
		return "", fmt.Errorf("%q", value)
	}
	return v, nil
}

func allowedGitHubDownloadHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "github.com" ||
		host == "release-assets.githubusercontent.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
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
	tmp := file.Name()
	defer os.Remove(tmp)
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
	return os.Rename(tmp, path)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isHex(value string) bool {
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}
