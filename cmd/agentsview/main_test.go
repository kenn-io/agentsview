package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestMustLoadConfig(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantHost      string
		wantPort      int
		wantPublicURL string
		wantProxyMode string
	}{
		{
			name:          "DefaultArgs",
			args:          []string{},
			wantHost:      "127.0.0.1",
			wantPort:      8080,
			wantPublicURL: "",
			wantProxyMode: "",
		},
		{
			name:          "ExplicitFlags",
			args:          []string{"--host", "0.0.0.0", "--port", "9090", "--public-url", "https://viewer.example.test", "--proxy", "caddy", "--proxy-bind-host", "10.0.60.2", "--public-port", "9443", "--no-browser"},
			wantHost:      "0.0.0.0",
			wantPort:      9090,
			wantPublicURL: "https://viewer.example.test:9443",
			wantProxyMode: "caddy",
		},
		{
			name:          "PartialFlags",
			args:          []string{"--port", "3000"},
			wantHost:      "127.0.0.1",
			wantPort:      3000,
			wantPublicURL: "",
			wantProxyMode: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
			cmd := newServeCommand()
			require.NoError(t, cmd.Flags().Parse(tt.args), "Parse")
			cfg := mustLoadConfig(cmd)

			assert.Equal(t, tt.wantHost, cfg.Host)
			assert.Equal(t, tt.wantPort, cfg.Port)
			assert.Equal(t, tt.wantPublicURL, cfg.PublicURL)
			assert.Equal(t, tt.wantProxyMode, cfg.Proxy.Mode)

			assert.NotEmpty(t, cfg.DataDir, "DataDir should be set")
			wantDBPath := filepath.Join(cfg.DataDir, "sessions.db")
			assert.Equal(t, wantDBPath, cfg.DBPath)
		})
	}
}

func TestPrepareServeRuntimeConfigPortZeroUsesAssignedPort(t *testing.T) {
	cfg := config.Config{
		Host: "127.0.0.1",
		Port: 0,
	}

	var err error
	out := captureStdout(t, func() {
		cfg, err = prepareServeRuntimeConfig(
			cfg,
			serveRuntimeOptions{
				Mode:          "serve",
				RequestedPort: 0,
			},
		)
	})
	require.NoError(t, err, "prepareServeRuntimeConfig")
	assert.NotZero(t, cfg.Port, "Port remained literal 0")
	assert.NotContains(t, out, "Port 0 in use",
		"unexpected literal port 0 fallback message")
	assert.Contains(t, out, "Using available port",
		"missing ephemeral port message")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err, "pipe")
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = orig
	})

	fn()

	require.NoError(t, w.Close(), "close stdout pipe writer")
	os.Stdout = orig

	data, err := io.ReadAll(r)
	require.NoError(t, err, "read stdout pipe")
	require.NoError(t, r.Close(), "close stdout pipe reader")
	return string(data)
}

func TestSetupLogFile(t *testing.T) {
	origOutput := log.Writer()

	dir := t.TempDir()
	setupLogFile(dir)

	// Close the log file before TempDir cleanup removes the
	// directory. On Windows, open files can't be deleted.
	// Registered after TempDir so LIFO ordering runs this first.
	t.Cleanup(func() {
		if c, ok := log.Writer().(io.Closer); ok {
			c.Close()
		}
		log.SetOutput(origOutput)
	})

	// Log something and verify it reaches the file.
	log.Print("test-log-message")

	logPath := filepath.Join(dir, "debug.log")
	data, err := os.ReadFile(logPath)
	require.NoError(t, err, "reading log file")
	assert.Contains(t, string(data), "test-log-message",
		"log file missing message")
}

func TestSetupLogFileOpenFailure(t *testing.T) {
	origOutput := log.Writer()
	t.Cleanup(func() { log.SetOutput(origOutput) })

	// Capture log output to verify warning is emitted.
	var buf bytes.Buffer
	log.SetOutput(io.MultiWriter(origOutput, &buf))

	// Pass a path that can't be opened (dir doesn't exist
	// and we use a file as the "dir").
	tmpFile := filepath.Join(t.TempDir(), "notadir")
	os.WriteFile(tmpFile, []byte("x"), 0o644)

	setupLogFile(tmpFile)

	assert.Contains(t, buf.String(), "cannot open log file",
		"expected warning about log file")
}

func TestTruncateLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// Write a file larger than the limit.
	big := bytes.Repeat([]byte("x"), 1024)
	os.WriteFile(path, big, 0o644)

	// Truncate with limit smaller than file size.
	truncateLogFile(path, 512)

	info, err := os.Stat(path)
	require.NoError(t, err, "stat after truncate")
	assert.Equal(t, int64(0), info.Size())
}

func TestTruncateLogFileUnderLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	content := []byte("small log content")
	os.WriteFile(path, content, 0o644)

	// File is under limit: should not be truncated.
	truncateLogFile(path, 1024)

	data, err := os.ReadFile(path)
	require.NoError(t, err, "read after truncate")
	assert.Equal(t, string(content), string(data), "content changed")
}

func TestTruncateLogFileMissing(t *testing.T) {
	// Non-existent file: should not panic.
	missing := filepath.Join(t.TempDir(), "missing", "log.txt")
	truncateLogFile(missing, 1024)
}

func TestTruncateLogFileSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.log")
	link := filepath.Join(dir, "link.log")

	// Write a target file larger than the limit.
	big := bytes.Repeat([]byte("x"), 1024)
	require.NoError(t, os.WriteFile(target, big, 0o644), "write target")
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, syscall.EPERM) ||
			errors.Is(err, syscall.EACCES) ||
			errors.Is(err, os.ErrPermission) ||
			errors.Is(err, syscall.ENOSYS) ||
			errors.Is(err, syscall.ENOTSUP) {
			t.Skip("symlinks not supported:", err)
		}
		t.Fatalf("symlink: %v", err)
	}

	// Truncate via symlink: should be a no-op.
	truncateLogFile(link, 512)

	data, err := os.ReadFile(target)
	require.NoError(t, err, "read target")
	assert.Len(t, data, 1024, "symlink target was truncated")
}

type fakeUnwatchedPollSyncer struct {
	lastSyncStartedAt time.Time
	roots             []string
	since             time.Time
	calls             int
}

func (f *fakeUnwatchedPollSyncer) LastSyncStartedAt() time.Time {
	return f.lastSyncStartedAt
}

func (f *fakeUnwatchedPollSyncer) SyncRootsSince(
	ctx context.Context, roots []string, since time.Time,
	onProgress sync.ProgressFunc,
) sync.SyncStats {
	f.calls++
	f.roots = append([]string(nil), roots...)
	f.since = since
	return sync.SyncStats{}
}

func TestPollUnwatchedRootsOnceUsesScopedIncrementalSync(t *testing.T) {
	lastSyncStartedAt := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	fake := &fakeUnwatchedPollSyncer{
		lastSyncStartedAt: lastSyncStartedAt,
	}
	roots := []string{"/tmp/claude", "/tmp/codex"}

	pollUnwatchedRootsOnce(fake, roots)

	assert.Equal(t, 1, fake.calls)
	assert.Equal(t, roots, fake.roots)
	assert.Equal(t, lastSyncStartedAt.Add(-unwatchedPollSafetyMargin), fake.since)
}

func TestCollectWatchRootsPreservesDirsSharingWatchRoot(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "codex-state")
	require.NoError(t, os.Mkdir(parent, 0o755), "mkdir parent")

	sessionsDir := filepath.Join(parent, "sessions")
	archivedDir := filepath.Join(parent, "archived_sessions")
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {sessionsDir, archivedDir},
		},
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	require.Empty(t, unwatchedDirs, "unwatched dirs before watcher setup")
	require.Len(t, roots, 1, "shared watch root should be represented once")
	assert.Equal(t, parent, roots[0].root)
	assert.ElementsMatch(t, []string{sessionsDir, archivedDir}, roots[0].dirs)
}

func TestResyncCoversSignals(t *testing.T) {
	tests := []struct {
		name     string
		stats    sync.SyncStats
		fellBack bool
		want     bool
	}{
		{
			name:  "clean resync no orphans covers signals",
			stats: sync.SyncStats{Synced: 5},
			want:  true,
		},
		{
			name: "fell back to incremental sync needs backfill",
			stats: sync.SyncStats{
				Synced: 2, Aborted: true,
			},
			fellBack: true,
			want:     false,
		},
		{
			name: "orphans copied need backfill",
			stats: sync.SyncStats{
				Synced: 5, OrphanedCopied: 3,
			},
			want: false,
		},
		{
			name: "orphans copied even with fallback false",
			stats: sync.SyncStats{
				Synced: 0, OrphanedCopied: 1,
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resyncCoversSignals(tc.stats, tc.fellBack)
			assert.Equal(t, tc.want, got)
		})
	}
}
