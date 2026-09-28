package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/benice2me11/codexify-go/internal/config"
	"github.com/benice2me11/codexify-go/internal/workspace"
)

type FileParam struct {
	DownloadURL string `json:"download_url"`
	FileID      string `json:"file_id"`
	MIMEType    string `json:"mime_type,omitempty"`
	FileName    string `json:"file_name,omitempty"`
}

type Receipt struct {
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	Source   string `json:"source"`
	MIMEType string `json:"mimeType,omitempty"`
}

type Downloader struct {
	cfg           config.ArtifactIngressConfig
	sem           chan struct{}
	baseTransport *http.Transport
}

func New(cfg config.ArtifactIngressConfig) *Downloader {
	size := cfg.MaxConcurrentDownloads
	if size < 1 {
		size = 1
	}
	return &Downloader{cfg: cfg, sem: make(chan struct{}, size)}
}

func (d *Downloader) Import(ctx context.Context, root *workspace.Root, file FileParam, destination string) (Receipt, error) {
	if !d.cfg.Enabled {
		return Receipt{}, errors.New("artifact_ingress_disabled: native file ingress is disabled by configuration")
	}
	if root == nil {
		return Receipt{}, errors.New("artifact_ingress_invalid: active workspace is unavailable")
	}
	if err := validateFileParam(file); err != nil {
		return Receipt{}, err
	}
	parsed, err := url.Parse(file.DownloadURL)
	if err != nil {
		return Receipt{}, errors.New("untrusted_file_url: host-provided file URL is invalid")
	}
	if err := validateDownloadURL(ctx, parsed, d.cfg.AllowedHosts, true); err != nil {
		return Receipt{}, err
	}
	rel, target, err := prepareDestination(root, destination)
	if err != nil {
		return Receipt{}, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, d.cfg.RequestTimeout.Duration())
	defer cancel()
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-requestCtx.Done():
		return Receipt{}, errors.New("file_import_timed_out: native file ingress remained at its concurrency/request deadline")
	}

	client := d.client()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return Receipt{}, errors.New("file_download_failed: native file request could not be created")
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return Receipt{}, fmt.Errorf("file_download_failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Receipt{}, fmt.Errorf("file_download_failed: file service returned HTTP %d", resp.StatusCode)
	}
	if raw := resp.Header.Get("Content-Length"); raw != "" {
		length, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || length < 0 {
			return Receipt{}, errors.New("file_download_failed: response contained an invalid content length")
		}
		if length > d.cfg.MaxFileBytes {
			return Receipt{}, fmt.Errorf("file_too_large: native file exceeds the configured %d byte limit", d.cfg.MaxFileBytes)
		}
	}

	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Receipt{}, fmt.Errorf("destination_unsafe: %w", err)
	}
	revalidated, err := root.Resolve(filepath.FromSlash(rel), true)
	if err != nil || !samePath(revalidated, target) {
		return Receipt{}, errors.New("destination_unsafe: destination changed while preparing import")
	}
	if _, err := os.Lstat(target); err == nil {
		return Receipt{}, fmt.Errorf("destination_exists: %s already exists", rel)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Receipt{}, errors.New("destination_unsafe: destination could not be inspected safely")
	}

	partial, err := os.CreateTemp(parent, ".codexify-import-*.partial")
	if err != nil {
		return Receipt{}, fmt.Errorf("write_failed: %w", err)
	}
	partialPath := partial.Name()
	_ = partial.Chmod(0o600)
	published := false
	defer func() {
		_ = partial.Close()
		if !published {
			_ = os.Remove(partialPath)
		}
	}()

	hasher := sha256.New()
	count, err := copyWithIdleTimeout(requestCtx, partial, hasher, resp.Body, d.cfg.MaxFileBytes, d.cfg.IdleTimeout.Duration())
	if err != nil {
		return Receipt{}, err
	}
	if err := partial.Sync(); err != nil {
		return Receipt{}, fmt.Errorf("write_failed: %w", err)
	}
	if err := partial.Close(); err != nil {
		return Receipt{}, fmt.Errorf("write_failed: %w", err)
	}

	revalidated, err = root.Resolve(filepath.FromSlash(rel), true)
	if err != nil || !samePath(revalidated, target) {
		return Receipt{}, errors.New("destination_unsafe: active workspace/destination changed before publication")
	}
	if _, err := os.Lstat(target); err == nil {
		return Receipt{}, fmt.Errorf("destination_exists: %s already exists", rel)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Receipt{}, errors.New("destination_unsafe: destination could not be inspected before publication")
	}

	if err := os.Link(partialPath, target); err != nil {
		if _, statErr := os.Lstat(target); statErr == nil {
			return Receipt{}, fmt.Errorf("destination_exists: %s already exists", rel)
		}
		return Receipt{}, fmt.Errorf("write_failed: atomic publication failed: %w", err)
	}
	_ = os.Remove(partialPath)
	published = true

	mimeType := file.MIMEType
	if mimeType == "" {
		mimeType = resp.Header.Get("Content-Type")
	}
	return Receipt{
		Path:     filepath.ToSlash(rel),
		Bytes:    count,
		SHA256:   "sha256:" + hex.EncodeToString(hasher.Sum(nil)),
		Source:   "openai_file",
		MIMEType: mimeType,
	}, nil
}

func validateFileParam(file FileParam) error {
	if !validMetadata(file.FileID, 512) {
		return errors.New("invalid_file_reference: host-provided file ID is invalid")
	}
	if file.MIMEType != "" && !validMetadata(file.MIMEType, 255) {
		return errors.New("invalid_file_reference: host-provided MIME type is invalid")
	}
	if file.FileName != "" && !validMetadata(file.FileName, 1024) {
		return errors.New("invalid_file_reference: host-provided filename is invalid")
	}
	if strings.TrimSpace(file.DownloadURL) == "" {
		return errors.New("invalid_file_reference: host-provided file URL is missing")
	}
	return nil
}

func validMetadata(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func prepareDestination(root *workspace.Root, raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsRune(raw, 0) || strings.HasSuffix(raw, "/") || strings.HasSuffix(raw, "\\") || filepath.IsAbs(raw) {
		return "", "", errors.New("invalid_destination: destination must be a non-empty workspace-relative file path")
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", errors.New("invalid_destination: destination escapes the active workspace")
	}
	target, err := root.Resolve(clean, true)
	if err != nil {
		return "", "", fmt.Errorf("destination_unsafe: %w", err)
	}
	return filepath.ToSlash(clean), target, nil
}

func (d *Downloader) client() *http.Client {
	transport := &http.Transport{}
	if d.baseTransport != nil {
		transport = d.baseTransport.Clone()
	}
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = d.cfg.IdleTimeout.Duration()
	transport.DialContext = d.dialContext
	redirects := 0
	return &http.Client{
		Transport: transport,
		Timeout:   d.cfg.RequestTimeout.Duration(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			redirects++
			if redirects > d.cfg.MaxRedirects {
				return errors.New("file_redirect_invalid: native file download exceeded the allowed redirect limit")
			}
			return validateDownloadURL(req.Context(), req.URL, d.cfg.AllowedHosts, true)
		},
	}
}

func (d *Downloader) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	match := classifyHost(host, d.cfg.AllowedHosts)
	if match == hostNone {
		return nil, errors.New("untrusted_file_url: host is outside configured allowlist")
	}
	dialer := &net.Dialer{Timeout: minDuration(d.cfg.IdleTimeout.Duration(), 30*time.Second)}
	if match == hostExplicit {
		return dialer.DialContext(ctx, network, address)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("untrusted_file_url: wildcard host could not be resolved safely")
	}
	for _, ip := range ips {
		if ipIsInternal(ip.IP) {
			return nil, errors.New("untrusted_file_url: wildcard host resolves to an internal/private address")
		}
	}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

type hostMatch int

const (
	hostNone hostMatch = iota
	hostWildcard
	hostExplicit
)

func validateDownloadURL(ctx context.Context, u *url.URL, allowed []string, resolve bool) error {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Hostname() == "" {
		return errors.New("untrusted_file_url: host-provided file URL is outside the configured native-file allowlist")
	}
	match := classifyHost(u.Hostname(), allowed)
	if match == hostNone {
		return errors.New("untrusted_file_url: host-provided file URL is outside the configured native-file allowlist")
	}
	if match == hostExplicit {
		return nil
	}
	if port := u.Port(); port != "" && port != "443" {
		return errors.New("untrusted_file_url: wildcard URLs must use standard HTTPS port")
	}
	host := u.Hostname()
	if hostIsInternal(host) {
		return errors.New("untrusted_file_url: wildcard host is internal/private")
	}
	if resolve && net.ParseIP(strings.Trim(host, "[]")) == nil {
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return fmt.Errorf("untrusted_file_url: host resolution failed: %w", err)
		}
		for _, ip := range ips {
			if ipIsInternal(ip.IP) {
				return errors.New("untrusted_file_url: wildcard host resolves to an internal/private address")
			}
		}
	}
	return nil
}

func classifyHost(host string, allowed []string) hostMatch {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	wildcard := false
	for _, raw := range allowed {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case pattern == "*":
			wildcard = true
		case strings.HasPrefix(pattern, "."):
			bare := strings.TrimPrefix(pattern, ".")
			if host == bare || strings.HasSuffix(host, pattern) {
				return hostExplicit
			}
		case host == pattern:
			return hostExplicit
		}
	}
	if wildcard {
		return hostWildcard
	}
	return hostNone
}

func hostIsInternal(host string) bool {
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipIsInternal(ip)
	}
	return false
}

func ipIsInternal(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() || v4.IsUnspecified() ||
			(v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127) ||
			(v4[0] == 192 && v4[1] == 0 && v4[2] == 2) ||
			(v4[0] == 198 && v4[1] == 51 && v4[2] == 100) ||
			(v4[0] == 203 && v4[1] == 0 && v4[2] == 113)
	}
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		(len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc)
}

type readChunk struct {
	data []byte
	err  error
}

func copyWithIdleTimeout(ctx context.Context, dst io.Writer, hash io.Writer, src io.ReadCloser, max int64, idle time.Duration) (int64, error) {
	chunks := make(chan readChunk, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				chunks <- readChunk{data: data}
			}
			if err != nil {
				chunks <- readChunk{err: err}
				return
			}
		}
	}()

	timer := time.NewTimer(idle)
	defer timer.Stop()
	var total int64
	for {
		select {
		case <-ctx.Done():
			_ = src.Close()
			return 0, fmt.Errorf("file_import_timed_out: %w", ctx.Err())
		case <-timer.C:
			_ = src.Close()
			return 0, errors.New("file_download_failed: native file stream exceeded idle timeout")
		case chunk := <-chunks:
			if len(chunk.data) > 0 {
				total += int64(len(chunk.data))
				if total > max {
					_ = src.Close()
					return 0, fmt.Errorf("file_too_large: native file exceeds configured %d byte limit", max)
				}
				if _, err := dst.Write(chunk.data); err != nil {
					return 0, fmt.Errorf("write_failed: %w", err)
				}
				if _, err := hash.Write(chunk.data); err != nil {
					return 0, err
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			}
			if chunk.err != nil {
				if errors.Is(chunk.err, io.EOF) {
					return total, nil
				}
				return 0, fmt.Errorf("file_download_failed: %w", chunk.err)
			}
		}
	}
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
