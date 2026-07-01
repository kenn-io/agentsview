package server

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func newRemoteSyncServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	claudeDir := filepath.Join(dir, "claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	sessionPath := filepath.Join(claudeDir, "session.jsonl")
	require.NoError(t, os.WriteFile(sessionPath, []byte("{}\n"), 0o644))
	mtime := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(sessionPath, mtime, mtime))

	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		DataDir:      dir,
		DBPath:       dbPath,
		AuthToken:    "remote-token",
		RequireAuth:  false,
		WriteTimeout: 30 * time.Second,
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
	}, database, nil)
	return srv, srv.Handler(), sessionPath
}

func TestRemoteSyncTargetsRequiresBearerAndBypassesHostCheck(t *testing.T) {
	_, handler, _ := newRemoteSyncServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote-sync/targets", nil)
	req.Host = "devbox.tailnet.ts.net:8080"
	req.Header.Set("Authorization", "Bearer remote-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "claude")
}

func TestRemoteSyncArchiveRejectsUnresolvedPath(t *testing.T) {
	_, handler, _ := newRemoteSyncServer(t)
	body := bytes.NewBufferString(`{"dirs":{"claude":["/etc"]}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive", body)
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoteSyncArchiveStreamsTar(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	targets := map[string]any{
		"dirs": map[string][]string{
			"claude": {filepath.Dir(sessionPath)},
		},
	}
	payload, err := json.Marshal(targets)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "application/x-tar", w.Header().Get("Content-Type"))
	tr := tar.NewReader(bytes.NewReader(w.Body.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			require.FailNow(t, "session file not found in tar")
		}
		require.NoError(t, err)
		if pathBaseSlash(hdr.Name) == filepath.Base(sessionPath) {
			assert.Equal(t, byte(tar.TypeReg), hdr.Typeflag)
			return
		}
	}
}

func pathBaseSlash(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

func TestRemoteSyncArchiveDoesNotAppendErrorAfterStreamingStarts(t *testing.T) {
	srv, _, sessionPath := newRemoteSyncServer(t)
	targets := map[string]any{
		"dirs": map[string][]string{
			"claude": {filepath.Dir(sessionPath)},
		},
	}
	payload, err := json.Marshal(targets)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := &errorOnFirstWriteRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}

	srv.remoteSyncArchiveHTTP(w, req)

	assert.NotContains(t, w.Body.String(), "forced tar write error")
}

type errorOnFirstWriteRecorder struct {
	*httptest.ResponseRecorder
	failed bool
}

func (w *errorOnFirstWriteRecorder) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if !w.failed {
		w.failed = true
		return n, errors.New("forced tar write error")
	}
	return n, err
}
