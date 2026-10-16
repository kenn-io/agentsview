package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// hermesArchiveAggregateFileInfo mirrors the legacy engine helper
// hermesArchiveEffectiveInfo for test assertions: the aggregate size and mtime
// of the state.db plus every transcript directly under its sessions directory.
// The Hermes provider now owns this aggregation; this helper only computes the
// expected values the engine must persist.
func hermesArchiveAggregateFileInfo(t *testing.T, stateDB string) (int64, int64) {
	t.Helper()
	info, err := os.Stat(stateDB)
	require.NoError(t, err)
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	sessionsDir := filepath.Join(filepath.Dir(stateDB), "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return size, mtime
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		isJSONL := filepath.Ext(name) == ".jsonl"
		isSessionJSON := filepath.Ext(name) == ".json" &&
			len(name) >= len("session_") && name[:len("session_")] == "session_"
		if !isJSONL && !isSessionJSON {
			continue
		}
		fileInfo, err := os.Stat(filepath.Join(sessionsDir, name))
		if err != nil || fileInfo.IsDir() {
			continue
		}
		size += fileInfo.Size()
		if fileMtime := fileInfo.ModTime().UnixNano(); fileMtime > mtime {
			mtime = fileMtime
		}
	}
	return size, mtime
}

// TestHermesProviderFingerprintAggregatesDirectTranscripts confirms the
// provider-owned archive fingerprint folds the size and mtime of transcripts
// living directly under the sessions directory into the state.db's freshness
// identity, replacing the engine's removed hermesArchiveEffectiveInfo.
func TestHermesProviderFingerprintAggregatesDirectTranscripts(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	transcriptPath := filepath.Join(root, "sessions", "extra.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(transcriptPath, []byte("{}\n{}\n"), 0o644))

	transcriptTime := time.Now().Add(2 * time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(transcriptPath, transcriptTime, transcriptTime))

	wantSize, wantMtime := hermesArchiveAggregateFileInfo(t, stateDB)

	provider, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{
		Roots:   []string{filepath.Join(root, "sessions")},
		Machine: "local",
	})
	require.True(t, ok)
	resolver, ok := provider.(parser.ReconciliationSourceResolver)
	require.True(t, ok)
	source, found, err := resolver.SourceForReconciliation(t.Context(), stateDB, "")
	require.NoError(t, err)
	require.True(t, found)

	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, wantSize, fingerprint.Size)
	assert.Equal(t, wantMtime, fingerprint.MTimeNS)
}

// TestHermesProviderFingerprintChangesWhenTranscriptRemoved confirms the
// archive fingerprint shrinks back to the state.db's own size when a direct
// transcript is removed, replacing the engine's removed effective-info logic.
func TestHermesProviderFingerprintChangesWhenTranscriptRemoved(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	transcriptPath := filepath.Join(root, "sessions", "extra.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(transcriptPath, []byte("{}\n{}\n"), 0o644))

	provider, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{
		Roots:   []string{filepath.Join(root, "sessions")},
		Machine: "local",
	})
	require.True(t, ok)
	resolver, ok := provider.(parser.ReconciliationSourceResolver)
	require.True(t, ok)
	source, found, err := resolver.SourceForReconciliation(t.Context(), stateDB, "")
	require.NoError(t, err)
	require.True(t, found)

	before, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	require.NoError(t, os.Remove(transcriptPath))
	after, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	stateInfo, err := os.Stat(stateDB)
	require.NoError(t, err)
	assert.NotEqual(t, before.Size, after.Size)
	assert.Equal(t, stateInfo.Size(), after.Size)
}

func TestHermesArchiveStoresCronGroups(t *testing.T) {
	agent := parser.AgentHermes
	prefix := string(agent) + ":"
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	database := dbtest.OpenTestDB(t)
	_, err = conn.ExecContext(t.Context(), `DELETE FROM messages; DELETE FROM sessions`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `
		INSERT INTO sessions (id, source, parent_session_id, started_at, ended_at, message_count)
		VALUES ('cron_job-a_20261008_120000', 'cron', NULL, 1791460800, 1791460860, 1),
		       ('middle', 'cron', 'cron_job-a_20261008_120000', 1791460860, 1791460920, 1),
		       ('tip', 'cron', 'middle', 1791460920, 1791460980, 1);
		INSERT INTO messages (session_id, role, content, timestamp)
		VALUES ('cron_job-a_20261008_120000', 'user', 'Generate digest', 1791460801),
		       ('middle', 'user', 'Continue digest', 1791460861),
		       ('tip', 'user', 'Finish digest', 1791460921);
	`)
	require.NoError(t, err)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
	checkGroups := func() {
		t.Helper()
		for _, id := range []string{prefix + "middle", prefix + "tip"} {
			stored, err := database.GetSession(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Regexp(t, `^job-a:[0-9a-f]{8}$`, stored.GroupKey, id)
		}
	}
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), nil, true))

	_, err = conn.ExecContext(t.Context(), `
		UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id = 'cron_job-a_20261008_120000';
		DELETE FROM messages WHERE session_id = 'cron_job-a_20261008_120000';
		DELETE FROM sessions WHERE id = 'cron_job-a_20261008_120000';
	`)
	require.NoError(t, err)
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), nil, true))
	checkGroups()
	stored, err := database.GetSession(t.Context(), prefix+"middle")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Empty(t, stored.ParentSessionID)

	require.NoError(t, conn.Close())
	_, err = database.AssignSessionProject(t.Context(), prefix+"middle", "ops")
	require.NoError(t, err)
	for range 2 {
		stats := engine.ResyncAll(t.Context(), nil)
		require.False(t, stats.Aborted, "%v", stats.Warnings)
		checkGroups()
		stored, err := database.GetSession(t.Context(), prefix+"middle")
		require.NoError(t, err)
		require.NotNil(t, stored)
		assert.Equal(t, "ops", stored.Project)
	}
}

func TestHermesProfileCreatedAfterEngineInitializationIsDiscovered(t *testing.T) {
	profilesRoot := filepath.Join(t.TempDir(), ".hermes", "profiles")
	require.NoError(t, os.MkdirAll(profilesRoot, 0o755))
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {profilesRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	before := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, before.Synced)

	profileRoot := filepath.Join(profilesRoot, "research")
	require.NoError(t, os.MkdirAll(profileRoot, 0o755))
	writeHermesArchiveStateDB(t, profileRoot)

	after := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, after.Synced)
	session, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.FirstMessage)
	assert.Equal(t, "state db message", *session.FirstMessage)
}

func TestReconcileHermesProfilesRootPreservesActiveMembers(t *testing.T) {
	profilesRoot := filepath.Join(t.TempDir(), ".hermes", "profiles")
	profileRoot := filepath.Join(profilesRoot, "research")
	require.NoError(t, os.MkdirAll(profileRoot, 0o755))
	stateDB := writeHermesArchiveStateDB(t, profileRoot)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {profilesRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "local", []db.SessionSourcePath{{
			Agent: string(parser.AgentHermes), FilePath: stateDB,
		}},
	))

	require.NoError(t, engine.ReconcileWatchRoots(
		t.Context(), []string{profilesRoot}, false,
	))

	result := engine.LastReconciliationResult()
	assert.True(t, result.Complete)
	active, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, active,
		"authoritative discovery of the profiles container must retain its members")
	require.NotNil(t, active.FirstMessage)
	assert.Equal(t, "state db message", *active.FirstMessage)
}

// TestProcessFileHermesArchiveSkipCacheUsesAggregateMtime confirms the
// provider-authoritative processFile path keys the skip cache on the aggregate
// archive mtime (state.db plus direct transcripts), so a cached entry stamped
// with that mtime short-circuits a reparse.
func TestProcessFileHermesArchiveSkipCacheUsesAggregateMtime(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	transcriptPath := filepath.Join(root, "sessions", "extra.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(transcriptPath, []byte("{}\n"), 0o644))
	transcriptTime := time.Now().Add(2 * time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(transcriptPath, transcriptTime, transcriptTime))

	_, wantMtime := hermesArchiveAggregateFileInfo(t, stateDB)

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {filepath.Join(root, "sessions")},
		},
		Machine: "local",
	})
	provider, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{
		Roots: []string{filepath.Join(root, "sessions")},
	})
	require.True(t, ok)
	resolver, ok := provider.(parser.ReconciliationSourceResolver)
	require.True(t, ok)
	source, found, err := resolver.SourceForReconciliation(t.Context(), stateDB, "")
	require.NoError(t, err)
	require.True(t, found)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	initial := engine.processFile(t.Context(), parser.DiscoveredFile{
		Path: stateDB, Agent: parser.AgentHermes,
	})
	require.NoError(t, initial.err)
	require.NotEmpty(t, initial.results)
	pending := make([]pendingWrite, 0, len(initial.results))
	for _, result := range initial.results {
		pending = append(pending, pendingWrite{
			sess: result.Session, msgs: result.Messages,
		})
	}
	_, _, failed, _ := engine.writeBatch(pending, syncWriteDefault, true)
	require.Zero(t, failed)
	engine.InjectSkipCache(map[string]int64{
		providerProcessCacheKeyWithHash(
			stateDB, fingerprint, provider.Capabilities().Sync,
		): wantMtime,
	})

	res := engine.processFile(t.Context(), parser.DiscoveredFile{
		Path:  stateDB,
		Agent: parser.AgentHermes,
	})

	require.NoError(t, res.err)
	assert.True(t, res.skip)
	assert.True(t, res.cacheSkip)
	assert.Equal(t, wantMtime, res.mtime)
}

// TestProcessFileHermesArchivePersistsAggregateFingerprint confirms the
// provider-authoritative processFile path stamps every archive session with the
// state.db path and the aggregate size and mtime, and that a second pass skips
// once the file info is persisted. This replaces the removed
// processHermes-based assertions.
func TestProcessFileHermesArchivePersistsAggregateFingerprint(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	transcriptPath := filepath.Join(root, "sessions", "extra.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(
		transcriptPath,
		[]byte(
			`{"role":"session_meta","platform":"cli","timestamp":"2026-05-14T10:00:00.000000"}`+"\n"+
				`{"role":"user","content":"new transcript","timestamp":"2026-05-14T10:01:00.000000"}`+"\n",
		),
		0o644,
	))

	wantSize, wantMtime := hermesArchiveAggregateFileInfo(t, stateDB)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {filepath.Join(root, "sessions")},
		},
		Machine: "local",
	})

	res := engine.processFile(t.Context(), parser.DiscoveredFile{
		Path:  stateDB,
		Agent: parser.AgentHermes,
	})

	require.NoError(t, res.err)
	require.NotEmpty(t, res.results)
	for _, result := range res.results {
		assert.Equal(t, stateDB, result.Session.File.Path)
		assert.Equal(t, wantSize, result.Session.File.Size)
		assert.Equal(t, wantMtime, result.Session.File.Mtime)
	}

	pending := make([]pendingWrite, 0, len(res.results))
	for _, result := range res.results {
		pending = append(pending, pendingWrite{
			sess:        result.Session,
			msgs:        result.Messages,
			usageEvents: result.UsageEvents,
		})
	}
	written, _, failed, _ := engine.writeBatch(pending, syncWriteDefault, true)
	require.Equal(t, 0, failed)
	require.NotZero(t, written)

	storedSize, storedMtime, ok := database.GetFileInfoByPath(t.Context(), stateDB)
	require.True(t, ok)
	assert.Equal(t, wantSize, storedSize)
	assert.Equal(t, wantMtime, storedMtime)
}

func TestSyncPathsHermesArchiveTranscriptKeepsSeparateSource(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	transcriptPath := filepath.Join(root, "sessions", "extra.jsonl")
	writeHermesTranscriptFile(t, filepath.Dir(transcriptPath), "extra")
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.syncPathsContext(t.Context(), []string{transcriptPath}, false))
	stored, err := database.GetSession(t.Context(), "hermes:extra")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, transcriptPath, database.GetSessionFilePath(t.Context(), stored.ID))
	assert.Equal(t, new("hello"), stored.FirstMessage)
	child, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	assert.Nil(t, child, "a standalone transcript event must not import other state.db members")
	_, _, found := database.GetFileInfoByPath(t.Context(), stateDB)
	assert.False(t, found)
}

func TestSyncPathsHermesArchiveWALCommitRefreshesMetadata(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	writer := openHermesArchiveWALWriter(t, stateDB)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	initial := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, initial.Synced)
	stateBefore, err := os.Stat(stateDB)
	require.NoError(t, err)

	_, err = writer.ExecContext(t.Context(), `UPDATE sessions SET title = 'WAL-only title' WHERE id = 'child'`)
	require.NoError(t, err)
	walPath := stateDB + "-wal"
	_, err = os.Stat(walPath)
	require.NoError(t, err)
	stateAfter, err := os.Stat(stateDB)
	require.NoError(t, err)
	assert.Equal(t, stateBefore.Size(), stateAfter.Size())
	assert.Equal(t, stateBefore.ModTime(), stateAfter.ModTime(),
		"the committed update must remain WAL-only for this regression")
	walTime := stateAfter.ModTime().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(walPath, walTime, walTime))

	engine.SyncPaths([]string{walPath})

	session, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.DisplayName)
	assert.Equal(t, "WAL-only title", *session.DisplayName)
	storedSize, storedMtime, found := database.GetFileInfoByPath(t.Context(), parser.VirtualSourcePath(stateDB, "child"))
	require.True(t, found)
	assert.Equal(t, stateAfter.Size(), storedSize)
	assert.Equal(t, walTime.UnixNano(), storedMtime)
}

func openHermesArchiveWALWriter(t *testing.T, stateDB string) *sql.DB {
	t.Helper()

	writer, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	writer.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })

	var journalMode string
	require.NoError(t, writer.QueryRowContext(t.Context(), `PRAGMA journal_mode = WAL`).Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
	_, err = writer.ExecContext(t.Context(), `PRAGMA wal_autocheckpoint = 0`)
	require.NoError(t, err)
	return writer
}

func TestReconcileHermesStateMemberDetectsSameStatTranscriptRewrite(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	transcriptPath := filepath.Join(sessionsDir, "session_child.json")
	writeTranscript := func(message string) {
		require.NoError(t, os.WriteFile(transcriptPath, []byte(`{
			"platform":"cli",
			"session_start":"2026-05-14T10:00:00Z",
			"last_updated":"2026-05-14T10:02:00Z",
			"messages":[
				{"role":"user","content":"`+message+`","timestamp":"2026-05-14T10:01:00Z"},
				{"role":"assistant","content":"Done.","timestamp":"2026-05-14T10:02:00Z"}
			]
		}`), 0o644))
	}
	writeTranscript("original prompt")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		Machine: "local",
	})
	assertMessages := func(want ...string) {
		messages, err := database.GetMessages(
			t.Context(), "hermes:child", 0, len(want), true,
		)
		require.NoError(t, err)
		require.Len(t, messages, len(want))
		for i := range want {
			assert.Equal(t, want[i], messages[i].Content)
		}
	}
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), nil, true))
	assertMessages("original prompt", "Done.")

	provider, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{
		Roots: []string{sessionsDir},
	})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{
		RawSessionID: "child",
	})
	require.NoError(t, err)
	require.True(t, found)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	memberPath := parser.VirtualSourcePath(stateDB, "child")
	storedSize, storedMtime, found := database.GetFileInfoByPath(t.Context(), memberPath)
	require.True(t, found)
	assert.Equal(t, fingerprint.Size, storedSize)
	assert.Equal(t, fingerprint.MTimeNS, storedMtime)
	storedHash, found := database.GetFileHashByPath(t.Context(), memberPath)
	require.True(t, found)
	assert.Equal(t, fingerprint.Hash, storedHash)

	transcriptInfo, err := os.Stat(transcriptPath)
	require.NoError(t, err)
	writeTranscript("modified prompt")
	require.NoError(t, os.Chtimes(
		transcriptPath, transcriptInfo.ModTime(), transcriptInfo.ModTime(),
	))
	rewrittenInfo, err := os.Stat(transcriptPath)
	require.NoError(t, err)
	require.Equal(t, transcriptInfo.Size(), rewrittenInfo.Size())
	require.Equal(t, transcriptInfo.ModTime(), rewrittenInfo.ModTime())

	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), nil, true))
	assertMessages("modified prompt", "Done.")
}

func TestReconcileHermesDefaultSessionsRootTombstonesRemovedStateMember(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentHermes: {sessionsDir: "archivebox"},
		},
		Machine: "localbox",
	})
	t.Cleanup(engine.Close)

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	))
	stored, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, stored, "initial reconciliation must store the state member")
	assert.Equal(t, "archivebox", stored.Machine)
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	))

	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "DELETE FROM sessions WHERE id = 'child'")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	))
	stored, err = database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	assert.NotNil(t, stored,
		"authoritative reconciliation must tombstone a removed state.db member")
}

// TestReconcileHermesScopedArchiveTombstonesRemovedStateMember pins
// aggregate-member authority for a pass scoped to one complete archive: the
// scope's proof spans the archive's whole state.db membership, so a removed
// member is reclaimed even though a second configured root stays uncovered
// and full provider-root coverage is out of reach.
func TestReconcileHermesScopedArchiveTombstonesRemovedStateMember(
	t *testing.T,
) {
	archiveRoot := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, archiveRoot)
	sessionsDir := filepath.Join(archiveRoot, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	otherRoot := t.TempDir()
	writeHermesTranscriptFile(t, otherRoot, "keeper")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir, otherRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir, otherRoot}, false,
	))
	stored, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, stored, "initial reconciliation must store the state member")

	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "DELETE FROM sessions WHERE id = 'child'")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	// The pass is scoped to the archive only; the other configured root is
	// deliberately not requested, so full provider-root coverage is absent.
	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentHermes, []string{sessionsDir},
	))

	stored, err = database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	assert.NotNil(t, stored,
		"an archive-scoped pass proves its own membership and reclaims the member")
	survivor, err := database.GetSession(t.Context(), "hermes:keeper")
	require.NoError(t, err)
	assert.NotNil(t, survivor,
		"the unrequested root's sessions are untouched")
}

func TestReconcileHermesLabeledStateDBRootTombstonesRemovedMember(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {stateDB},
		},
		SourceMachines: map[parser.AgentType]map[string]string{
			parser.AgentHermes: {stateDB: "archivebox"},
		},
		Machine: "localbox",
	})
	t.Cleanup(engine.Close)

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	stored, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "archivebox", stored.Machine)

	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "DELETE FROM sessions WHERE id = 'child'")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{root}, false,
	))
	stored, err = database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	assert.NotNil(t, stored)
}

// Streamed discovery must open state.db a bounded number of times per pass:
// once to stream members and once for the transcript membership check — not
// once per transcript file, which scales reconciliation work with archive
// size instead of the changed batch.
func TestReconcileHermesStateDBOpensBoundedPerPass(t *testing.T) {
	scansFor := func(t *testing.T, transcripts int) int {
		t.Helper()

		root := t.TempDir()
		writeHermesArchiveStateDB(t, root)
		sessionsDir := filepath.Join(root, "sessions")
		require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
		for i := range transcripts {
			require.NoError(t, os.WriteFile(
				filepath.Join(sessionsDir, fmt.Sprintf("transcript-%03d.jsonl", i)),
				[]byte(`{"type":"user_message","content":"hi","timestamp":"2026-05-14T10:00:00Z"}`+"\n"),
				0o644,
			))
		}

		database := dbtest.OpenTestDB(t)
		engine := NewEngine(t.Context(), database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentHermes: {sessionsDir},
			},
			Machine: "local",
		})
		t.Cleanup(engine.Close)
		require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
			t.Context(), []string{sessionsDir}, false,
		))
		return engine.LastReconciliationResult().Metrics.SharedContainerScans
	}

	small := scansFor(t, 2)
	large := scansFor(t, 30)
	assert.Positive(t, small, "state.db opens must be instrumented")
	assert.Equal(t, small, large,
		"state.db opens must not scale with transcript count")
}

// Fingerprinting streamed state members must not open state.db once per
// member: a warm pass over an unchanged archive re-fingerprints every member,
// so per-pass opens must stay constant as the member count grows.
func TestReconcileHermesStateMemberFingerprintOpensBoundedPerPass(
	t *testing.T,
) {
	scansFor := func(t *testing.T, members int) int {
		t.Helper()

		root := t.TempDir()
		stateDB := writeHermesArchiveStateDB(t, root)
		sessionsDir := filepath.Join(root, "sessions")
		require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
		conn, err := sql.Open("sqlite3", stateDB)
		require.NoError(t, err)
		for i := range members {
			_, err = conn.ExecContext(t.Context(), `
				INSERT INTO sessions (
					id, source, model, started_at, ended_at, message_count
				) VALUES (?, 'discord', 'gpt-5.4', 1778767900.0, 1778768000.0, 1)
			`, fmt.Sprintf("member-%03d", i))
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `
				INSERT INTO messages (session_id, role, content, timestamp)
				VALUES (?, 'user', 'hello', 1778767910.0)
			`, fmt.Sprintf("member-%03d", i))
			require.NoError(t, err)
		}
		require.NoError(t, conn.Close())

		database := dbtest.OpenTestDB(t)
		engine := NewEngine(t.Context(), database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentHermes: {sessionsDir},
			},
			Machine: "local",
		})
		t.Cleanup(engine.Close)
		// First pass stores every member (per-member parse work is bounded
		// by the changed batch); the warm second pass is fingerprint-only.
		require.NoError(t, engine.ReconcileWatchRoots(
			t.Context(), []string{sessionsDir}, false,
		))
		require.NoError(t, engine.ReconcileWatchRoots(
			t.Context(), []string{sessionsDir}, false,
		))
		return engine.LastReconciliationResult().Metrics.SharedContainerScans
	}

	small := scansFor(t, 2)
	large := scansFor(t, 30)
	assert.Positive(t, small, "state.db opens must be instrumented")
	assert.Equal(t, small, large,
		"warm-pass state.db opens must not scale with member count")
}

// A change to one state.db member moves the shared container stat that every
// member's fingerprint inherits, so stored size+mtime freshness fails for all
// of them. The per-member content hash must then establish freshness: only
// the changed member re-parses and rewrites, keeping per-change work bounded
// by the changed batch instead of total archive size. Member deletion must
// keep tombstoning at the same cardinality.
func TestReconcileHermesSiblingMemberChangeBoundsUnchangedMemberWork(
	t *testing.T,
) {
	passFor := func(t *testing.T, members int) (synced, scans int) {
		t.Helper()

		root := t.TempDir()
		stateDB := writeHermesArchiveStateDB(t, root)
		sessionsDir := filepath.Join(root, "sessions")
		require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
		conn, err := sql.Open("sqlite3", stateDB)
		require.NoError(t, err)
		for i := range members {
			_, err = conn.ExecContext(t.Context(), `
				INSERT INTO sessions (
					id, source, model, started_at, ended_at, message_count
				) VALUES (?, 'discord', 'gpt-5.4', 1778767900.0, 1778768000.0, 1)
			`, fmt.Sprintf("member-%03d", i))
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `
				INSERT INTO messages (session_id, role, content, timestamp)
				VALUES (?, 'user', 'hello', 1778767910.0)
			`, fmt.Sprintf("member-%03d", i))
			require.NoError(t, err)
		}
		require.NoError(t, conn.Close())

		database := dbtest.OpenTestDB(t)
		engine := NewEngine(t.Context(), database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentHermes: {sessionsDir},
			},
			Machine: "local",
		})
		t.Cleanup(engine.Close)
		require.NoError(t, engine.ReconcileWatchRoots(
			t.Context(), []string{sessionsDir}, false,
		))

		// Change exactly one member; the shared container stat moves for all.
		conn, err = sql.Open("sqlite3", stateDB)
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(),
			"UPDATE messages SET content = 'changed sibling' WHERE session_id = 'child'",
		)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		bumped := time.Now().Add(2 * time.Second).Truncate(time.Second)
		require.NoError(t, os.Chtimes(stateDB, bumped, bumped))

		stats, _, err := engine.ReconcileWatchRootsWithStats(
			t.Context(), []string{sessionsDir}, false, nil,
		)
		require.NoError(t, err)
		scans = engine.LastReconciliationResult().Metrics.SharedContainerScans

		// Deletion must keep tombstoning through the hash-freshness path.
		conn, err = sql.Open("sqlite3", stateDB)
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(),
			"DELETE FROM sessions WHERE id = 'member-000'",
		)
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(),
			"DELETE FROM messages WHERE session_id = 'member-000'",
		)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		require.NoError(t, engine.ReconcileWatchRoots(
			t.Context(), []string{sessionsDir}, false,
		))
		removed, err := database.GetSession(t.Context(), "hermes:member-000")
		require.NoError(t, err)
		assert.NotNil(t, removed, "removed member must remain browsable")
		survivor, err := database.GetSession(t.Context(), "hermes:member-001")
		require.NoError(t, err)
		assert.NotNil(t, survivor, "surviving members must stay active")

		return stats.Synced, scans
	}

	smallSynced, smallScans := passFor(t, 2)
	largeSynced, largeScans := passFor(t, 30)
	assert.Equal(t, 1, smallSynced,
		"only the changed member may re-sync after a sibling change")
	assert.Equal(t, smallSynced, largeSynced,
		"sibling-change writes must not scale with member count")
	assert.Positive(t, smallScans, "state.db opens must be instrumented")
	assert.Equal(t, smallScans, largeScans,
		"sibling-change state.db opens must not scale with member count")
}

func TestReconcileHermesSyncAllSeededStateMemberRemovalTombstones(
	t *testing.T,
) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, source, model, started_at, ended_at, message_count
		) VALUES (
			'survivor', 'discord', 'gpt-5.4', 1778767900.0, 1778768000.0, 1
		);
		INSERT INTO messages (
			session_id, role, content, timestamp
		) VALUES (
			'survivor', 'user', 'still here', 1778767910.0
		);
	`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Positive(t, engine.SyncAll(t.Context(), nil).Synced)
	stored, err := database.GetSessionFull(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, stored, "SyncAll must store the state member")

	conn, err = sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	_, err = conn.ExecContext(
		t.Context(), "DELETE FROM sessions WHERE id = 'child'",
	)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	))
	removed, err := database.GetSessionFull(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, removed)
	assert.NotNil(t, removed.SourceMissingAt)
	assert.Nil(t, removed.DeletedAt)
	survivor, err := database.GetSessionFull(t.Context(), "hermes:survivor")
	require.NoError(t, err)
	require.NotNil(t, survivor)
	assert.Nil(t, survivor.SourceMissingAt)
}

func TestReconcileHermesRelativeSessionsRootTombstonesRemovedStateMember(
	t *testing.T,
) {
	workingDir := t.TempDir()
	t.Chdir(workingDir)
	root := filepath.Join(workingDir, "archive")
	require.NoError(t, os.MkdirAll(root, 0o755))
	stateDB := writeHermesArchiveStateDB(t, root)
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	relativeSessionsDir, err := filepath.Rel(workingDir, sessionsDir)
	require.NoError(t, err)
	require.False(t, filepath.IsAbs(relativeSessionsDir))

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {relativeSessionsDir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	))
	stored, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, stored, "initial reconciliation must store the state member")

	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "DELETE FROM sessions WHERE id = 'child'")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	))
	stored, err = database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	assert.NotNil(t, stored,
		"absolute reconciliation scope must cover a configured relative root")
}

func TestReconcileHermesDefaultSessionsRootPreservesMissingStateDBArchive(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	))

	require.NoError(t, os.Remove(stateDB))
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	))
	stored, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	assert.NotNil(t, stored,
		"a missing persistent state.db cannot prove its archived members were deleted")
}

func TestReconcileHermesUnreadableStateDBSyncsTranscriptsWithoutTombstones(
	t *testing.T,
) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	))
	stored, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, stored, "initial reconciliation must store the state member")

	require.NoError(t, os.WriteFile(stateDB, []byte("not a sqlite database"), 0o600))
	jsonlPath := filepath.Join(sessionsDir, "orphan.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, []byte(
		`{"role":"session_meta","platform":"cli","timestamp":"2026-05-14T10:00:00Z"}`+"\n"+
			`{"role":"user","content":"fallback transcript","timestamp":"2026-05-14T10:01:00Z"}`+"\n"+
			`{"role":"assistant","content":"Done.","timestamp":"2026-05-14T10:02:00Z"}`+"\n",
	), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionsDir, "session_orphan.json"),
		[]byte(`{
			"platform":"cli",
			"session_start":"2026-05-14T10:00:00Z",
			"last_updated":"2026-05-14T10:02:00Z",
			"messages":[
				{"role":"user","content":"duplicate legacy transcript","timestamp":"2026-05-14T10:01:00Z"}
			]
		}`),
		0o600,
	))

	err = engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{sessionsDir}, false,
	)

	require.Error(t, err, "unreadable state discovery must keep the scope incomplete")
	result := engine.LastReconciliationResult()
	assert.False(t, result.Complete)
	assert.Equal(t, 1, result.ProviderFailures)
	stored, getErr := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, getErr)
	assert.NotNil(t, stored,
		"incomplete state discovery must not tombstone archived state-only sessions")
	fallback, getErr := database.GetSession(t.Context(), "hermes:orphan")
	require.NoError(t, getErr)
	require.NotNil(t, fallback,
		"transcript fallback candidates must still be processed before retry")
	assert.Equal(t, filepath.Clean(jsonlPath), database.GetSessionFilePath(t.Context(), "hermes:orphan"),
		"JSONL must remain canonical when a legacy JSON duplicate exists")
}

func TestReconcileHermesScopedRootPreservesStateMemberMovedToAnotherRoot(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	firstStateDB := writeHermesArchiveStateDB(t, firstRoot)
	firstSessions := filepath.Join(firstRoot, "sessions")
	secondSessions := filepath.Join(secondRoot, "sessions")
	require.NoError(t, os.MkdirAll(firstSessions, 0o755))
	require.NoError(t, os.MkdirAll(secondSessions, 0o755))

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {firstSessions, secondSessions},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), nil, true,
	))

	writeHermesArchiveStateDB(t, secondRoot)
	conn, err := sql.Open("sqlite3", firstStateDB)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "DELETE FROM sessions WHERE id = 'child'")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{firstSessions}, false,
	))
	stored, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	assert.NotNil(t, stored,
		"a scoped pass cannot disprove the same virtual member under another root")
}

func writeHermesArchiveStateDB(t *testing.T, root string) string {
	t.Helper()

	stateDB := filepath.Join(root, "state.db")
	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)

	_, err = conn.ExecContext(t.Context(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			user_id TEXT,
			model TEXT,
			model_config TEXT,
			system_prompt TEXT,
			parent_session_id TEXT,
			started_at REAL NOT NULL,
			ended_at REAL,
			end_reason TEXT,
			message_count INTEGER DEFAULT 0,
			tool_call_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			billing_provider TEXT,
			billing_base_url TEXT,
			billing_mode TEXT,
			estimated_cost_usd REAL,
			actual_cost_usd REAL,
			cost_status TEXT,
			cost_source TEXT,
			pricing_version TEXT,
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
			tool_name TEXT,
			timestamp REAL NOT NULL,
			token_count INTEGER,
			finish_reason TEXT,
			reasoning TEXT,
			reasoning_content TEXT,
			reasoning_details TEXT,
			codex_reasoning_items TEXT,
			codex_message_items TEXT
		);
		INSERT INTO sessions (
			id, source, model, started_at, ended_at, message_count
		) VALUES (
			'child', 'discord', 'gpt-5.4', 1778767200.0, 1778767800.0, 1
		);
		INSERT INTO messages (
			session_id, role, content, timestamp
		) VALUES (
			'child', 'user', 'state db message', 1778767210.0
		);
	`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	return stateDB
}

// TestReconcileHermesRemovedProfileTombstonesItsTranscripts pins what a
// removal event under a profiles container reclaims. The profile directory is
// gone by the time the pass resolves, so no owned profile matches and the
// request widens to the container. That scope proves the container without
// covering it, which is enough to stat the removed profile's stored
// transcripts and tombstone them while a live sibling's are retained.
func TestReconcileHermesRemovedProfileTombstonesItsTranscripts(t *testing.T) {
	container := filepath.Join(t.TempDir(), ".hermes", "profiles")
	writeHermesProfileTranscript(t, container, "research", "gone")
	writeHermesProfileTranscript(t, container, "writing", "kept")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {container},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

	removed := filepath.Join(container, "research")
	require.NoError(t, os.RemoveAll(removed))
	require.NoError(t, engine.ReconcileWatchRootsAfterLostEvents(
		t.Context(), []string{removed}, false,
	))

	stored, err := database.GetSession(t.Context(), "hermes:gone")
	require.NoError(t, err)
	assert.NotNil(t, stored,
		"a removal event must reclaim the deleted profile's transcripts")
	survivor, err := database.GetSession(t.Context(), "hermes:kept")
	require.NoError(t, err)
	assert.NotNil(t, survivor, "a live sibling profile keeps its sessions")
}

// TestReconcileHermesFlatRootDescendantTombstonesRemovedTranscript pins
// descendant resolution for a plain transcript-directory root: with no
// state.db and no sessions/ subdirectory there is no archive topology, so a
// requested transcript resolves generically — traverse the configured root,
// prove only the requested file — and a deleted transcript is reclaimed
// while its sibling keeps its session.
func TestReconcileHermesFlatRootDescendantTombstonesRemovedTranscript(
	t *testing.T,
) {
	root := t.TempDir()
	writeHermesTranscriptFile(t, root, "gone")
	writeHermesTranscriptFile(t, root, "kept")

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

	removed := filepath.Join(root, "gone.jsonl")
	require.NoError(t, os.Remove(removed))
	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentHermes, []string{removed},
	))

	stored, err := database.GetSession(t.Context(), "hermes:gone")
	require.NoError(t, err)
	assert.NotNil(t, stored,
		"a removed flat-root transcript must be reclaimed by its own request")
	survivor, err := database.GetSession(t.Context(), "hermes:kept")
	require.NoError(t, err)
	assert.NotNil(t, survivor,
		"a sibling transcript outside the proof keeps its session")
}

func writeHermesProfileTranscript(
	t *testing.T, container, profile, sessionID string,
) {
	t.Helper()
	writeHermesTranscriptFile(
		t, filepath.Join(container, profile, "sessions"), sessionID,
	)
}

func writeHermesTranscriptFile(t *testing.T, dir, sessionID string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, sessionID+".jsonl"), []byte(
			`{"role":"session_meta","platform":"cli","timestamp":"2026-05-14T10:00:00Z"}`+"\n"+
				`{"role":"user","content":"hello","timestamp":"2026-05-14T10:01:00Z"}`+"\n"+
				`{"role":"assistant","content":"Done.","timestamp":"2026-05-14T10:02:00Z"}`+"\n",
		), 0o600))
}

func TestHermesIncrementalSyncBoundsSiblingWrites(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		for _, mode := range []string{"full sync", "state event", "WAL event", "transcript event"} {
			t.Run(string(agent)+"/"+mode, func(t *testing.T) {
				for _, members := range []int{2, 30} {
					t.Run(strconv.Itoa(members), func(t *testing.T) {
						root := t.TempDir()
						stateDB := writeHermesArchiveStateDB(t, root)
						conn, err := sql.Open("sqlite3", stateDB)
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, conn.Close()) })
						if mode == "WAL event" {
							_, err = conn.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0")
							require.NoError(t, err)
						}
						for i := range members {
							id := fmt.Sprintf("member-%03d", i)
							_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions
								(id, source, model, started_at, ended_at, message_count)
								VALUES (?, 'cli', 'model', 1778767200, 1778767800, 1)`, id)
							require.NoError(t, err)
							_, err = conn.ExecContext(t.Context(), `INSERT INTO messages
								(session_id, role, content, timestamp) VALUES (?, 'user', 'hello', 1778767210)`, id)
							require.NoError(t, err)
						}
						writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "standalone")
						database := dbtest.OpenTestDB(t)
						engine := NewEngine(t.Context(), database, EngineConfig{
							AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
						})
						t.Cleanup(engine.Close)
						seed := engine.SyncAll(t.Context(), nil)
						require.Zero(t, seed.Failed)
						siblingIDs := []string{string(agent) + ":member-000", string(agent) + ":standalone"}
						before := make([]*db.Session, len(siblingIDs))
						for i, id := range siblingIDs {
							before[i], err = database.GetSessionFull(t.Context(), id)
							require.NoError(t, err)
							require.NotNil(t, before[i], id)
						}
						var scans atomic.Int64
						ctx := parser.WithSharedContainerScanObserver(t.Context(), func() { scans.Add(1) })
						if mode == "transcript event" {
							transcript := filepath.Join(root, "sessions", "child.jsonl")
							require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o755))
							require.NoError(t, os.WriteFile(transcript, []byte(`{"role":"user","content":"changed child prompt from transcript","timestamp":"2026-05-14T10:01:00Z"}`+"\n"), 0o600))
							require.NoError(t, engine.syncPathsContext(ctx, []string{transcript}, false))
						} else {
							_, err = conn.ExecContext(t.Context(), "UPDATE messages SET content = 'changed child prompt from transcript' WHERE session_id = 'child'")
							require.NoError(t, err)
							if mode == "full sync" {
								stats := engine.SyncAll(ctx, nil)
								assert.Equal(t, 1, stats.Synced)
								assert.Zero(t, stats.Failed)
							} else {
								eventPath := stateDB
								if mode == "WAL event" {
									eventPath += "-wal"
								}
								require.NoError(t, engine.syncPathsContext(ctx, []string{eventPath}, false))
							}
						}
						for i, id := range siblingIDs {
							after, err := database.GetSessionFull(t.Context(), id)
							require.NoError(t, err)
							assert.Equal(t, before[i], after, "unchanged sibling %s must receive no archive writes", id)
						}
						child, err := database.GetSessionFull(t.Context(), string(agent)+":child")
						require.NoError(t, err)
						require.NotNil(t, child)
						assert.Equal(t, new("changed child prompt from transcript"), child.FirstMessage)
						assert.Positive(t, scans.Load())
						// A state.db/WAL event's orphan-transcript fallback
						// (surfacing a transcript whose row disappeared, see
						// TestHermesStateEventSurfacesTranscriptAfterRowRemoved)
						// adds one fixed extra state.db open to check
						// transcript membership. That's a constant cost, not
						// one that scales with archive size: 2 and 30
						// members both land on the same count, which is what
						// this assertion must keep proving as the bound
						// grows with future fixed overhead.
						assert.LessOrEqual(t, scans.Load(), int64(6), "container opens must stay bounded as the archive grows")
					})
				}
			})
		}
	}
}

// TestHermesFingerprintSeedingDoesNotParseSiblingTranscripts proves
// fingerprint-cache seeding detects an unchanged sibling by hashing its
// transcript's bytes, not by fully decoding it the way a real Parse does.
// Before this, seeding called chooseHermesStateSessionSource (a full
// JSON/JSONL decode) on every member just to pick a selected path for
// stat purposes, so an unchanged full sync still paid parse cost that
// scaled with archive size instead of staying flat.
func TestHermesFingerprintSeedingDoesNotParseSiblingTranscripts(t *testing.T) {
	for _, members := range []int{2, 30} {
		t.Run(strconv.Itoa(members), func(t *testing.T) {
			root := t.TempDir()
			stateDB := writeHermesArchiveStateDB(t, root)
			conn, err := sql.Open("sqlite3", stateDB)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })
			for i := range members {
				id := fmt.Sprintf("member-%03d", i)
				_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions
					(id, source, model, started_at, ended_at, message_count)
					VALUES (?, 'cli', 'model', 1778767200, 1778767800, 1)`, id)
				require.NoError(t, err)
				writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), id)
			}
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

			before := parser.HermesTranscriptParses()
			stats := engine.SyncAll(t.Context(), nil)
			assert.Zero(t, stats.Synced, "an unchanged full sync must not resync any sibling")
			assert.Zero(t, stats.Failed)
			after := parser.HermesTranscriptParses()
			assert.Zero(t, after-before,
				"fingerprint-cache seeding must not decode any sibling transcript on an unchanged full sync")
		})
	}
}

// TestHermesFingerprintCacheRootEligibility proves
// providerFingerprintCacheRootEligible/withProviderFingerprintCache decide
// whether to attach the batch fingerprint cache correctly across every root
// shape Hermes and Augure Desktop can be configured with: a flat root that
// exists on disk, a root that does not exist anywhere, a "sessions"
// directory that has not been created yet but whose sibling state.db has,
// and a profiles container (.hermes/profiles) that expands to per-profile
// archive roots during discovery. A flat os.Stat on the configured root
// alone cannot see any of the latter three, so without resolving them the
// way discovery does, the cache never activated for those layouts and
// every sibling member reopened state.db on its own; conversely, a cache
// that merely differs in context identity without actually bounding opens
// would pass a bare context-identity check, so every case counts real
// database opens instead.
func TestHermesFingerprintCacheRootEligibility(t *testing.T) {
	for _, test := range []struct {
		name             string
		buildRoots       func(t *testing.T) (archiveRoot, configuredRoot string)
		wantBoundedOpens bool
	}{
		{
			name: "existing root",
			buildRoots: func(t *testing.T) (string, string) {
				t.Helper()
				root := t.TempDir()
				return root, root
			},
			wantBoundedOpens: true,
		},
		{
			name: "missing root",
			buildRoots: func(t *testing.T) (string, string) {
				t.Helper()
				return t.TempDir(), filepath.Join(t.TempDir(), "missing")
			},
			wantBoundedOpens: false,
		},
		{
			name: "sessions dir not yet created",
			buildRoots: func(t *testing.T) (string, string) {
				t.Helper()
				root := t.TempDir()
				return root, filepath.Join(root, "sessions")
			},
			wantBoundedOpens: true,
		},
		{
			name: "profiles container",
			buildRoots: func(t *testing.T) (string, string) {
				t.Helper()
				profilesRoot := filepath.Join(t.TempDir(), ".hermes", "profiles")
				profileRoot := filepath.Join(profilesRoot, "research")
				require.NoError(t, os.MkdirAll(profileRoot, 0o755))
				return profileRoot, profilesRoot
			},
			wantBoundedOpens: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
				t.Run(string(agent), func(t *testing.T) {
					archiveRoot, configuredRoot := test.buildRoots(t)
					writeHermesArchiveStateDB(t, archiveRoot)
					execHermesState(t, filepath.Join(archiveRoot, "state.db"), `INSERT INTO sessions
						(id, source, model, started_at, ended_at, message_count)
						VALUES ('sibling', 'cli', 'model', 1778767300, 1778767900, 1)`)
					execHermesState(t, filepath.Join(archiveRoot, "state.db"), `INSERT INTO messages
						(session_id, role, content, timestamp)
						VALUES ('sibling', 'user', 'sibling message', 1778767310)`)

					provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{archiveRoot}})
					require.True(t, ok)
					sources, err := provider.Discover(t.Context())
					require.NoError(t, err)
					require.Len(t, sources, 2, "both sibling members must be discovered")

					engine := NewEngine(t.Context(), dbtest.OpenTestDB(t), EngineConfig{
						AgentDirs: map[parser.AgentType][]string{agent: {configuredRoot}}, Machine: "local",
					})
					t.Cleanup(engine.Close)

					ctx, closeCache, err := engine.withProviderFingerprintCache(t.Context(), nil)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, closeCache()) })

					var scans atomic.Int64
					scanCtx := parser.WithSharedContainerScanObserver(ctx, func() { scans.Add(1) })
					for _, source := range sources {
						_, err := provider.Fingerprint(scanCtx, source)
						require.NoError(t, err)
					}

					if test.wantBoundedOpens {
						assert.LessOrEqual(t, scans.Load(), int64(1),
							"a root the cache recognizes must bound the two siblings to one database open")
					} else {
						assert.Equal(t, int64(2), scans.Load(),
							"without an eligible root each sibling must open the database on its own")
					}
				})
			}
		})
	}
}

func TestHermesMemberDiscoveryMigratesLegacyArchivePaths(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	legacy := engine.processFile(t.Context(), parser.DiscoveredFile{Path: stateDB, Agent: parser.AgentHermes})
	require.NoError(t, legacy.err)
	require.Len(t, legacy.results, 1)
	result := legacy.results[0]
	written, _, failed, _ := engine.writeBatch([]pendingWrite{{
		sess: result.Session, msgs: result.Messages, usageEvents: result.UsageEvents,
	}}, syncWriteDefault, true)
	require.Equal(t, 1, written)
	require.Zero(t, failed)
	require.Equal(t, stateDB, database.GetSessionFilePath(t.Context(), "hermes:child"))
	before, err := database.GetSessionFull(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, before)
	stats := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, stats.Synced)
	assert.Zero(t, stats.Failed)
	assert.Equal(t, parser.VirtualSourcePath(stateDB, "child"), database.GetSessionFilePath(t.Context(), "hermes:child"))
	after, err := database.GetSessionFull(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, before.TranscriptRevision, after.TranscriptRevision)
	assert.Nil(t, after.SourceMissingAt)
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
}

// TestHermesSingleSessionRefreshDoesNotUnmigrateLegacySibling proves
// FindSource checks the requested session ID before matching a stored bare
// "state.db" hint -- the FilePath every session got under the pre-PR,
// whole-archive write. Without that check, refreshing one never-migrated
// legacy session resolves to the whole-archive source (since its stored
// hint is the bare archive path), and SyncSingleSessionContext then
// reparses and ForceReplaces every session in state.db, undoing any
// sibling that had already migrated to its own per-session path.
func TestHermesSingleSessionRefreshDoesNotUnmigrateLegacySibling(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			stateDB := writeHermesArchiveStateDB(t, root)
			execHermesState(t, stateDB, `INSERT INTO sessions
				(id, source, model, started_at, ended_at, message_count)
				VALUES ('sibling', 'cli', 'model', 1778767300, 1778767900, 1)`)
			execHermesState(t, stateDB, `INSERT INTO messages
				(session_id, role, content, timestamp)
				VALUES ('sibling', 'user', 'sibling message', 1778767310)`)

			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)

			// Seed both sessions the pre-PR way: one whole-archive parse,
			// written with FilePath stamped as the bare state.db path for
			// every session, not a per-session member path.
			legacy := engine.processFile(t.Context(), parser.DiscoveredFile{Path: stateDB, Agent: agent})
			require.NoError(t, legacy.err)
			require.Len(t, legacy.results, 2)
			writes := make([]pendingWrite, len(legacy.results))
			for i, result := range legacy.results {
				writes[i] = pendingWrite{sess: result.Session, msgs: result.Messages, usageEvents: result.UsageEvents}
			}
			written, _, failed, _ := engine.writeBatch(writes, syncWriteDefault, true)
			require.Equal(t, len(legacy.results), written)
			require.Zero(t, failed)
			childID := string(agent) + ":child"
			siblingID := string(agent) + ":sibling"
			require.Equal(t, stateDB, database.GetSessionFilePath(t.Context(), childID))
			require.Equal(t, stateDB, database.GetSessionFilePath(t.Context(), siblingID))

			// Migrate the sibling to its own per-session path via its own
			// transcript event, leaving "child" on the legacy bare path.
			writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "sibling")
			require.NoError(t, engine.syncPathsContext(
				t.Context(), []string{filepath.Join(root, "sessions", "sibling.jsonl")}, false,
			))
			require.Equal(t, parser.VirtualSourcePath(stateDB, "sibling"),
				database.GetSessionFilePath(t.Context(), siblingID),
				"the sibling must migrate to its own per-session path")

			// Refresh the still-legacy "child" session by ID.
			require.NoError(t, engine.SyncSingleSessionContext(t.Context(), childID))

			assert.Equal(t, parser.VirtualSourcePath(stateDB, "child"),
				database.GetSessionFilePath(t.Context(), childID),
				"refreshing a legacy session must resolve its own member, not the whole archive")
			assert.Equal(t, parser.VirtualSourcePath(stateDB, "sibling"),
				database.GetSessionFilePath(t.Context(), siblingID),
				"refreshing an unrelated legacy session must not undo a sibling's migration")
		})
	}
}

// TestHermesSingleSessionRefreshOfDeletedLegacySessionIsNoOp proves the
// Hermes/Augure Desktop carve-out in Engine.FindSourceFile: when a legacy
// session's refresh resolves to not-found (its state.db row AND its
// transcript are both genuinely gone, not just migrated to its own
// per-session path), FindSourceFile must trust that authoritative
// not-found instead of falling back to the stored bare state.db path --
// that fallback is exactly the whole-archive-reparse harm
// TestHermesSingleSessionRefreshDoesNotUnmigrateLegacySibling fixed for the
// migrated case, reached through a different caller.
func TestHermesSingleSessionRefreshOfDeletedLegacySessionIsNoOp(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			stateDB := writeHermesArchiveStateDB(t, root)
			execHermesState(t, stateDB, `INSERT INTO sessions
				(id, source, model, started_at, ended_at, message_count)
				VALUES ('sibling', 'cli', 'model', 1778767300, 1778767900, 1)`)
			execHermesState(t, stateDB, `INSERT INTO messages
				(session_id, role, content, timestamp)
				VALUES ('sibling', 'user', 'sibling message', 1778767310)`)

			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)

			// Seed both sessions the pre-PR way: FilePath stamped as the
			// bare state.db path for every session.
			legacy := engine.processFile(t.Context(), parser.DiscoveredFile{Path: stateDB, Agent: agent})
			require.NoError(t, legacy.err)
			require.Len(t, legacy.results, 2)
			writes := make([]pendingWrite, len(legacy.results))
			for i, result := range legacy.results {
				writes[i] = pendingWrite{sess: result.Session, msgs: result.Messages, usageEvents: result.UsageEvents}
			}
			written, _, failed, _ := engine.writeBatch(writes, syncWriteDefault, true)
			require.Equal(t, len(legacy.results), written)
			require.Zero(t, failed)
			childID := string(agent) + ":child"
			siblingID := string(agent) + ":sibling"

			// Migrate the sibling to its own per-session path.
			writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "sibling")
			require.NoError(t, engine.syncPathsContext(
				t.Context(), []string{filepath.Join(root, "sessions", "sibling.jsonl")}, false,
			))
			siblingBefore, err := database.GetSessionFull(t.Context(), siblingID)
			require.NoError(t, err)
			require.Equal(t, parser.VirtualSourcePath(stateDB, "sibling"), *siblingBefore.FilePath)

			// "child" is now genuinely gone: its row is removed and it
			// never had a transcript file.
			execHermesState(t, stateDB, "DELETE FROM sessions WHERE id = 'child'")

			err = engine.SyncSingleSessionContext(t.Context(), childID)
			require.Error(t, err, "refreshing a session with no row and no transcript must not succeed")

			siblingAfter, err := database.GetSessionFull(t.Context(), siblingID)
			require.NoError(t, err)
			assert.Equal(t, siblingBefore, siblingAfter,
				"a deleted legacy session's failed refresh must not touch an unrelated migrated sibling")
		})
	}
}

func TestHermesPartialDiscoverySyncsReadableTranscripts(t *testing.T) {
	for _, test := range []struct {
		damage string
		mode   string
	}{
		{damage: "malformed database", mode: "full sync"},
		{damage: "malformed database", mode: "state event"},
		{damage: "malformed database", mode: "transcript event"},
		{damage: "incompatible schema", mode: "full sync"},
		{damage: "incompatible schema", mode: "transcript event"},
		{damage: "missing messages table", mode: "full sync"},
		{damage: "missing messages table", mode: "state event"},
		{damage: "missing messages table", mode: "transcript event"},
	} {
		t.Run(test.damage+"/"+test.mode, func(t *testing.T) {
			root := t.TempDir()
			stateDB := writeHermesArchiveStateDB(t, root)
			healthyRoot := t.TempDir()
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentHermes: {root, healthyRoot},
				},
				Machine: "local",
			})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			before, err := database.GetSessionFull(t.Context(), "hermes:child")
			require.NoError(t, err)
			require.NotNil(t, before)

			switch test.damage {
			case "malformed database":
				require.NoError(t, os.WriteFile(stateDB, []byte("not a sqlite database"), 0o600))
			case "incompatible schema":
				require.NoError(t, os.Remove(stateDB))
				execHermesState(t, stateDB, "CREATE TABLE unrelated (id TEXT PRIMARY KEY)")
			case "missing messages table":
				execHermesState(t, stateDB, "DROP TABLE messages")
			}
			dir := filepath.Join(root, "sessions")
			writeHermesTranscriptFile(t, dir, "fallback")
			writeHermesTranscriptFile(t, healthyRoot, "healthy")
			healthy := filepath.Join(healthyRoot, "healthy.jsonl")
			var reported bool
			switch test.mode {
			case "full sync":
				reported = engine.SyncAll(t.Context(), nil).Failed > 0
			case "state event":
				reported = engine.syncPathsContext(t.Context(), []string{stateDB, healthy}, false) != nil
			case "transcript event":
				reported = engine.syncPathsContext(t.Context(), []string{filepath.Join(dir, "fallback.jsonl"), healthy}, false) != nil
			}

			assert.True(t, reported, "incomplete discovery must be reported")
			for _, id := range []string{"hermes:fallback", "hermes:healthy"} {
				assert.Equal(t, []string{"hello", "Done."}, messageContents(t, database, id), id)
			}
			after, err := database.GetSessionFull(t.Context(), "hermes:child")
			require.NoError(t, err)
			assert.Equal(t, before, after, "incomplete discovery must preserve state-only history")
		})
	}
}

func TestHermesSyncAllSinceIncludesWALOnlyEdits(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE)")
	require.NoError(t, err)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

	_, err = conn.ExecContext(t.Context(), "UPDATE messages SET content = 'WAL-only edit' WHERE session_id = 'child'")
	require.NoError(t, err)
	old := time.Now().Add(-2 * time.Hour)
	cutoff := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(stateDB, old, old))
	walInfo, err := os.Stat(stateDB + "-wal")
	require.NoError(t, err)
	require.True(t, walInfo.ModTime().After(cutoff))

	stats := engine.SyncAllSince(t.Context(), cutoff, nil)
	assert.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, []string{"WAL-only edit"}, messageContents(t, database, "hermes:child"))
}

func TestHermesDottedCronIDsSyncAndResolve(t *testing.T) {
	const id = "cron_job.daily_20261012_120000"
	for _, test := range []struct {
		mode string
		want []string
	}{
		{mode: "full sync", want: []string{"state db message"}},
		{mode: "state event", want: []string{"state db message"}},
		{mode: "WAL event", want: []string{"state db message"}},
		{mode: "transcript only", want: []string{"hello", "Done."}},
	} {
		t.Run(test.mode, func(t *testing.T) {
			root := t.TempDir()
			event := filepath.Join(root, "state.db")
			if test.mode == "transcript only" {
				writeHermesTranscriptFile(t, root, id)
			} else {
				stateDB := writeHermesArchiveStateDB(t, root)
				conn, err := sql.Open("sqlite3", stateDB)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, conn.Close()) })
				_, err = conn.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; UPDATE sessions SET id = ?, source = 'cron'; UPDATE messages SET session_id = ?`, id, id)
				require.NoError(t, err)
				if test.mode == "WAL event" {
					event += "-wal"
				}
			}
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			switch test.mode {
			case "state event", "WAL event":
				require.NoError(t, engine.syncPathsContext(t.Context(), []string{event}, false))
			default:
				require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			}

			fullID := "hermes:" + id
			assert.Equal(t, test.want, messageContents(t, database, fullID))
			require.NoError(t, engine.SyncSingleSessionContext(t.Context(), fullID),
				"single-session sync must resolve the dotted cron ID")
		})
	}
}

func TestHermesTranscriptBesideStateDBReplacesMessages(t *testing.T) {
	edited := `{"role":"user","content":"edited prompt","timestamp":"2026-05-14T10:01:00Z"}` + "\n" +
		`{"role":"assistant","content":"corrected reply","timestamp":"2026-05-14T10:02:00Z"}` + "\n"
	for _, test := range []struct {
		name       string
		id         string
		change     func(t *testing.T, root string) string
		event      bool
		incomplete bool
		want       []string
	}{
		{
			name: "full sync applies an edit",
			id:   "standalone",
			change: func(t *testing.T, root string) string {
				t.Helper()
				return writeHermesSessionsFile(t, root, "standalone.jsonl", edited)
			},
			want: []string{"edited prompt", "corrected reply"},
		},
		{
			name: "transcript event applies an edit",
			id:   "standalone",
			change: func(t *testing.T, root string) string {
				t.Helper()
				return writeHermesSessionsFile(t, root, "standalone.jsonl", edited)
			},
			event: true,
			want:  []string{"edited prompt", "corrected reply"},
		},
		{
			name: "emptied transcript keeps history",
			id:   "standalone",
			change: func(t *testing.T, root string) string {
				t.Helper()
				return writeHermesSessionsFile(t, root, "standalone.jsonl", "")
			},
			want: []string{"hello", "Done."},
		},
		{
			name: "database session falls back to its transcript",
			id:   "child",
			change: func(t *testing.T, root string) string {
				t.Helper()
				writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "child")
				execHermesState(t, filepath.Join(root, "state.db"), "DROP TABLE messages")
				return filepath.Join(root, "sessions", "child.jsonl")
			},
			incomplete: true,
			want:       []string{"hello", "Done."},
		},
		{
			name: "transcript event falls back from an unreadable database session",
			id:   "child",
			change: func(t *testing.T, root string) string {
				t.Helper()
				writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "child")
				execHermesState(t, filepath.Join(root, "state.db"), "DROP TABLE messages")
				return filepath.Join(root, "sessions", "child.jsonl")
			},
			event:      true,
			incomplete: true,
			want:       []string{"hello", "Done."},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeHermesArchiveStateDB(t, root)
			writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "standalone")
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

			path := test.change(t, root)
			var incomplete bool
			if test.event {
				incomplete = engine.syncPathsContext(t.Context(), []string{path}, false) != nil
			} else {
				incomplete = engine.SyncAll(t.Context(), nil).Failed > 0
			}
			assert.Equal(t, test.incomplete, incomplete)

			assert.Equal(t, test.want, messageContents(t, database, "hermes:"+test.id))
		})
	}
}

// TestHermesSingleSessionSyncAfterStateDBDamage pins FindSource's stored
// member-path hint against current state.db membership, not just the
// database file's existence: when a synced session's row disappears but
// its transcript remains, refreshing that single session must fall back
// to the transcript instead of erroring or returning nothing. It covers
// both ways the row can disappear: the whole state.db file removed, and
// just the row deleted from an otherwise intact state.db.
func TestHermesSingleSessionSyncAfterStateDBDamage(t *testing.T) {
	for _, test := range []struct {
		name   string
		damage func(t *testing.T, stateDB string)
	}{
		{
			name: "state.db removed",
			damage: func(t *testing.T, stateDB string) {
				t.Helper()
				require.NoError(t, os.Remove(stateDB))
			},
		},
		{
			name: "row removed",
			damage: func(t *testing.T, stateDB string) {
				t.Helper()
				execHermesState(t, stateDB, "DELETE FROM sessions WHERE id = 'child'")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			stateDB := writeHermesArchiveStateDB(t, root)
			writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "child")
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			require.Equal(t, parser.VirtualSourcePath(stateDB, "child"),
				database.GetSessionFilePath(t.Context(), "hermes:child"))

			test.damage(t, stateDB)

			require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "hermes:child"))
			assert.Equal(t, filepath.Join(root, "sessions", "child.jsonl"),
				database.GetSessionFilePath(t.Context(), "hermes:child"))
			assert.Equal(t, []string{"hello", "Done."}, messageContents(t, database, "hermes:child"))
		})
	}
}

// TestHermesMalformedSessionRowFallsBackToReadableTranscript proves a
// session whose own state.db row can't be fully decoded (a malformed
// started_at value, not merely a missing one) still syncs, using its
// readable transcript, instead of silently losing that transcript --
// the same per-session isolation this provider already guarantees for a
// parse failure. The malformed row's own virtual member still reports a
// visible, observable parse failure (hermesStateMembership.Has, used by
// the orphan-transcript fallback, no longer counts an undecodable row as
// membership, so it stopped suppressing this session's transcript, but
// the row's own doomed member keeps surfacing its own decode error). An
// unrelated sibling in the same state.db must stay unaffected.
func TestHermesMalformedSessionRowFallsBackToReadableTranscript(t *testing.T) {
	for _, test := range []struct {
		name   string
		damage string
	}{
		{
			name:   "malformed started_at",
			damage: "UPDATE sessions SET started_at = 'not-a-number' WHERE id = 'child'",
		},
		{
			name:   "malformed ended_at with a valid started_at",
			damage: "UPDATE sessions SET ended_at = 'not-a-number' WHERE id = 'child'",
		},
		{
			name:   "a message that fails to decode",
			damage: "UPDATE messages SET timestamp = 'not-a-number' WHERE session_id = 'child'",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			stateDB := writeHermesArchiveStateDB(t, root)
			execHermesState(t, stateDB, `INSERT INTO sessions
				(id, source, model, started_at, ended_at, message_count)
				VALUES ('sibling', 'cli', 'model', 1778767300, 1778767900, 1)`)
			execHermesState(t, stateDB, `INSERT INTO messages
				(session_id, role, content, timestamp)
				VALUES ('sibling', 'user', 'sibling message', 1778767310)`)
			execHermesState(t, stateDB, test.damage)
			writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "child")

			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)

			stats := engine.SyncAll(t.Context(), nil)
			assert.Positive(t, stats.Failed,
				"the malformed row's own member must still report a visible, observable decode failure")
			assert.Equal(t, []string{"hello", "Done."}, messageContents(t, database, "hermes:child"),
				"a session whose row or messages can't fully decode must still sync from its readable transcript")
			assert.Equal(t, []string{"sibling message"}, messageContents(t, database, "hermes:sibling"),
				"an unrelated sibling must be unaffected by another row's malformed value")
		})
	}
}

// TestHermesUnreadableJSONSnapshotFallsBackToOtherSources proves an
// unreadable session_<id>.json -- an optional legacy snapshot candidate --
// does not block a session whose state.db row and .jsonl transcript are
// both fine. Before this, readHermesStateSessionFingerprintInputs listed
// every existing candidate including one that can't be read, and
// hermesStateMemberFingerprintFinish failed the whole fingerprint the
// moment it couldn't open that one candidate, blocking the session from
// syncing at all even though its other sources were perfectly usable.
// Fixing the file's permissions afterward must still invalidate the
// fingerprint and trigger a resync, so skipping an unreadable candidate
// can't leave a stuck, stale fingerprint once the candidate is readable.
func TestHermesUnreadableJSONSnapshotFallsBackToOtherSources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod(0) does not prevent reads on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root can read mode-0 files")
	}

	root := t.TempDir()
	writeHermesArchiveStateDB(t, root)
	sessionsDir := filepath.Join(root, "sessions")
	writeHermesTranscriptFile(t, sessionsDir, "child")
	jsonPath := writeHermesSessionsFile(t, root, "session_child.json",
		`{"role":"session_meta","timestamp":"2026-05-14T09:00:00Z"}`+"\n")
	require.NoError(t, os.Chmod(jsonPath, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(jsonPath, 0o600)) })

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Failed, "an unreadable optional snapshot must not fail the session's fingerprint")
	assert.Equal(t, []string{"hello", "Done."}, messageContents(t, database, "hermes:child"),
		"the session must still sync from its readable sources")

	require.NoError(t, os.Chmod(jsonPath, 0o600))
	stats = engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Synced,
		"a candidate becoming readable must still invalidate the fingerprint and resync")
}

// TestHermesStateEventSurfacesTranscriptAfterRowRemoved proves a state.db
// change event picks up a session whose row was deleted since the last
// sync, without waiting for the next full sync. changedPathSources's
// state.db/WAL branch used to enumerate only current rows via
// discoverStateEach, so a deleted row's surviving transcript stayed
// unsynced -- stale database-sourced content lingered in the archive --
// until some later full sync happened to run.
func TestHermesStateEventSurfacesTranscriptAfterRowRemoved(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "child")
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	require.Equal(t, parser.VirtualSourcePath(stateDB, "child"),
		database.GetSessionFilePath(t.Context(), "hermes:child"))

	execHermesState(t, stateDB, "DELETE FROM sessions WHERE id = 'child'")

	require.NoError(t, engine.syncPathsContext(t.Context(), []string{stateDB}, false))

	assert.Equal(t, filepath.Join(root, "sessions", "child.jsonl"),
		database.GetSessionFilePath(t.Context(), "hermes:child"),
		"a state.db event must surface a transcript whose row disappeared, without a full sync")
	assert.Equal(t, []string{"hello", "Done."}, messageContents(t, database, "hermes:child"))
}

// TestHermesSingleSessionSyncSwitchesToNewStateDBRow pins the opposite
// direction: a session first synced from its transcript because state.db had
// no row for it yet, then later gets a real row added. Refreshing that
// single session must prefer the new state.db row so the database's
// messages, title, and usage/cost get picked back up instead of being stuck
// on the stale transcript hint.
func TestHermesSingleSessionSyncSwitchesToNewStateDBRow(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "late")
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	require.Equal(t, filepath.Join(root, "sessions", "late.jsonl"),
		database.GetSessionFilePath(t.Context(), "hermes:late"),
		"a session never rowed in state.db must sync from its transcript")

	execHermesState(t, stateDB, `INSERT INTO sessions
		(id, source, model, started_at, ended_at, message_count)
		VALUES ('late', 'cli', 'model', 1778767200, 1778767800, 2)`)
	// Hermes picks whichever source has the richer content when both the
	// database row and the transcript exist for a session, so these two
	// database messages must out-score the transcript's "hello"/"Done." pair
	// for this assertion to prove the database row, not just its presence,
	// won the content selection.
	execHermesState(t, stateDB, `INSERT INTO messages
		(session_id, role, content, timestamp)
		VALUES ('late', 'user', 'a considerably longer state db message one', 1778767210)`)
	execHermesState(t, stateDB, `INSERT INTO messages
		(session_id, role, content, timestamp)
		VALUES ('late', 'assistant', 'a considerably longer state db message two', 1778767220)`)

	require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "hermes:late"))
	assert.Equal(t, parser.VirtualSourcePath(stateDB, "late"),
		database.GetSessionFilePath(t.Context(), "hermes:late"))
	assert.Equal(t, []string{
		"a considerably longer state db message one",
		"a considerably longer state db message two",
	}, messageContents(t, database, "hermes:late"))
}

// TestHermesSingleSessionRefreshPrefersNewerJSONLOverStaleJSONSnapshot
// proves a transcript-only session first stored from the legacy
// session_<id>.json snapshot refreshes from a newer <id>.jsonl once one
// appears for the same session ID, instead of staying stuck rereading the
// stored path's exact, now-superseded file. FindSource's stored-hint loop
// used to trust that exact path directly for a non-member transcript hint,
// bypassing the JSONL-preferred-over-JSON selection discovery and
// changed-path handling already apply through findHermesSourceFile.
func TestHermesSingleSessionRefreshPrefersNewerJSONLOverStaleJSONSnapshot(t *testing.T) {
	for _, test := range []struct {
		name        string
		withArchive bool
	}{
		{name: "no state.db at all"},
		{name: "state.db present without this session's row", withArchive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			sessionsDir := filepath.Join(root, "sessions")
			require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
			if test.withArchive {
				// writeHermesArchiveStateDB seeds an unrelated "child"
				// member; "standalone" never gets a row in this state.db.
				writeHermesArchiveStateDB(t, root)
			}
			writeHermesSessionsFile(t, root, "session_standalone.json",
				`{"messages":[{"role":"user","content":"snapshot prompt"}]}`)

			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			require.Equal(t, []string{"snapshot prompt"}, messageContents(t, database, "hermes:standalone"))
			require.Equal(t, filepath.Join(sessionsDir, "session_standalone.json"),
				database.GetSessionFilePath(t.Context(), "hermes:standalone"))

			writeHermesSessionsFile(t, root, "standalone.jsonl",
				`{"role":"user","content":"jsonl prompt","timestamp":"2026-05-14T10:01:00Z"}`+"\n")

			require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "hermes:standalone"))

			assert.Equal(t, []string{"jsonl prompt"}, messageContents(t, database, "hermes:standalone"),
				"refresh must prefer the newer jsonl over the stale stored json snapshot")
			assert.Equal(t, filepath.Join(sessionsDir, "standalone.jsonl"),
				database.GetSessionFilePath(t.Context(), "hermes:standalone"))
		})
	}
}

func TestHermesSingleSessionSyncKeepsTranscriptSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(t *testing.T, root string)
		want   []string
	}{
		{
			name: "edited transcript",
			change: func(t *testing.T, root string) {
				t.Helper()
				writeHermesSessionsFile(t, root, "standalone.jsonl",
					`{"role":"user","content":"edited prompt","timestamp":"2026-05-14T10:01:00Z"}`+"\n")
			},
			want: []string{"edited prompt"},
		},
		{
			name: "JSONL removed beside a JSON snapshot",
			change: func(t *testing.T, root string) {
				t.Helper()
				writeHermesSessionsFile(t, root, "session_standalone.json",
					`{"messages":[{"role":"user","content":"snapshot prompt"}]}`)
				require.NoError(t, os.Remove(filepath.Join(root, "sessions", "standalone.jsonl")))
			},
			want: []string{"snapshot prompt"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			stateDB := writeHermesArchiveStateDB(t, root)
			writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "standalone")
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			test.change(t, root)

			require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "hermes:standalone"))

			assert.Equal(t, test.want, messageContents(t, database, "hermes:standalone"))
			assert.Equal(t, parser.VirtualSourcePath(stateDB, "child"),
				database.GetSessionFilePath(t.Context(), "hermes:child"),
				"refreshing one transcript must not reparse the whole archive")
		})
	}
}

// TestParseDiffHermesLimitScopesPerSession is the per-session-keying acid
// test for Hermes/Augure Desktop's "state.db#<sessionID>" virtual path. A
// shared state.db holds two sessions; --limit 1 samples one and cuts the
// other. Before isPerSessionDBVirtualSource recognized "state.db", the cut
// sibling's virtual path collapsed to the bare state.db path and the
// --limit sample (or a parse failure) would fan out across both sessions.
func TestParseDiffHermesLimitScopesPerSession(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	execHermesState(t, stateDB, `INSERT INTO sessions
		(id, source, model, started_at, ended_at, message_count)
		VALUES ('sibling', 'cli', 'model', 1778767300, 1778767900, 1)`)
	execHermesState(t, stateDB, `INSERT INTO messages
		(session_id, role, content, timestamp)
		VALUES ('sibling', 'user', 'sibling message', 1778767310)`)

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

	diffEngine := NewDiffEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(diffEngine.Close)
	report, err := diffEngine.ParseDiff(t.Context(), ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentHermes},
		Limit:  1,
	})
	require.NoError(t, err)

	assert.True(t, report.FilesLimited, "files limited")
	assert.Equal(t, ParseDiffTotals{
		Examined: 1, Identical: 1, Skipped: 1,
	}, report.Totals, "one session sampled, one cut")
	assert.Zero(t, report.Totals.Changed,
		"the untouched sibling must not be reported as changed")

	var skipped []SessionDiff
	for _, s := range report.Sessions {
		if s.Class == DiffSkipped {
			skipped = append(skipped, s)
		}
	}
	require.Len(t, skipped, 1, "exactly one skipped session listed")
	assert.Contains(t, skipped[0].Reason, "limit",
		"cut sibling must read as not-sampled, not as 'database-backed agent'")
}

// TestParseDiffHermesLimitOrdersByMemberRecency proves a --limit sample
// orders a Hermes database member by its own real freshness instead of an
// arbitrary zero: discoveredFileMtime's os.Stat fallback can't see a
// virtual "state.db#<id>" member path's on-disk existence, so every
// member used to land on mtime zero, and --limit could cut a just-changed
// session in favor of an old standalone transcript. The database member's
// row timestamp (stamped onto SourceRef.DiscoveryMTimeNS during discovery)
// now gives it a real recency to order by.
func TestParseDiffHermesLimitOrdersByMemberRecency(t *testing.T) {
	root := t.TempDir()
	writeHermesArchiveStateDB(t, root)
	stateDB := filepath.Join(root, "state.db")
	// A standalone transcript with an old file mtime: no database row, so
	// its ordering value still comes from discoveredFileMtime's os.Stat.
	writeHermesTranscriptFile(t, filepath.Join(root, "sessions"), "old")
	oldPath := filepath.Join(root, "sessions", "old.jsonl")
	oldTime := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(oldPath, oldTime, oldTime))

	// A database member just "changed": its row timestamp is real wall
	// time, always newer than the standalone transcript's 2020 mtime.
	fresh := time.Now()
	execHermesState(t, stateDB, fmt.Sprintf(`INSERT INTO sessions
		(id, source, model, started_at, ended_at, message_count)
		VALUES ('fresh', 'cli', 'model', %d, %d, 1)`,
		fresh.Unix(), fresh.Unix()))
	execHermesState(t, stateDB, `INSERT INTO messages
		(session_id, role, content, timestamp)
		VALUES ('fresh', 'user', 'fresh message', 1778767210)`)

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

	diffEngine := NewDiffEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(diffEngine.Close)
	report, err := diffEngine.ParseDiff(t.Context(), ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentHermes},
		Limit:  1,
	})
	require.NoError(t, err)
	require.True(t, report.FilesLimited, "files limited")

	var skipped []string
	for _, s := range report.Sessions {
		if s.Class == DiffSkipped {
			skipped = append(skipped, s.SessionID)
		}
	}
	assert.NotContains(t, skipped, "hermes:fresh",
		"the just-changed database member must be the one sampled, not cut")
}

// TestParseDiffHermesPerSessionParseErrorStaysScoped proves a malformed
// session inside a shared state.db surfaces as DiffParseError attributed to
// its own "state.db#<sessionID>" virtual path, leaving an unrelated sibling
// session Identical. Before isPerSessionDBVirtualSource recognized
// "state.db", both sessions' virtual paths collapsed to the bare state.db
// path and the failure would have attributed to the sibling too.
func TestParseDiffHermesPerSessionParseErrorStaysScoped(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)

	// Never synced: a session row whose non-numeric started_at is listed
	// for fan-out but fails the per-session parse afterward.
	execHermesState(t, stateDB, `INSERT INTO sessions
		(id, source, model, started_at, ended_at, message_count)
		VALUES ('broken', 'cli', 'model', 'not-a-number', 1778767900, 1)`)

	diffEngine := NewDiffEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local",
	})
	t.Cleanup(diffEngine.Close)
	report, err := diffEngine.ParseDiff(t.Context(), ParseDiffOptions{
		Agents: []parser.AgentType{parser.AgentHermes},
	})
	require.NoError(t, err)

	assert.Equal(t, ParseDiffTotals{
		Examined: 1, Identical: 1, ParseErrors: 1,
	}, report.Totals,
		"the good session compares clean; the malformed session is a parse error")

	var errEntry *SessionDiff
	for i := range report.Sessions {
		if report.Sessions[i].Class == DiffParseError {
			errEntry = &report.Sessions[i]
		}
	}
	require.NotNil(t, errEntry, "parse error entry listed")
	assert.Contains(t, errEntry.FilePath, "state.db#broken",
		"error attributed to the per-session virtual path")

	var childEntry *SessionDiff
	for i := range report.Sessions {
		if report.Sessions[i].SessionID == "hermes:child" {
			childEntry = &report.Sessions[i]
		}
	}
	if childEntry != nil {
		assert.Equal(t, DiffIdentical, childEntry.Class,
			"the sibling session must not be reported as failed")
	}
}

func writeHermesSessionsFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, "sessions", name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// execHermesState opens stateDB and execs one statement. It mirrors
// cursor_ide_integration_test.go's execCursorIDEStateDB, but this file
// is package sync (internal tests) while that one is package sync_test
// (external/integration tests), so the two can't share one unexported
// helper across that package boundary.
func execHermesState(t *testing.T, stateDB, statement string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), statement)
	require.NoError(t, err)
}
