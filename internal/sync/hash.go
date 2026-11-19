package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// ComputeHash returns the SHA-256 hex digest of data from r.
func ComputeHash(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ComputeFileHash returns the SHA-256 hex digest of the file at path.
func ComputeFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	hash, err := ComputeHash(f)
	if err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return hash, nil
}

// computeFileHashPrefix indirects ComputeFileHashPrefix so cardinality-scaling
// tests can count the source-content reads a sync pass performs *to decide
// freshness*. Only providerIncrementalContentChanged goes through it today.
//
// The incremental-append path also hashes (to refresh a stored fingerprint
// after consuming new bytes) and deliberately calls ComputeFileHashPrefix
// directly: that read is write-side work proportional to data that genuinely
// changed, not a per-session freshness probe, so counting it would blur the
// invariant TestWarmFullSyncDoesNotRehashClaudeArchive pins. Route a new
// freshness gate through this var; leave write-side hashing on the direct call.
var computeFileHashPrefix = ComputeFileHashPrefix

// contentCheckHash hashes a source prefix for a freshness gate and counts the
// bytes it read toward the changed-path pass summary.
func (e *Engine) contentCheckHash(path string, size int64) (string, error) {
	hash, err := computeFileHashPrefix(path, size)
	if err == nil {
		e.countContentCheck(size)
	}
	return hash, err
}

// countContentCheck records one source whose content a sync read to check
// it, and the bytes that read.
func (e *Engine) countContentCheck(size int64) {
	e.contentChecks.Add(1)
	e.contentCheckBytes.Add(size)
}

// ComputeFileHashPrefix returns the SHA-256 hex digest of the first size bytes
// of the file at path. It returns an error if the file is shorter than size.
func ComputeFileHashPrefix(path string, size int64) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("hashing %s: negative prefix size %d", path, size)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.CopyN(h, f, size); err != nil {
		return "", fmt.Errorf(
			"hashing first %d bytes of %s: %w", size, path, err,
		)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
