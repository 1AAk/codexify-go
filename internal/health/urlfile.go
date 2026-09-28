package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type URLFileChecker struct {
	Path   string
	Client *http.Client
}

type HTTPChecker struct {
	URL    string
	Client *http.Client
}

func NewHTTPChecker(rawURL string) *HTTPChecker {
	return &HTTPChecker{
		URL: rawURL,
		Client: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
}

func (c *HTTPChecker) Check(ctx context.Context) error {
	u, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("parse health url: %w", err)
	}
	if u.Scheme != "http" {
		return errors.New("health url must use http")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("health url must be loopback, got %q", host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("health returned %s", resp.Status)
	}
	return nil
}

func NewURLFileChecker(path string) *URLFileChecker {
	return &URLFileChecker{
		Path: path,
		Client: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
}

func (c *URLFileChecker) Check(ctx context.Context) error {
	data, err := os.ReadFile(c.Path)
	if err != nil {
		return fmt.Errorf("read health url: %w", err)
	}
	base := strings.TrimSpace(string(data))
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("parse health url: %w", err)
	}
	if u.Scheme != "http" {
		return errors.New("health url must use http")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("health url must be loopback, got %q", host)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/readyz"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("readyz returned %s", resp.Status)
	}
	return nil
}
