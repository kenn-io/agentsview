package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// defaultArchiveQueryPolicy builds an archiveQueryPolicy seeded with the
// defaults shared by the usage tests: skip the read-only daemon and refresh
// usage directly. mut may override any field; pass nil to use the defaults.
func defaultArchiveQueryPolicy(mut func(*archiveQueryPolicy)) archiveQueryPolicy {
	policy := archiveQueryPolicy{
		ReadOnlyDaemon:       archiveQuerySkipReadOnlyDaemon,
		DirectReadOnlyAction: "refresh usage directly",
	}
	if mut != nil {
		mut(&policy)
	}
	return policy
}

// resolveTestArchiveQueryBackend resolves an archive-query backend for policy,
// failing the test on error and registering the cleanup hook.
func resolveTestArchiveQueryBackend(
	t *testing.T, policy archiveQueryPolicy,
) archiveQueryBackend {
	t.Helper()
	backend, cleanup, err := resolveArchiveQueryBackend(context.Background(), policy)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return backend
}

// testDataDir creates an isolated data directory, exports it via
// AGENTSVIEW_DATA_DIR for the duration of the test, and returns the path.
func testDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	return dir
}

// captureOutput redirects an os.File-backed stream (os.Stdout or os.Stderr)
// into a buffer while fn runs and returns everything written. set installs the
// pipe writer and restore returns the original stream; both are supplied by the
// thin wrappers below so callers do not repeat the pipe/copy/restore plumbing.
func captureOutput(
	t *testing.T, set func(*os.File), restore func() *os.File, fn func(),
) string {
	t.Helper()

	orig := restore()
	r, w, err := os.Pipe()
	require.NoError(t, err, "pipe")
	set(w)
	t.Cleanup(func() { set(orig) })

	var buf bytes.Buffer
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(&buf, r)
		readDone <- err
	}()

	fn()

	require.NoError(t, w.Close(), "close pipe writer")
	set(orig)

	require.NoError(t, <-readDone, "read pipe")
	require.NoError(t, r.Close(), "close pipe reader")
	return buf.String()
}

// captureStdout returns everything fn writes to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureOutput(t,
		func(f *os.File) { os.Stdout = f },
		func() *os.File { return os.Stdout },
		fn,
	)
}

// captureStderr returns everything fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return captureOutput(t,
		func(f *os.File) { os.Stderr = f },
		func() *os.File { return os.Stderr },
		fn,
	)
}

// captureLogOutput redirects the standard logger into a buffer for the
// duration of the test and restores the previous writer on cleanup.
func captureLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// restoreTestLogOutput saves the global log writer and restores it on cleanup,
// closing any file-backed writer installed during the test first so TempDir
// cleanup can remove it on Windows.
func restoreTestLogOutput(t *testing.T) {
	t.Helper()
	orig := log.Writer()
	t.Cleanup(func() {
		if closer, ok := log.Writer().(io.Closer); ok {
			_ = closer.Close()
		}
		log.SetOutput(orig)
	})
}

// writeTestFile writes content to path, failing the test on error.
func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, content, 0o644), "write %s", path)
}

// requireSymlinkOrSkip creates a symlink from link to target, skipping the test
// when the platform or filesystem does not support symlinks.
func requireSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.ENOTSUP) {
		t.Skip("symlinks not supported:", err)
	}
	require.NoError(t, err, "symlink")
}
