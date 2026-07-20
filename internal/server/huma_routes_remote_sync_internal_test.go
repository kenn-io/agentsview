package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
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

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
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

func TestRemoteSyncHermesSessionlessProfileCannotExposeCredentials(t *testing.T) {
	dir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "test.db"))
	profileRoot := filepath.Join(dir, ".hermes", "profiles", "empty")
	require.NoError(t, os.MkdirAll(profileRoot, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(profileRoot, ".env"), []byte("TOKEN=secret\n"), 0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(profileRoot, "auth.json"), []byte(`{"token":"secret"}`), 0o600,
	))
	srv := New(config.Config{
		Host:        "127.0.0.1",
		Port:        8080,
		DataDir:     dir,
		DBPath:      filepath.Join(dir, "test.db"),
		AuthToken:   "remote-token",
		RequireAuth: false,
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {profileRoot},
		},
	}, database, nil)
	handler := srv.Handler()

	targetReq := httptest.NewRequest(http.MethodGet, "/api/v1/remote-sync/targets", nil)
	targetReq.Header.Set("Authorization", "Bearer remote-token")
	targetW := httptest.NewRecorder()
	handler.ServeHTTP(targetW, targetReq)
	require.Equal(t, http.StatusOK, targetW.Code, "body: %s", targetW.Body.String())
	var targets remotesync.TargetSet
	require.NoError(t, json.Unmarshal(targetW.Body.Bytes(), &targets))
	assert.NotContains(t, targets.Dirs, parser.AgentHermes)
	assert.Empty(t, targets.ExtraFiles)

	payload, err := json.Marshal(remotesync.TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentHermes: {profileRoot}},
	})
	require.NoError(t, err)
	archiveReq := httptest.NewRequest(
		http.MethodPost, "/api/v1/remote-sync/archive", bytes.NewReader(payload),
	)
	archiveReq.Header.Set("Authorization", "Bearer remote-token")
	archiveReq.Header.Set("Content-Type", "application/json")
	archiveW := httptest.NewRecorder()
	handler.ServeHTTP(archiveW, archiveReq)

	assert.Equal(t, http.StatusForbidden, archiveW.Code)
	assert.NotContains(t, archiveW.Body.String(), "secret")
}

func TestRemoteSyncHermesFlatCustomRootStreamsTranscripts(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	root := filepath.Join(dir, "custom", "hermes-archive")
	require.NoError(t, os.MkdirAll(root, 0o755))
	sessionPath := filepath.Join(root, "child.jsonl")
	require.NoError(t, os.WriteFile(sessionPath, []byte("{}\n"), 0o644))
	srv := New(config.Config{
		Host:        "127.0.0.1",
		Port:        8080,
		DataDir:     dir,
		DBPath:      dbPath,
		AuthToken:   "remote-token",
		RequireAuth: false,
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {root},
		},
	}, database, nil)
	handler := srv.Handler()

	targetReq := httptest.NewRequest(http.MethodGet, "/api/v1/remote-sync/targets", nil)
	targetReq.Header.Set("Authorization", "Bearer remote-token")
	targetW := httptest.NewRecorder()
	handler.ServeHTTP(targetW, targetReq)
	require.Equal(t, http.StatusOK, targetW.Code, "body: %s", targetW.Body.String())
	var targets remotesync.TargetSet
	require.NoError(t, json.Unmarshal(targetW.Body.Bytes(), &targets))
	require.Equal(t, []string{root}, targets.Dirs[parser.AgentHermes])

	payload, err := json.Marshal(targets)
	require.NoError(t, err)
	archiveReq := httptest.NewRequest(
		http.MethodPost, "/api/v1/remote-sync/archive", bytes.NewReader(payload),
	)
	archiveReq.Header.Set("Authorization", "Bearer remote-token")
	archiveReq.Header.Set("Content-Type", "application/json")
	archiveW := httptest.NewRecorder()
	handler.ServeHTTP(archiveW, archiveReq)

	require.Equal(t, http.StatusOK, archiveW.Code, "body: %s", archiveW.Body.String())
	assert.True(t, hasTarEntrySuffix(tarEntries(t, archiveW.Body.Bytes()), "child.jsonl"))
}

func TestRemoteSyncHermesSidecarRemovalRaces(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, http.Handler, remotesync.TargetSet, string, func())
	}{
		{
			name: "between targets and manifest",
			run: func(
				t *testing.T, handler http.Handler, targets remotesync.TargetSet,
				_ string, removeWAL func(),
			) {
				removeWAL()
				payload, err := json.Marshal(targets)
				require.NoError(t, err)
				req := httptest.NewRequest(
					http.MethodPost, "/api/v1/remote-sync/manifest", bytes.NewReader(payload),
				)
				req.Header.Set("Authorization", "Bearer remote-token")
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			},
		},
		{
			name: "between manifest and archive",
			run: func(
				t *testing.T, handler http.Handler, targets remotesync.TargetSet,
				wal string, removeWAL func(),
			) {
				manifestPayload, err := json.Marshal(targets)
				require.NoError(t, err)
				manifestReq := httptest.NewRequest(
					http.MethodPost, "/api/v1/remote-sync/manifest",
					bytes.NewReader(manifestPayload),
				)
				manifestReq.Header.Set("Authorization", "Bearer remote-token")
				manifestReq.Header.Set("Content-Type", "application/json")
				manifestW := httptest.NewRecorder()
				handler.ServeHTTP(manifestW, manifestReq)
				require.Equal(t, http.StatusOK, manifestW.Code,
					"body: %s", manifestW.Body.String())

				removeWAL()
				archivePayload, err := json.Marshal(remotesync.ArchiveRequest{
					TargetSet:  targets,
					DeltaFiles: []string{wal},
				})
				require.NoError(t, err)
				archiveReq := httptest.NewRequest(
					http.MethodPost, "/api/v1/remote-sync/archive",
					bytes.NewReader(archivePayload),
				)
				archiveReq.Header.Set("Authorization", "Bearer remote-token")
				archiveReq.Header.Set("Content-Type", "application/json")
				archiveW := httptest.NewRecorder()
				handler.ServeHTTP(archiveW, archiveReq)
				assert.Equal(t, http.StatusOK, archiveW.Code,
					"body: %s", archiveW.Body.String())
				entries := tarEntries(t, archiveW.Body.Bytes())
				assert.True(t, hasTarEntrySuffix(entries, "state.db"))
				assert.False(t, hasTarEntrySuffix(entries, "state.db-wal"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, targets, wal, removeWAL := newHermesRemoteSyncServer(t)
			assert.Contains(t, targets.ExtraFiles, wal)
			tt.run(t, handler, targets, wal, removeWAL)
		})
	}
}

func newHermesRemoteSyncServer(
	t *testing.T,
) (http.Handler, remotesync.TargetSet, string, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	profileRoot := filepath.Join(dir, ".hermes", "profiles", "research")
	sessionsDir := filepath.Join(profileRoot, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionsDir, "session.json"), []byte(`{}`), 0o644,
	))
	stateDB := filepath.Join(profileRoot, "state.db")
	stateConn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	stateConn.SetMaxOpenConns(1)
	stateConnClosed := false
	t.Cleanup(func() {
		if !stateConnClosed {
			require.NoError(t, stateConn.Close())
		}
	})
	_, err = stateConn.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = stateConn.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
	_, err = stateConn.Exec(`PRAGMA wal_autocheckpoint=0`)
	require.NoError(t, err)
	_, err = stateConn.Exec(`INSERT INTO sessions (id) VALUES ('wal-session')`)
	require.NoError(t, err)
	wal := stateDB + "-wal"
	require.FileExists(t, wal)
	removeWAL := func() {
		_, err := stateConn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		require.NoError(t, err)
		require.NoError(t, stateConn.Close())
		stateConnClosed = true
		if err := os.Remove(wal); !os.IsNotExist(err) {
			require.NoError(t, err)
		}
	}
	srv := New(config.Config{
		Host:        "127.0.0.1",
		Port:        8080,
		DataDir:     dir,
		DBPath:      dbPath,
		AuthToken:   "remote-token",
		RequireAuth: false,
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {profileRoot},
		},
	}, database, nil)
	handler := srv.Handler()
	targetReq := httptest.NewRequest(http.MethodGet, "/api/v1/remote-sync/targets", nil)
	targetReq.Header.Set("Authorization", "Bearer remote-token")
	targetW := httptest.NewRecorder()
	handler.ServeHTTP(targetW, targetReq)
	require.Equal(t, http.StatusOK, targetW.Code, "body: %s", targetW.Body.String())
	var targets remotesync.TargetSet
	require.NoError(t, json.Unmarshal(targetW.Body.Bytes(), &targets))
	return handler, targets, wal, removeWAL
}

// newWindsurfRemoteSyncServer builds a remote-sync server whose only
// agent is Windsurf (a file-scoped, sanitized agent). It returns the
// handler, the resolved targets the client would see, and the raw
// state.vscdb path under the Windsurf root.
func newWindsurfRemoteSyncServer(t *testing.T) (http.Handler, remotesync.TargetSet, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	windsurfRoot := filepath.Join(dir, "Windsurf", "User")
	workspaceDir := filepath.Join(windsurfRoot, "workspaceStorage", "workspace-a")
	stateDB := filepath.Join(workspaceDir, parser.WindsurfStateDBName)
	workspaceJSON := filepath.Join(workspaceDir, "workspace.json")
	secretPath := filepath.Join(workspaceDir, "extension-secret.json")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	closeStateDB := writeWindsurfArchiveStateDB(t, stateDB)
	t.Cleanup(closeStateDB)
	require.NoError(t, os.WriteFile(workspaceJSON, []byte(`{"folder":"file:///work/demo"}`), 0o644))
	require.NoError(t, os.WriteFile(secretPath, []byte("do not archive"), 0o644))
	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		DataDir:      dir,
		DBPath:       dbPath,
		AuthToken:    "remote-token",
		RequireAuth:  false,
		WriteTimeout: 30 * time.Second,
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWindsurf: {windsurfRoot},
		},
	}, database, nil)
	handler := srv.Handler()

	targetReq := httptest.NewRequest(http.MethodGet, "/api/v1/remote-sync/targets", nil)
	targetReq.Header.Set("Authorization", "Bearer remote-token")
	targetW := httptest.NewRecorder()
	handler.ServeHTTP(targetW, targetReq)
	require.Equal(t, http.StatusOK, targetW.Code, "body: %s", targetW.Body.String())
	var targets remotesync.TargetSet
	require.NoError(t, json.Unmarshal(targetW.Body.Bytes(), &targets))
	return handler, targets, stateDB
}

func TestRemoteSyncManifestRefusesFileScopedAgents(t *testing.T) {
	handler, targets, _ := newWindsurfRemoteSyncServer(t)
	payload, err := json.Marshal(targets)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/manifest", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// 501 is in the client's manifest-unsupported set, so the client
	// falls back to the full-archive flow (which sanitizes Windsurf).
	assert.Equal(t, http.StatusNotImplemented, w.Code, "body: %s", w.Body.String())
}

func TestRemoteSyncArchiveRejectsDeltaForFileScopedAgent(t *testing.T) {
	handler, targets, stateDB := newWindsurfRemoteSyncServer(t)
	// A malicious client that skips the manifest and requests the raw,
	// unsanitized state.vscdb as a delta must be refused.
	req := remotesync.ArchiveRequest{
		TargetSet:  targets,
		DeltaFiles: []string{stateDB},
	}
	payload, err := json.Marshal(req)
	require.NoError(t, err)
	archiveReq := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive", bytes.NewReader(payload))
	archiveReq.Header.Set("Authorization", "Bearer remote-token")
	archiveReq.Header.Set("Content-Type", "application/json")
	archiveW := httptest.NewRecorder()

	handler.ServeHTTP(archiveW, archiveReq)

	assert.Equal(t, http.StatusForbidden, archiveW.Code, "body: %s", archiveW.Body.String())
	assert.NotContains(t, archiveW.Body.String(), "extension secret value")
}

func TestRemoteSyncArchiveWindsurfStreamsSanitizedStateDB(t *testing.T) {
	handler, targets, _ := newWindsurfRemoteSyncServer(t)
	payload, err := json.Marshal(targets)
	require.NoError(t, err)
	archiveReq := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive", bytes.NewReader(payload))
	archiveReq.Header.Set("Authorization", "Bearer remote-token")
	archiveReq.Header.Set("Content-Type", "application/json")
	archiveW := httptest.NewRecorder()

	handler.ServeHTTP(archiveW, archiveReq)

	require.Equal(t, http.StatusOK, archiveW.Code, "body: %s", archiveW.Body.String())
	archiveBytes := archiveW.Body.Bytes()
	assert.NotContains(t, string(archiveBytes), "extension secret value")
	entries := tarEntries(t, archiveBytes)
	names := tarEntryNames(entries)
	stateEntry, ok := tarEntryWithSuffix(entries, "workspace-a/"+parser.WindsurfStateDBName)
	require.True(t, ok, "entries: %v", names)
	assert.True(t, hasTarEntrySuffix(entries, "workspace-a/workspace.json"), "entries: %v", entries)
	assert.False(t, hasTarEntrySuffix(entries, "workspace-a/extension-secret.json"), "entries: %v", entries)
	assert.False(t, hasTarEntrySuffix(entries, "workspace-a/"+parser.WindsurfStateDBName+"-wal"), "entries: %v", entries)
	assert.False(t, hasTarEntrySuffix(entries, "workspace-a/"+parser.WindsurfStateDBName+"-shm"), "entries: %v", entries)
	assertSanitizedWindsurfArchiveDB(t, stateEntry.Body)
}

func pathBaseSlash(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

type tarTestEntry struct {
	Name string
	Body []byte
}

func tarEntries(t *testing.T, archive []byte) []tarTestEntry {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	var entries []tarTestEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		require.NoError(t, err)
		body, err := io.ReadAll(tr)
		require.NoError(t, err)
		entries = append(entries, tarTestEntry{Name: hdr.Name, Body: body})
	}
}

func tarEntryNames(entries []tarTestEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

func hasTarEntrySuffix(entries []tarTestEntry, suffix string) bool {
	_, ok := tarEntryWithSuffix(entries, suffix)
	return ok
}

func tarEntryWithSuffix(entries []tarTestEntry, suffix string) (tarTestEntry, bool) {
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name, suffix) {
			return entry, true
		}
	}
	return tarTestEntry{}, false
}

func writeWindsurfArchiveStateDB(t *testing.T, dbPath string) func() {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	_, err = conn.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
	_, err = conn.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO ItemTable (key, value) VALUES (?, ?)`,
		"workbench.panel.aichat.view.aichat.chatdata",
		`{"version":1,"sessionId":"remote-windsurf","requests":[{"requestId":"request-1","message":{"text":"Remote chat"},"response":[{"value":"Remote answer"}],"timestamp":1710000000000}]}`,
	)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO ItemTable (key, value) VALUES (?, ?)`,
		"extension.secret",
		"extension secret value",
	)
	require.NoError(t, err)
	require.FileExists(t, dbPath+"-wal")
	return func() {
		require.NoError(t, conn.Close())
	}
}

func assertSanitizedWindsurfArchiveDB(t *testing.T, body []byte) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	require.NoError(t, os.WriteFile(dbPath, body, 0o644))
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	rows, err := conn.Query(`SELECT key, value FROM ItemTable ORDER BY key`)
	require.NoError(t, err)
	defer rows.Close()
	got := make(map[string]string)
	for rows.Next() {
		var key, value string
		require.NoError(t, rows.Scan(&key, &value))
		got[key] = value
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 1)
	for key, value := range got {
		assert.Equal(t, "workbench.panel.aichat.view.aichat.chatdata", key)
		assert.Contains(t, value, "Remote chat")
		assert.NotContains(t, value, "extension secret value")
	}
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

func TestRemoteSyncManifestListsFiles(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	payload, err := json.Marshal(map[string]any{
		"dirs": map[string][]string{"claude": {filepath.Dir(sessionPath)}},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/manifest",
		bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)
	var manifest remotesync.Manifest
	require.NoError(t, json.NewDecoder(gz).Decode(&manifest))
	require.Len(t, manifest.Files, 1)
	assert.Equal(t, sessionPath, manifest.Files[0].Path)
	assert.Equal(t, int64(3), manifest.Files[0].Size)
	info, err := os.Stat(sessionPath)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime().UnixNano(), manifest.Files[0].MtimeNS)
}

func TestRemoteSyncManifestRejectsUnresolvedPath(t *testing.T) {
	_, handler, _ := newRemoteSyncServer(t)
	body := bytes.NewBufferString(`{"dirs":{"claude":["/etc"]}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/manifest", body)
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoteSyncArchiveDeltaStreamsOnlyRequestedFiles(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	other := filepath.Join(filepath.Dir(sessionPath), "other.jsonl")
	require.NoError(t, os.WriteFile(other, []byte("{}\n"), 0o644))
	payload, err := json.Marshal(map[string]any{
		"dirs":  map[string][]string{"claude": {filepath.Dir(sessionPath)}},
		"files": []string{other},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive",
		bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	names := []string{}
	tr := tar.NewReader(bytes.NewReader(w.Body.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, pathBaseSlash(hdr.Name))
	}
	assert.Equal(t, []string{"other.jsonl"}, names)
}

func TestRemoteSyncArchiveDeltaRejectsFileOutsideAllowedDirs(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	payload, err := json.Marshal(map[string]any{
		"dirs":  map[string][]string{"claude": {filepath.Dir(sessionPath)}},
		"files": []string{"/etc/passwd"},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive",
		bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoteSyncArchiveGzipsWhenAdvertised(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	payload, err := json.Marshal(map[string]any{
		"dirs": map[string][]string{"claude": {filepath.Dir(sessionPath)}},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive",
		bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.NotEmpty(t, hdr.Name)
}

func TestRemoteSyncArchiveExplicitEmptyDeltaReturnsEmptyTar(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	payload, err := json.Marshal(map[string]any{
		"dirs":  map[string][]string{"claude": {filepath.Dir(sessionPath)}},
		"files": []string{},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	tr := tar.NewReader(bytes.NewReader(w.Body.Bytes()))
	_, err = tr.Next()
	assert.Equal(t, io.EOF, err, "explicit empty delta must stream an empty tar")
}
