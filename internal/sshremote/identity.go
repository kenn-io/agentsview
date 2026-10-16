package sshremote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	releaseVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)
	unsafeIdentityChars   = regexp.MustCompile(`[^0-9A-Za-z._+-]`)
)

// LocalBuild describes the collector binary that will provision remotes.
type LocalBuild struct {
	Version    string
	Executable string
}

// ReleaseID returns the identity of the official release binary for this
// version. Release tags are immutable, so the version alone names the bytes.
func (b LocalBuild) ReleaseID() (string, bool) {
	if !releaseVersionPattern.MatchString(b.Version) {
		return "", false
	}
	return "v" + b.releaseVersion(), true
}

// releaseVersion is the version without its leading "v", as used in
// release file names.
func (b LocalBuild) releaseVersion() string { return strings.TrimPrefix(b.Version, "v") }

// ContentID names the exact local executable. Development builds share
// version strings, so the hash keeps a rebuilt binary from reusing an old one.
func (b LocalBuild) ContentID() (string, error) {
	sum, err := executableHash(b.Executable)
	if err != nil {
		return "", err
	}
	version := unsafeIdentityChars.ReplaceAllString(b.Version, "_")
	if version == "" {
		version = "unknown"
	}
	return version + "-" + sum[:16], nil
}

type hashKey struct {
	path  string
	size  int64
	mtime time.Time
}

var (
	hashCacheMu sync.Mutex
	hashCache   = map[hashKey]string{}
)

// executableHash hashes path once per size and mtime.
func executableHash(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat local executable: %w", err)
	}
	key := hashKey{path: path, size: info.Size(), mtime: info.ModTime()}
	hashCacheMu.Lock()
	defer hashCacheMu.Unlock()
	if sum, ok := hashCache[key]; ok {
		return sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open local executable: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash local executable: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	hashCache[key] = sum
	return sum, nil
}
