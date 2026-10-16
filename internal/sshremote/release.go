package sshremote

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/atomicfile"
)

const defaultReleaseBaseURL = "https://github.com/kenn-io/agentsview/releases/download"

var errReleaseNotFound = errors.New("no published release for this version")

type platform struct {
	OS   string
	Arch string
}

func (p platform) String() string { return p.OS + "/" + p.Arch }

func localPlatform() platform { return platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// parsePlatform maps `uname -s` and `uname -m` to a release platform.
func parsePlatform(unameS, unameM string) (platform, error) {
	var p platform
	switch unameS {
	case "Linux":
		p.OS = "linux"
	case "Darwin":
		p.OS = "darwin"
	}
	switch unameM {
	case "x86_64", "amd64":
		p.Arch = "amd64"
	case "aarch64", "arm64":
		p.Arch = "arm64"
	}
	if p.OS == "" || p.Arch == "" {
		return platform{}, fmt.Errorf(
			"remote platform %s %s is not supported for SSH sync; install agentsview on the remote",
			unameS, unameM)
	}
	return p, nil
}

// releaseFetcher downloads and verifies release CLI tarballs.
type releaseFetcher struct {
	BaseURL  string
	Client   *http.Client
	CacheDir string // <data_dir>/ssh-binaries
}

func releaseFileName(version string, p platform) string {
	return fmt.Sprintf("agentsview_%s_%s_%s.tar.gz", version, p.OS, p.Arch)
}

// Fetch returns the path of a verified tarball for version (without "v").
// A cached SHA256SUMS is trusted; a cached tarball is re-verified against it.
// cacheDirLocks serializes fetches that share a release cache directory in
// this process. One download then serves every waiter, and no fetch opens a
// file while another replaces it, which Windows refuses.
// Each lock is a one-slot channel, so a waiter whose sync is cancelled stops
// waiting instead of blocking behind another host's download.
var cacheDirLocks sync.Map

func lockCacheDir(ctx context.Context, dir string) (unlock func(), err error) {
	value, _ := cacheDirLocks.LoadOrStore(dir, make(chan struct{}, 1))
	slot := value.(chan struct{})
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f releaseFetcher) Fetch(ctx context.Context, version string, p platform) (string, error) {
	dir := filepath.Join(f.CacheDir, version)
	unlock, err := lockCacheDir(ctx, dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create release cache: %w", err)
	}
	name := releaseFileName(version, p)
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	if _, err := os.Stat(sumsPath); errors.Is(err, os.ErrNotExist) {
		tmp, err := f.download(ctx, version, "SHA256SUMS", dir)
		if err != nil {
			return "", err
		}
		if err := atomicfile.Replace(tmp, sumsPath); err != nil {
			_ = os.Remove(tmp)
			// Windows refuses to replace a file another fetch has open. That
			// fetch stored the same release's SHA256SUMS, so use its copy.
			if _, statErr := os.Stat(sumsPath); statErr != nil {
				return "", fmt.Errorf("store SHA256SUMS: %w", err)
			}
		}
	}
	want, err := checksumFor(sumsPath, name)
	if err != nil {
		return "", err
	}
	tarball := filepath.Join(dir, name)
	if got, err := fileSHA256(tarball); err == nil && got == want {
		return tarball, nil
	}
	tmp, err := f.download(ctx, version, name, dir)
	if err != nil {
		return "", err
	}
	got, err := fileSHA256(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if got != want {
		_ = os.Remove(tmp)
		return "", fmt.Errorf(
			"release %s checksum mismatch: got %s, SHA256SUMS lists %s", name, got, want)
	}
	if err := atomicfile.Replace(tmp, tarball); err != nil {
		_ = os.Remove(tmp)
		// As for SHA256SUMS: a concurrent fetch may hold the stored tarball
		// open. Its bytes are the verified release, so use it.
		if stored, hashErr := fileSHA256(tarball); hashErr != nil || stored != want {
			return "", fmt.Errorf("store release tarball: %w", err)
		}
	}
	return tarball, nil
}

// download fetches <BaseURL>/v<version>/<name> into a new temp file in dir and
// returns its path. Each call gets its own file, so concurrent fetches of one
// release never share a partial body; the caller renames or removes it.
func (f releaseFetcher) download(ctx context.Context, version, name, dir string) (string, error) {
	base := f.BaseURL
	if base == "" {
		base = defaultReleaseBaseURL
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	url := fmt.Sprintf("%s/v%s/%s", strings.TrimRight(base, "/"), version, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build release request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: %s", errReleaseNotFound, url)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", fmt.Errorf("create temp file for %s: %w", name, err)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	return tmp.Name(), nil
}

// checksumFor returns the hex digest SHA256SUMS lists for name.
func checksumFor(path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open SHA256SUMS: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		sum, file, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "  ")
		if ok && file == name {
			return sum, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read SHA256SUMS: %w", err)
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s", name)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
