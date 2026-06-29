package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestHTTPSyncDownloadsArchiveAndImports(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/wes/.claude/projects/test-project/session.jsonl": testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "http remote").
			String(),
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer remote-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"dirs":{"claude":["/home/wes/.claude/projects"]}}`))
		case "/api/v1/remote-sync/archive":
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	stats, err := HTTPSync{
		Host:  "devbox",
		URL:   ts.URL,
		Token: "remote-token",
		DB:    database,
	}.Run(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
}

func TestHTTPSyncReportsDownloadAndImportProgress(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/wes/.claude/projects/test-project/session.jsonl": testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "http remote progress").
			String(),
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"dirs":{"claude":["/home/wes/.claude/projects"]}}`))
		case "/api/v1/remote-sync/archive":
			w.Header().Set("Content-Type", "application/x-tar")
			w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	var progress []syncpkg.Progress

	stats, err := HTTPSync{
		Host:  "devbox",
		URL:   ts.URL,
		Token: "remote-token",
		DB:    database,
		Progress: func(p syncpkg.Progress) {
			progress = append(progress, p)
		},
	}.Run(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	assert.Contains(t, progressDetails(progress),
		"Resolving agent directories on devbox")
	assert.Contains(t, progressDetails(progress),
		"Downloading session archive from devbox")
	assert.Contains(t, progressDetails(progress),
		"Extracting session archive from devbox")
	assert.Contains(t, progressDetails(progress),
		"Processing sessions from devbox")
	require.NotEmpty(t, progress, "expected progress events")
	assert.Equal(t, int64(len(archive)), maxBytesDone(progress))
	assert.Equal(t, int64(len(archive)), maxBytesTotal(progress))
}

func progressDetails(progress []syncpkg.Progress) []string {
	out := make([]string, 0, len(progress))
	for _, p := range progress {
		if p.Detail != "" {
			out = append(out, p.Detail)
		}
	}
	return out
}

func maxBytesDone(progress []syncpkg.Progress) int64 {
	var max int64
	for _, p := range progress {
		if p.BytesDone > max {
			max = p.BytesDone
		}
	}
	return max
}

func maxBytesTotal(progress []syncpkg.Progress) int64 {
	var max int64
	for _, p := range progress {
		if p.BytesTotal > max {
			max = p.BytesTotal
		}
	}
	return max
}

func buildHTTPTestTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	for name, body := range files {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: mtime,
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}
