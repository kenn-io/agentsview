package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	engine := NewEngine(dbtest.OpenTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentHermes: {filepath.Join(root, "sessions")},
		},
		Machine: "local",
	})
	engine.InjectSkipCache(map[string]int64{
		stateDB: wantMtime,
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
	written, _, failed := engine.writeBatch(pending, syncWriteDefault, true)
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

func writeHermesArchiveStateDB(t *testing.T, root string) string {
	t.Helper()
	stateDB := filepath.Join(root, "state.db")
	conn, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

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
	return stateDB
}
