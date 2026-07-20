package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestPreparedHTTPSyncRebuildContributor(t *testing.T) {
	const (
		sessionID   = "session"
		remoteDir   = "/remote"
		remoteFile  = "/remote/path/session.jsonl"
		skippedDir  = "/remote-skips"
		skippedFile = "/remote-skips/path/intentionally-empty.jsonl"
	)
	sessionBody := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID(
		"2024-01-01T00:00:00Z", "remote rebuild searchable", sessionID,
	).String()
	usageBody := testjsonl.ClaudeUserJSON(
		"<command-name>/usage</command-name>\n"+
			"<command-message>usage</command-message>\n"+
			"<command-args></command-args>",
		"2024-01-01T00:01:00Z",
	)
	files := map[string]string{
		remoteFile:  sessionBody,
		skippedFile: usageBody,
	}
	mtime := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	manifest := Manifest{Files: []ManifestEntry{
		{Path: remoteFile, Size: int64(len(sessionBody)), MtimeNS: mtime.UnixNano()},
		{Path: skippedFile, Size: int64(len(usageBody)), MtimeNS: mtime.UnixNano()},
	}}
	archive := func(t *testing.T) []byte {
		t.Helper()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, path := range []string{remoteFile, skippedFile} {
			name, err := safeRemotePathArchiveName(path)
			require.NoError(t, err)
			body := files[path]
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: mtime,
			}))
			_, err = tw.Write([]byte(body))
			require.NoError(t, err)
		}
		require.NoError(t, tw.Close())
		return buf.Bytes()
	}(t)
	targets := TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentClaude: {remoteDir, skippedDir},
	}}
	targetsJSON, err := json.Marshal(targets)
	require.NoError(t, err)
	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(targetsJSON)
		case "/api/v1/remote-sync/manifest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(manifestJSON)
		case "/api/v1/remote-sync/archive":
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	database, err := db.Open(filepath.Join(t.TempDir(), "active.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.NoError(t, database.ReplaceRemoteSkippedFiles("devbox", map[string]int64{
		remoteFile: mtime.UnixNano(),
	}))
	hs := HTTPSync{
		Host: "devbox", URL: server.URL, DataDir: t.TempDir(), DB: database,
	}
	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	assert.FileExists(t, remappedRemotePath(prepared.Root(), remoteFile))
	contributor, err := prepared.RebuildContributor()
	require.NoError(t, err)

	localEngine := syncpkg.NewEngine(database, syncpkg.EngineConfig{})
	t.Cleanup(localEngine.Close)
	stats, err := localEngine.ResyncAllWithOptions(
		context.Background(), nil,
		syncpkg.RebuildOptions{Contributors: []syncpkg.RebuildContributor{contributor}},
	)
	require.NoError(t, err)
	assert.False(t, stats.Aborted)

	full, err := database.GetSessionFull(context.Background(), "devbox~"+sessionID)
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.Equal(t, "devbox", full.Machine)
	require.NotNil(t, full.FilePath)
	assert.Equal(t, "devbox:"+remoteFile, *full.FilePath)
	messages, err := database.GetMessages(
		context.Background(), "devbox~"+sessionID, 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "remote rebuild searchable", messages[0].Content)
	search, err := database.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "searchable", Limit: 10, IncludeOneShot: true,
	})
	require.NoError(t, err)
	require.Len(t, search.Matches, 1)
	assert.Equal(t, "devbox~"+sessionID, search.Matches[0].SessionID)
	remoteCache, err := database.LoadRemoteSkippedFiles("devbox")
	require.NoError(t, err)
	require.Len(t, remoteCache, 1)
	for cachedPath := range remoteCache {
		assert.True(t, strings.HasPrefix(
			cachedPath, skippedFile+"?source_hash=",
		), "rowless Claude skips must persist their content hash")
	}
	assert.NotContains(t, remoteCache, remoteFile,
		"a full contributor must not load the active database's stale host cache")

	prepared.targets = TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentClaude: {skippedDir},
	}}
	activeStats, err := prepared.ImportActive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, activeStats.Failed)
	assert.Equal(t, 1, activeStats.Skipped)
}

func TestPreparedHTTPSyncImportActiveImportsPreparedRoot(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "session.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "prepared import")
	database, hs := newMirrorSync(t, remote, t.TempDir())

	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	before, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, before.Sessions, "Prepare must not import active sessions")

	stats, err := prepared.ImportActive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	after, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, after.Sessions, 1)
	assert.Equal(t, "devbox", after.Sessions[0].Machine)
}

func TestImporterImportsExtractedRemoteFiles(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	tmp := t.TempDir()
	remoteDir := "/home/wes/.claude/projects"
	localDir := filepath.Join(tmp, "home", "wes", ".claude", "projects", "test-project")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	sessionPath := filepath.Join(localDir, "session.jsonl")
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "remote import").
			String(),
	), 0o644))

	stats, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(context.Background(), TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {remoteDir},
		},
	}, tmp)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	page, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "devbox", page.Sessions[0].Machine)
	full, err := database.GetSessionFull(context.Background(), page.Sessions[0].ID)
	require.NoError(t, err)
	require.NotNil(t, full)
	require.NotNil(t, full.FilePath)
	assert.Contains(t, *full.FilePath, "devbox:/home/wes/.claude/projects/test-project/session.jsonl")
}

func TestImporterImportsHermesDatabaseOnlySession(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	extracted := t.TempDir()
	remoteSessionsDir := "/home/remote/.hermes/profiles/research/sessions"
	remoteStateDB := "/home/remote/.hermes/profiles/research/state.db"
	localStateDB := remappedRemotePath(extracted, remoteStateDB)
	require.NoError(t, os.MkdirAll(filepath.Dir(localStateDB), 0o755))
	require.NoError(t, os.MkdirAll(
		remappedRemotePath(extracted, remoteSessionsDir), 0o755,
	))
	writeHermesImportStateDB(t, localStateDB)

	stats, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(context.Background(), TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentHermes: {remoteSessionsDir},
		},
	}, extracted)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	session, err := database.GetSession(
		context.Background(), "devbox~hermes:database-only",
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.DisplayName)
	assert.Equal(t, "Database-only profile", *session.DisplayName)
	require.NotNil(t, session.ParentSessionID)
	assert.Equal(t, "devbox~hermes:parent", *session.ParentSessionID)
	assert.Equal(t, 70, session.TotalOutputTokens)
	assert.Equal(t, 320, session.PeakContextTokens)
}

func writeHermesImportStateDB(t *testing.T, path string) {
	t.Helper()
	stateDB, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, stateDB.Close()) }()

	_, err = stateDB.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			model TEXT,
			parent_session_id TEXT,
			started_at REAL NOT NULL,
			ended_at REAL,
			message_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			estimated_cost_usd REAL,
			actual_cost_usd REAL,
			cost_status TEXT,
			cost_source TEXT,
			title TEXT,
			api_call_count INTEGER DEFAULT 0
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT,
			tool_call_id TEXT,
			tool_calls TEXT,
			timestamp REAL NOT NULL,
			finish_reason TEXT,
			reasoning TEXT,
			reasoning_content TEXT,
			reasoning_details TEXT,
			codex_reasoning_items TEXT,
			codex_message_items TEXT
		);
		INSERT INTO sessions (
			id, source, model, parent_session_id, started_at, ended_at,
			message_count, input_tokens, output_tokens, cache_read_tokens,
			title
		) VALUES (
			'database-only', 'cli', 'gpt-5.4', 'parent',
			1778767200.0, 1778767800.0, 1, 300, 70, 20,
			'Database-only profile'
		);
		INSERT INTO messages (session_id, role, content, timestamp)
		VALUES ('database-only', 'user', 'database-only message', 1778767210.0);
	`)
	require.NoError(t, err)
}

// TestImporterMapsHermesStateDBExtraFileAndRefreshesWALChanges verifies both
// remote archive path translation and repeated-import freshness. A committed
// WAL-only metadata update must invalidate the skip entry even though the main
// state.db file remains unchanged.
func TestImporterMapsHermesStateDBExtraFileAndRefreshesWALChanges(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	extracted := t.TempDir()
	remoteSessionsDir := "/home/remote/.hermes/profiles/research/sessions"
	remoteStateDB := "/home/remote/.hermes/profiles/research/state.db"
	localStateDB := remappedRemotePath(extracted, remoteStateDB)
	require.NoError(t, os.MkdirAll(filepath.Dir(localStateDB), 0o755))
	require.NoError(t, os.MkdirAll(
		remappedRemotePath(extracted, remoteSessionsDir), 0o755,
	))
	writeHermesImportStateDB(t, localStateDB)
	writer := openHermesImportWALWriter(t, localStateDB)
	remoteStateWAL := remoteStateDB + "-wal"

	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentHermes: {remoteSessionsDir},
		},
		ExtraFiles: []string{remoteStateDB, remoteStateWAL},
	}

	stats, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(context.Background(), targets, extracted)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)

	full, err := database.GetSessionFull(
		context.Background(), "devbox~hermes:database-only",
	)
	require.NoError(t, err)
	require.NotNil(t, full)
	require.NotNil(t, full.FilePath)
	assert.Equal(t, "devbox:"+remoteStateDB, *full.FilePath,
		"state.db extra file must map to its original remote path")
	assert.NotContains(t, *full.FilePath, extracted,
		"the import must not leak the local extraction dir into FilePath")

	// A second import of the unchanged archive must skip the state.db and
	// persist its skip entry keyed by the remote path. Without the
	// extra-file mapping the local temp path cannot be translated back, so
	// the entry is silently dropped and the archive is re-fingerprinted on
	// every subsequent sync.
	second, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(context.Background(), targets, extracted)
	require.NoError(t, err)
	assert.Zero(t, second.SessionsSynced,
		"unchanged state.db must not re-sync on the second import")

	remoteCache, err := database.LoadRemoteSkippedFiles("devbox")
	require.NoError(t, err)
	require.NotEmpty(t, remoteCache,
		"the state.db skip entry must survive the import, not be discarded")
	_, ok := remoteCache[remoteStateDB]
	assert.True(t, ok,
		"skip cache must key the state.db entry by its remote path, got %v",
		remoteCache)

	stateBefore, err := os.Stat(localStateDB)
	require.NoError(t, err)
	_, err = writer.Exec(`
		UPDATE sessions
		SET title = 'WAL-refreshed profile'
		WHERE id = 'database-only'
	`)
	require.NoError(t, err)
	localStateWAL := remappedRemotePath(extracted, remoteStateWAL)
	require.FileExists(t, localStateWAL)
	stateAfter, err := os.Stat(localStateDB)
	require.NoError(t, err)
	assert.Equal(t, stateBefore.Size(), stateAfter.Size())
	assert.Equal(t, stateBefore.ModTime(), stateAfter.ModTime(),
		"the committed update must remain WAL-only for this regression")
	walTime := stateAfter.ModTime().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(localStateWAL, walTime, walTime))

	changed, err := Importer{
		Host: "devbox",
		DB:   database,
	}.ImportExtracted(context.Background(), targets, extracted)
	require.NoError(t, err)
	assert.Equal(t, 1, changed.SessionsSynced,
		"a WAL-only commit must invalidate the remote archive skip entry")
	refreshed, err := database.GetSession(
		context.Background(), "devbox~hermes:database-only",
	)
	require.NoError(t, err)
	require.NotNil(t, refreshed)
	require.NotNil(t, refreshed.DisplayName)
	assert.Equal(t, "WAL-refreshed profile", *refreshed.DisplayName)
}

func openHermesImportWALWriter(t *testing.T, stateDB string) *sql.DB {
	t.Helper()
	writer, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	writer.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })

	var journalMode string
	require.NoError(t, writer.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
	_, err = writer.Exec(`PRAGMA wal_autocheckpoint = 0`)
	require.NoError(t, err)
	return writer
}

func TestRemotePathMappingHandlesWindowsDrivePath(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := `C:\Users\wes\.codex\sessions`
	remoteFile := `C:\Users\wes\.codex\sessions\2026\session.jsonl`
	wantLocalDir := filepath.Join(
		tempDir, "__drive_C", "Users", "wes", ".codex", "sessions",
	)
	wantLocalFile := filepath.Join(
		tempDir, "__drive_C", "Users", "wes", ".codex", "sessions",
		"2026", "session.jsonl",
	)

	assert.Equal(t, wantLocalDir, RemappedDir(tempDir, remoteDir))
	assert.Equal(t, wantLocalFile, remappedRemotePath(tempDir, remoteFile))
	assert.Equal(t,
		remoteFile,
		RemapToRemotePath(tempDir, remoteDir, wantLocalFile),
	)
}

func TestRemotePathMappingHandlesForwardSlashUNCPath(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := `//server/share/.codex/sessions`
	remoteFile := `//server/share/.codex/sessions/2026/session.jsonl`
	wantLocalDir := filepath.Join(
		tempDir, "__unc", "server", "share", ".codex", "sessions",
	)
	wantLocalFile := filepath.Join(
		tempDir, "__unc", "server", "share", ".codex", "sessions",
		"2026", "session.jsonl",
	)

	assert.Equal(t, wantLocalDir, RemappedDir(tempDir, remoteDir))
	assert.Equal(t, wantLocalFile, remappedRemotePath(tempDir, remoteFile))
	assert.Equal(t,
		remoteFile,
		RemapToRemotePath(tempDir, remoteDir, wantLocalFile),
	)
}

func TestImporterRejectsEscapingRemoteTargets(t *testing.T) {
	stats, err := Importer{
		Host: "devbox",
	}.ImportExtracted(context.Background(), TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"../../outside"},
		},
	}, t.TempDir())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe remote path")
	assert.Zero(t, stats)
}

func TestLocalArchivePathRejectsDotDotComponents(t *testing.T) {
	_, err := safeLocalArchivePath(t.TempDir(), "safe/../escape.jsonl")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe archive path")
}

func TestRemoteSkipCacheUsesArchivePathMapping(t *testing.T) {
	tempDir := filepath.Join("tmp", "sync-123")
	remoteDir := `C:\Users\wes\.codex\sessions`
	remoteFile := `C:\Users\wes\.codex\sessions\2026\session.jsonl`
	tempRoot := RemappedDir(tempDir, remoteDir)
	tempFile := filepath.Join(tempRoot, "2026", "session.jsonl")

	translated := translateRemoteCacheToTemp(
		map[string]int64{remoteFile: 123},
		[]string{remoteDir},
		[]string{tempRoot},
	)
	assert.Equal(t, map[string]int64{tempFile: 123}, translated)

	got, ok := tempPathToRemotePath(
		tempFile,
		[]string{remoteDir},
		[]string{tempRoot},
	)
	require.True(t, ok)
	assert.Equal(t, remoteFile, got)
}
