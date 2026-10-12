package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
						database := dbtest.OpenTestDB(t)
						engine := NewEngine(t.Context(), database, EngineConfig{
							AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
						})
						t.Cleanup(engine.Close)
						seed := engine.SyncAll(t.Context(), nil)
						require.Zero(t, seed.Failed)
						siblingID := string(agent) + ":member-000"
						before, err := database.GetSessionFull(t.Context(), siblingID)
						require.NoError(t, err)
						require.NotNil(t, before)
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
						after, err := database.GetSessionFull(t.Context(), siblingID)
						require.NoError(t, err)
						assert.Equal(t, before, after, "unchanged siblings must receive no archive writes")
						child, err := database.GetSessionFull(t.Context(), string(agent)+":child")
						require.NoError(t, err)
						require.NotNil(t, child)
						assert.Equal(t, new("changed child prompt from transcript"), child.FirstMessage)
						assert.Positive(t, scans.Load())
						assert.LessOrEqual(t, scans.Load(), int64(4), "container opens must stay bounded as the archive grows")

						_, err = conn.ExecContext(t.Context(), "DELETE FROM messages WHERE session_id = 'member-000'; DELETE FROM sessions WHERE id = 'member-000'")
						require.NoError(t, err)
						require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{root}, false))
						removed, err := database.GetSessionFull(t.Context(), siblingID)
						require.NoError(t, err)
						require.NotNil(t, removed)
						assert.NotNil(t, removed.SourceMissingAt)
						assert.Nil(t, removed.DeletedAt, "source deletion must preserve the archived session")
						assert.Equal(t, before.FirstMessage, removed.FirstMessage)
						assert.Equal(t, before.TranscriptRevision, removed.TranscriptRevision)
						survivor, err := database.GetSessionFull(t.Context(), string(agent)+":member-001")
						require.NoError(t, err)
						require.NotNil(t, survivor)
						assert.Nil(t, survivor.SourceMissingAt)
						assert.Nil(t, survivor.DeletedAt)
					})
				}
			})
		}
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
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, before.TranscriptRevision, after.TranscriptRevision)
	assert.Nil(t, after.SourceMissingAt)
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
}

func TestHermesStandaloneTranscriptEditsReplaceMessages(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		for _, mode := range []string{"full sync", "transcript event"} {
			t.Run(string(agent)+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				writeHermesArchiveStateDB(t, root)
				dir := filepath.Join(root, "sessions")
				writeHermesTranscriptFile(t, dir, "standalone")
				path := filepath.Join(dir, "standalone.jsonl")
				database := dbtest.OpenTestDB(t)
				engine := NewEngine(t.Context(), database, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
				})
				t.Cleanup(engine.Close)
				require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
				require.NoError(t, os.WriteFile(path, []byte(
					`{"role":"user","content":"edited prompt","timestamp":"2026-05-14T10:01:00Z"}`+"\n"+
						`{"role":"assistant","content":"corrected reply","timestamp":"2026-05-14T10:02:00Z"}`+"\n",
				), 0o600))
				if mode == "full sync" {
					require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
				} else {
					require.NoError(t, engine.syncPathsContext(t.Context(), []string{path}, false))
				}
				for range 2 {
					messages, err := database.GetAllMessages(t.Context(), string(agent)+":standalone")
					require.NoError(t, err)
					contents := make([]string, len(messages))
					for i, message := range messages {
						contents[i] = message.Content
					}
					assert.Equal(t, []string{"edited prompt", "corrected reply"}, contents)
					assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
				}
			})
		}
	}
}

func TestHermesPartialDiscoverySyncsReadableTranscripts(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		for _, damage := range []string{"malformed database", "incompatible schema"} {
			for _, mode := range []string{"full sync", "transcript event"} {
				t.Run(string(agent)+"/"+damage+"/"+mode, func(t *testing.T) {
					root := t.TempDir()
					stateDB := writeHermesArchiveStateDB(t, root)
					healthyRoot := t.TempDir()
					database := dbtest.OpenTestDB(t)
					engine := NewEngine(t.Context(), database, EngineConfig{
						AgentDirs: map[parser.AgentType][]string{agent: {root, healthyRoot}}, Machine: "local",
					})
					t.Cleanup(engine.Close)
					require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
					before, err := database.GetSessionFull(t.Context(), string(agent)+":child")
					require.NoError(t, err)
					require.NotNil(t, before)
					require.NoError(t, os.Remove(stateDB))
					if damage == "malformed database" {
						require.NoError(t, os.WriteFile(stateDB, []byte("not a sqlite database"), 0o600))
					} else {
						conn, openErr := sql.Open("sqlite3", stateDB)
						require.NoError(t, openErr)
						_, execErr := conn.ExecContext(t.Context(), "CREATE TABLE unrelated (id TEXT PRIMARY KEY)")
						require.NoError(t, execErr)
						require.NoError(t, conn.Close())
					}
					dir := filepath.Join(root, "sessions")
					writeHermesTranscriptFile(t, dir, "fallback")
					writeHermesTranscriptFile(t, healthyRoot, "healthy")
					if mode == "full sync" {
						stats := engine.SyncAll(t.Context(), nil)
						assert.Positive(t, stats.Failed, "partial discovery must report the database error")
					} else {
						err = engine.syncPathsContext(t.Context(), []string{filepath.Join(dir, "fallback.jsonl"), filepath.Join(healthyRoot, "healthy.jsonl")}, false)
						require.Error(t, err, "partial classification must report the database error")
					}
					for _, id := range []string{"fallback", "healthy"} {
						stored, getErr := database.GetSession(t.Context(), string(agent)+":"+id)
						require.NoError(t, getErr)
						require.NotNil(t, stored)
						assert.Equal(t, new("hello"), stored.FirstMessage)
					}
					after, err := database.GetSessionFull(t.Context(), string(agent)+":child")
					require.NoError(t, err)
					assert.Equal(t, before, after, "incomplete discovery must preserve state-only history")
				})
			}
		}
	}
}

func TestHermesSyncAllSinceIncludesWALOnlyEdits(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
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
				AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
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
			messages, err := database.GetAllMessages(t.Context(), string(agent)+":child")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "WAL-only edit", messages[0].Content)
		})
	}
}

func TestHermesIncompleteTranscriptPreservesArchivedHistory(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		for _, mode := range []string{"full sync", "transcript event"} {
			for _, test := range []struct {
				name, content string
			}{
				{name: "empty"},
				{name: "metadata only", content: `{"role":"session_meta","platform":"cli"}` + "\n"},
				{name: "malformed", content: "invalid JSON\n"},
			} {
				t.Run(string(agent)+"/"+mode+"/"+test.name, func(t *testing.T) {
					root := t.TempDir()
					writeHermesArchiveStateDB(t, root)
					dir := filepath.Join(root, "sessions")
					writeHermesTranscriptFile(t, dir, "standalone")
					path := filepath.Join(dir, "standalone.jsonl")
					database := dbtest.OpenTestDB(t)
					engine := NewEngine(t.Context(), database, EngineConfig{
						AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
					})
					t.Cleanup(engine.Close)
					require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
					id := string(agent) + ":standalone"
					before, err := database.GetSessionFull(t.Context(), id)
					require.NoError(t, err)
					require.NotNil(t, before)
					messagesBefore, err := database.GetAllMessages(t.Context(), id)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(path, []byte(test.content), 0o600))
					if mode == "full sync" {
						require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
					} else {
						require.NoError(t, engine.syncPathsContext(t.Context(), []string{path}, false))
					}
					for range 2 {
						after, getErr := database.GetSessionFull(t.Context(), id)
						require.NoError(t, getErr)
						assert.Equal(t, before, after)
						messagesAfter, getErr := database.GetAllMessages(t.Context(), id)
						require.NoError(t, getErr)
						assert.Equal(t, messagesBefore, messagesAfter)
						require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
					}
				})
			}
		}
	}
}

func TestHermesUnreadableStateMembersUseTranscriptFallback(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		for _, damage := range []string{"malformed database", "session IDs only", "messages unavailable", "unreadable member"} {
			for _, mode := range []string{"full sync", "state event", "WAL event", "transcript event", "reconciliation"} {
				if damage == "malformed database" && mode == "WAL event" {
					continue
				}
				t.Run(string(agent)+"/"+damage+"/"+mode, func(t *testing.T) {
					root := t.TempDir()
					stateDB := writeHermesArchiveStateDB(t, root)
					conn, err := sql.Open("sqlite3", stateDB)
					require.NoError(t, err)
					_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions
						(id, source, model, started_at, message_count) VALUES ('keeper', 'cli', 'model', 1778767200, 1);
						INSERT INTO messages (session_id, role, content, timestamp)
						VALUES ('keeper', 'user', 'retained history', 1778767210)`)
					require.NoError(t, err)
					require.NoError(t, conn.Close())
					database := dbtest.OpenTestDB(t)
					engine := NewEngine(t.Context(), database, EngineConfig{
						AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
					})
					t.Cleanup(engine.Close)
					require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
					before, err := database.GetSessionFull(t.Context(), string(agent)+":keeper")
					require.NoError(t, err)
					require.NotNil(t, before)
					seedDB := writeHermesArchiveStateDB(t, t.TempDir())
					require.NoError(t, os.Remove(stateDB))
					if damage == "malformed database" {
						require.NoError(t, os.WriteFile(stateDB, []byte("not a sqlite database"), 0o600))
					} else {
						writer, openErr := sql.Open("sqlite3", stateDB)
						require.NoError(t, openErr)
						writer.SetMaxOpenConns(1)
						t.Cleanup(func() { require.NoError(t, writer.Close()) })
						if mode == "WAL event" {
							_, err = writer.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0")
							require.NoError(t, err)
						}
						switch damage {
						case "session IDs only":
							_, err = writer.ExecContext(t.Context(), "CREATE TABLE sessions (id TEXT PRIMARY KEY); INSERT INTO sessions VALUES ('child')")
						case "messages unavailable":
							_, err = writer.ExecContext(t.Context(), "ATTACH DATABASE ? AS seed; CREATE TABLE sessions AS SELECT * FROM seed.sessions", seedDB)
						default:
							_, err = writer.ExecContext(t.Context(), `ATTACH DATABASE ? AS seed;
								CREATE TABLE sessions AS SELECT * FROM seed.sessions;
								CREATE TABLE messages AS SELECT * FROM seed.messages;
								UPDATE sessions SET started_at = 'invalid' WHERE id = 'child';
								INSERT INTO sessions (id, source, started_at, message_count)
								VALUES ('usable', 'cli', 1778767200, 1);
								INSERT INTO messages (session_id, role, content, timestamp)
								VALUES ('usable', 'user', 'usable state message', 1778767210)`, seedDB)
						}
						require.NoError(t, err)
					}
					dir := filepath.Join(root, "sessions")
					writeHermesTranscriptFile(t, dir, "child")
					writeHermesTranscriptFile(t, dir, "standalone")
					path := filepath.Join(dir, "child.jsonl")
					switch mode {
					case "full sync":
						assert.Positive(t, engine.SyncAll(t.Context(), nil).Failed)
					case "reconciliation":
						err = engine.ReconcileWatchRootsAfterLostEvents(t.Context(), []string{root}, false)
						require.Error(t, err)
						assert.False(t, engine.LastReconciliationResult().Complete)
					default:
						event := stateDB
						switch mode {
						case "WAL event":
							event += "-wal"
						case "transcript event":
							event = path
						}
						err = engine.syncPathsContext(t.Context(), []string{event}, false)
						require.Error(t, err)
					}
					messages, err := database.GetAllMessages(t.Context(), string(agent)+":child")
					require.NoError(t, err)
					contents := make([]string, len(messages))
					for i, message := range messages {
						contents[i] = message.Content
					}
					assert.Equal(t, []string{"hello", "Done."}, contents)
					if mode != "transcript event" {
						standalone, getErr := database.GetSession(t.Context(), string(agent)+":standalone")
						require.NoError(t, getErr)
						require.NotNil(t, standalone)
						assert.Equal(t, new("hello"), standalone.FirstMessage)
						if damage == "unreadable member" {
							usable, getErr := database.GetSession(t.Context(), string(agent)+":usable")
							require.NoError(t, getErr)
							require.NotNil(t, usable)
							assert.Equal(t, new("usable state message"), usable.FirstMessage)
						}
					}
					after, err := database.GetSessionFull(t.Context(), string(agent)+":keeper")
					require.NoError(t, err)
					assert.Equal(t, before, after)
				})
			}
		}
	}
}

func TestHermesSingleSessionResyncUsesTranscriptAfterDatabaseFailure(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		for _, damage := range []string{"malformed database", "session IDs only", "messages unavailable"} {
			t.Run(string(agent)+"/"+damage, func(t *testing.T) {
				root := t.TempDir()
				stateDB := writeHermesArchiveStateDB(t, root)
				dir := filepath.Join(root, "sessions")
				writeHermesTranscriptFile(t, dir, "sibling")
				database := dbtest.OpenTestDB(t)
				engine := NewEngine(t.Context(), database, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
				})
				t.Cleanup(engine.Close)
				require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
				id := string(agent) + ":child"
				require.Equal(t, parser.VirtualSourcePath(stateDB, "child"), database.GetSessionFilePath(t.Context(), id))
				siblingID := string(agent) + ":sibling"
				before, err := database.GetSessionFull(t.Context(), siblingID)
				require.NoError(t, err)
				require.NotNil(t, before)
				seedDB := writeHermesArchiveStateDB(t, t.TempDir())
				require.NoError(t, os.Remove(stateDB))
				if damage == "malformed database" {
					require.NoError(t, os.WriteFile(stateDB, []byte("not a sqlite database"), 0o600))
				} else {
					conn, openErr := sql.Open("sqlite3", stateDB)
					require.NoError(t, openErr)
					if damage == "session IDs only" {
						_, err = conn.ExecContext(t.Context(), "CREATE TABLE sessions (id TEXT PRIMARY KEY); INSERT INTO sessions VALUES ('child')")
					} else {
						_, err = conn.ExecContext(t.Context(), "ATTACH DATABASE ? AS seed; CREATE TABLE sessions AS SELECT * FROM seed.sessions", seedDB)
					}
					require.NoError(t, err)
					require.NoError(t, conn.Close())
				}
				writeHermesTranscriptFile(t, dir, "child")
				writeHermesTranscriptFile(t, dir, "unrequested")
				require.NoError(t, os.WriteFile(filepath.Join(dir, "sibling.jsonl"), []byte(
					`{"role":"user","content":"unsynced sibling edit","timestamp":"2026-05-14T10:01:00Z"}`+"\n",
				), 0o600))
				require.NoError(t, engine.SyncSingleSessionContext(t.Context(), id))
				messages, err := database.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				contents := make([]string, len(messages))
				for i, message := range messages {
					contents[i] = message.Content
				}
				assert.Equal(t, []string{"hello", "Done."}, contents)
				assert.Equal(t, filepath.Join(dir, "child.jsonl"), database.GetSessionFilePath(t.Context(), id))
				after, err := database.GetSessionFull(t.Context(), siblingID)
				require.NoError(t, err)
				assert.Equal(t, before, after, "single-session resync must leave siblings untouched")
				unrequested, err := database.GetSession(t.Context(), string(agent)+":unrequested")
				require.NoError(t, err)
				assert.Nil(t, unrequested)
			})
		}
	}
}
