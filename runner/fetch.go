package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// installCurrentHarnessBinary makes the wrapper under ~/.local/bin the one
// the bridge serves now, and returns its path. With no copy on disk it
// downloads one. With a copy, it asks the bridge with If-Modified-Since set
// to the copy's modification time, which is the bridge file's Last-Modified
// from the previous download: 304 keeps the copy, 200 replaces it. Before
// 2026-10-14 the runner downloaded a wrapper only when it was missing, so
// SSDawn ran an April wrapper long after the bridge had moved on (todo
// 10e10d90).
func installCurrentHarnessBinary(serverURL string, harness msg.Harness) (string, error) {
	short := msg.HarnessShortName(harness)
	if short == "" {
		return "", fmt.Errorf("unknown harness type: %s", harness)
	}

	endpoint, err := buildBinaryURL(serverURL, short)
	if err != nil {
		return "", err
	}

	dest, err := harnessBinaryPath(harness)
	if err != nil {
		return "", err
	}
	_, err = installCurrentBinary(endpoint, dest)
	return dest, err
}

// installCurrentBinary makes dest the file the bridge serves at endpoint,
// asking with If-Modified-Since set to dest's modification time, and reports
// whether it replaced dest. A downloaded file takes the bridge's
// Last-Modified as its modification time, so the next ask means "newer than
// what I have?".
func installCurrentBinary(endpoint, dest string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return false, fmt.Errorf("create bin dir: %w", err)
	}

	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	if current, err := os.Stat(dest); err == nil {
		request.Header.Set("If-Modified-Since", current.ModTime().UTC().Format(http.TimeFormat))
	}
	httpClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpClient.Do(request)
	if err != nil {
		return false, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return false, fmt.Errorf("download %s: HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Write to a sibling tmp file then rename, so a crashed download
	// doesn't leave a half-written binary on PATH.
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return false, fmt.Errorf("download body: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return false, err
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return false, err
	}
	// The copy's modification time is the bridge file's, so the next
	// If-Modified-Since asks "newer than what I have?".
	lastModified, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		os.Remove(tmpPath)
		return false, fmt.Errorf("the bridge sent %s without a usable Last-Modified (%q), so a later update could not be detected", endpoint, resp.Header.Get("Last-Modified"))
	}
	if err := os.Chtimes(tmpPath, lastModified, lastModified); err != nil {
		os.Remove(tmpPath)
		return false, err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return false, fmt.Errorf("install %s: %w", dest, err)
	}
	return true, nil
}

// harnessBinaryPath returns the canonical install path for a harness
// wrapper binary on this runner. ~/.local/bin so it doesn't need root
// and lands inside the user's PATH on most distros.
func harnessBinaryPath(harness msg.Harness) (string, error) {
	bin := msg.HarnessBinaryName(harness)
	if bin == "" {
		return "", fmt.Errorf("unknown harness type: %s", harness)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", bin), nil
}

// buildBinaryURL composes the canonical /api/runner/binary URL for a
// short harness name (e.g. "claudecode") on this runner's GOOS/GOARCH.
func buildBinaryURL(serverURL, shortName string) (string, error) {
	return buildBinaryURLFor(serverURL, shortName, runtime.GOOS, runtime.GOARCH)
}

// buildBinaryURLFor is buildBinaryURL for another system: the Windows
// computer-use tool a runner in WSL installs, for one.
func buildBinaryURLFor(serverURL, shortName, goos, goarch string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/runner/binary"
	q := u.Query()
	q.Set("name", shortName)
	q.Set("os", goos)
	q.Set("arch", goarch)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
