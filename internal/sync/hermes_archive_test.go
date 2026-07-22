package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
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
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	before, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	require.NoError(t, os.Remove(transcriptPath))
	after, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	stateInfo, err := os.Stat(stateDB)
	require.NoError(t, err)
	assert.NotEqual(t, before.Size, after.Size)
	assert.Equal(t, stateInfo.Size(), after.Size)
}

func TestHermesProfileCreatedAfterEngineInitializationIsDiscovered(t *testing.T) {
	profilesRoot := filepath.Join(t.TempDir(), ".hermes", "profiles")
	require.NoError(t, os.MkdirAll(profilesRoot, 0o755))
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {profilesRoot},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	before := engine.SyncAll(context.Background(), nil)
	assert.Zero(t, before.Synced)

	profileRoot := filepath.Join(profilesRoot, "research")
	require.NoError(t, os.MkdirAll(profileRoot, 0o755))
	writeHermesArchiveStateDB(t, profileRoot)

	after := engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, after.Synced)
	session, err := database.GetSession(context.Background(), "hermes:child")
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
	engine := NewEngine(database, EngineConfig{
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
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {filepath.Join(root, "sessions")},
		},
		Machine: "local",
	})
	provider, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{
		Roots: []string{filepath.Join(root, "sessions")},
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)
	initial := engine.processFile(context.Background(), parser.DiscoveredFile{
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
			stateDB, parser.AgentHermes, fingerprint,
		): wantMtime,
	})

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
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
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {filepath.Join(root, "sessions")},
		},
		Machine: "local",
	})

	res := engine.processFile(context.Background(), parser.DiscoveredFile{
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

	storedSize, storedMtime, ok := database.GetFileInfoByPath(stateDB)
	require.True(t, ok)
	assert.Equal(t, wantSize, storedSize)
	assert.Equal(t, wantMtime, storedMtime)
}

// TestSyncPathsHermesArchiveTranscriptPersistsAggregateFingerprint confirms that
// syncing a transcript path inside an archive routes through the provider, which
// reparses the whole archive and persists the aggregate file info under the
// state.db path. This replaces the removed syncSingleHermesArchive coverage.
func TestSyncPathsHermesArchiveTranscriptPersistsAggregateFingerprint(t *testing.T) {
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
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {filepath.Join(root, "sessions")},
		},
		Machine: "local",
	})

	engine.SyncPaths([]string{transcriptPath})

	storedSize, storedMtime, found := database.GetFileInfoByPath(stateDB)
	require.True(t, found)
	assert.Equal(t, wantSize, storedSize)
	assert.Equal(t, wantMtime, storedMtime)
}

func TestSyncPathsHermesArchiveWALCommitRefreshesMetadata(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	writer := openHermesArchiveWALWriter(t, stateDB)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {root},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	initial := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, initial.Synced)
	stateBefore, err := os.Stat(stateDB)
	require.NoError(t, err)

	_, err = writer.Exec(`UPDATE sessions SET title = 'WAL-only title' WHERE id = 'child'`)
	require.NoError(t, err)
	walPath := stateDB + "-wal"
	walInfo, err := os.Stat(walPath)
	require.NoError(t, err)
	stateAfter, err := os.Stat(stateDB)
	require.NoError(t, err)
	assert.Equal(t, stateBefore.Size(), stateAfter.Size())
	assert.Equal(t, stateBefore.ModTime(), stateAfter.ModTime(),
		"the committed update must remain WAL-only for this regression")
	walTime := stateAfter.ModTime().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(walPath, walTime, walTime))

	engine.SyncPaths([]string{walPath})

	session, err := database.GetSession(context.Background(), "hermes:child")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.DisplayName)
	assert.Equal(t, "WAL-only title", *session.DisplayName)
	storedSize, storedMtime, found := database.GetFileInfoByPath(stateDB)
	require.True(t, found)
	assert.Equal(t, stateAfter.Size()+walInfo.Size(), storedSize)
	assert.Equal(t, walTime.UnixNano(), storedMtime)
}

func openHermesArchiveWALWriter(t *testing.T, stateDB string) *sql.DB {
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
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		Machine: "local",
	})
	assertMessages := func(want ...string) {
		messages, err := database.GetMessages(
			context.Background(), "hermes:child", 0, len(want), true,
		)
		require.NoError(t, err)
		require.Len(t, messages, len(want))
		for i := range want {
			assert.Equal(t, want[i], messages[i].Content)
		}
	}
	require.NoError(t, engine.ReconcileWatchRoots(context.Background(), nil, true))
	assertMessages("original prompt", "Done.")

	provider, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{
		Roots: []string{sessionsDir},
	})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), parser.FindSourceRequest{
		RawSessionID: "child",
	})
	require.NoError(t, err)
	require.True(t, found)
	fingerprint, err := provider.Fingerprint(context.Background(), source)
	require.NoError(t, err)
	memberPath := parser.VirtualSourcePath(stateDB, "child")
	storedSize, storedMtime, found := database.GetFileInfoByPath(memberPath)
	require.True(t, found)
	assert.Equal(t, fingerprint.Size, storedSize)
	assert.Equal(t, fingerprint.MTimeNS, storedMtime)
	storedHash, found := database.GetFileHashByPath(memberPath)
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

	require.NoError(t, engine.ReconcileWatchRoots(context.Background(), nil, true))
	assertMessages("modified prompt", "Done.")
}

func TestReconcileHermesDefaultSessionsRootTombstonesRemovedStateMember(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
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
	assert.Nil(t, stored,
		"authoritative reconciliation must tombstone a removed state.db member")
}

// Streamed discovery must open state.db a bounded number of times per pass:
// once to stream members and once for the transcript membership check — not
// once per transcript file, which scales reconciliation work with archive
// size instead of the changed batch.
func TestReconcileHermesStateDBOpensBoundedPerPass(t *testing.T) {
	scansFor := func(t *testing.T, transcripts int) int {
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
		engine := NewEngine(database, EngineConfig{
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
		engine := NewEngine(database, EngineConfig{
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
		engine := NewEngine(database, EngineConfig{
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
			t.Context(), []string{sessionsDir}, false,
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
		assert.Nil(t, removed, "removed member must tombstone")
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

// Aggregate discovery (SyncAll) and streamed reconciliation must agree on
// per-member source identity: a state.db member synced through SyncAll and
// then removed without a watcher event must still be tombstoned by the
// audit's streamed reconciliation, while surviving members stay active.
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
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	require.Positive(t, engine.SyncAll(t.Context(), nil).Synced)
	stored, err := database.GetSession(t.Context(), "hermes:child")
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
	removed, err := database.GetSession(t.Context(), "hermes:child")
	require.NoError(t, err)
	assert.Nil(t, removed,
		"audit reconciliation must tombstone a SyncAll-seeded removed member")
	survivor, err := database.GetSession(t.Context(), "hermes:survivor")
	require.NoError(t, err)
	assert.NotNil(t, survivor,
		"surviving state members must stay active")
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
	engine := NewEngine(database, EngineConfig{
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
	assert.Nil(t, stored,
		"absolute reconciliation scope must cover a configured relative root")
}

func TestReconcileHermesDefaultSessionsRootPreservesMissingStateDBArchive(t *testing.T) {
	root := t.TempDir()
	stateDB := writeHermesArchiveStateDB(t, root)
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
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
	engine := NewEngine(database, EngineConfig{
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
	assert.Equal(t, jsonlPath, database.GetSessionFilePath("hermes:orphan"),
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
	engine := NewEngine(database, EngineConfig{
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

	_, err = conn.Exec(`
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
