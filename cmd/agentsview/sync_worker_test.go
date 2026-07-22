package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// testConfigWithClaudeFixture builds a config pointing at a temp data dir and a
// Claude projects dir seeded with three parseable sessions.
func testConfigWithClaudeFixture(t *testing.T) config.Config {
	t.Helper()
	dataDir := t.TempDir()
	claudeDir := t.TempDir()
	for i := range 3 {
		projDir := filepath.Join(claudeDir, fmt.Sprintf("-home-proj%d", i))
		require.NoError(t, os.MkdirAll(projDir, 0o755))
		content := testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-01-01T00:00:00Z", "hello").
			AddClaudeAssistant("2026-01-01T00:00:01Z", "hi").
			String()
		require.NoError(t, os.WriteFile(
			filepath.Join(projDir, fmt.Sprintf("session%d.jsonl", i)),
			[]byte(content), 0o644,
		))
	}
	return config.Config{
		DataDir:          dataDir,
		DBPath:           filepath.Join(dataDir, "sessions.db"),
		LocalMachineName: "local",
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
	}
}

// decodeSingleResult scans NDJSON worker output and returns the sole terminal
// result, failing the test if the count is not exactly one.
func decodeSingleResult(t *testing.T, out *bytes.Buffer) workerResult {
	t.Helper()
	var results []workerResult
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		var line workerLine
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line),
			"every stdout line must be a workerLine JSON object")
		if line.Result != nil {
			results = append(results, *line.Result)
		}
	}
	require.NoError(t, sc.Err())
	require.Len(t, results, 1, "exactly one terminal result")
	return results[0]
}

func TestSyncWorkerStartupModeSyncsAndEmitsTerminalResult(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, "startup", &out))

	var results []workerResult
	sawProgress := false
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var line workerLine
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line),
			"every stdout line must be a workerLine JSON object")
		if line.Progress != nil {
			sawProgress = true
		}
		if line.Result != nil {
			results = append(results, *line.Result)
		}
	}
	require.NoError(t, sc.Err())
	assert.True(t, sawProgress)
	require.Len(t, results, 1, "exactly one terminal result")
	assert.Equal(t, "ok", results[0].Status)
	assert.True(t, results[0].DiscoveryComplete)
	assert.Equal(t, 3, results[0].Synced)
	require.NotNil(t, results[0].Stats,
		"the terminal result must carry the full SyncStats payload")
	assert.Equal(t, 3, results[0].Stats.TotalSessions,
		"public SyncStats fields must survive the NDJSON protocol")
}

func TestSyncWorkerReportsAbortAsFailure(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // aborted before work starts
	var out bytes.Buffer
	err := runSyncWorkerContext(ctx, cfg, "startup", &out)
	require.Error(t, err, "aborted work must not exit zero")
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "aborted", result.Status)
	assert.False(t, result.DiscoveryComplete)
}

func TestSyncWorkerFailsWhenWriteLockHeld(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	holdWriteOwnerLockForTest(t, cfg.DataDir) // hold db.write.lock like a daemon
	var out bytes.Buffer
	err := runSyncWorker(cfg, "startup", &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "write lock")
}

func TestSyncWorkerRejectsUnknownMode(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	var out bytes.Buffer
	err := runSyncWorker(cfg, "bogus", &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown sync-worker mode")
}

func TestSyncWorkerResyncBuildModeBuildsReplacement(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	// Seed the archive the worker rebuilds from, then close it so the worker
	// opens it read-only exactly as it does under the daemon's write barrier.
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	require.Equal(t, 3, engine.SyncAll(context.Background(), nil).Synced)
	engine.Close()
	require.NoError(t, database.Close())

	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, "resync-build", &out))
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "ok", result.Status)
	assert.True(t, result.DiscoveryComplete)
	assert.Equal(t, 3, result.Synced)
	assert.FileExists(t, cfg.DBPath+"-resync",
		"worker must leave the built replacement for the daemon to swap")
}

// TestSyncWorkerResyncBuildAppliesClassifierConfig pins the classifier wiring
// for the resync-build worker: it runs in a fresh process, so unless it
// installs the configured automation patterns before building, the rebuilt
// archive's forced is_automated backfill classifies every session with only
// the built-in patterns.
func TestSyncWorkerResyncBuildAppliesClassifierConfig(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	cfg.Automated.Prefixes = []string{"hello"}
	t.Cleanup(func() {
		db.SetUserAutomationPrefixes(nil)
		db.SetUserAutomationSubstrings(nil)
		db.SetUserAutomationExactMatches(nil)
	})

	// Seed the archive with default patterns, so no session is automated.
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	require.Equal(t, 3, engine.SyncAll(context.Background(), nil).Synced)
	engine.Close()
	require.NoError(t, database.Close())

	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, "resync-build", &out))
	require.Equal(t, "ok", decodeSingleResult(t, &out).Status)

	conn, err := sql.Open("sqlite3", cfg.DBPath+"-resync")
	require.NoError(t, err)
	defer conn.Close()
	var automated int
	require.NoError(t, conn.QueryRow(
		"SELECT COUNT(*) FROM sessions WHERE is_automated = 1",
	).Scan(&automated))
	assert.Equal(t, 3, automated,
		"the rebuilt archive must classify sessions with the configured user prefixes")
}

// markArchiveStale drops the archive's user_version so the next open reports
// NeedsResync, mimicking a parser data-version bump under a running daemon.
func markArchiveStale(t *testing.T, dbPath string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = conn.Exec("PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

// TestSyncWorkerRefusesResyncForLiveArchiveModes locks in the split-brain guard:
// the live-archive worker passes (sync/audit) run inside the daemon's writer
// handoff, so they must refuse a stale-version archive instead of swapping the
// file out from under the daemon's still-open reader pool.
func TestSyncWorkerRefusesResyncForLiveArchiveModes(t *testing.T) {
	for _, mode := range []string{"sync", "audit"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfigWithClaudeFixture(t)
			database, err := db.Open(cfg.DBPath)
			require.NoError(t, err)
			engine := sync.NewEngine(database, workerEngineConfig(cfg))
			require.Equal(t, 3, engine.SyncAll(context.Background(), nil).Synced)
			engine.Close()
			require.NoError(t, database.Close())
			markArchiveStale(t, cfg.DBPath)

			before, err := os.Stat(cfg.DBPath)
			require.NoError(t, err)

			var out bytes.Buffer
			err = runSyncWorkerContext(context.Background(), cfg, mode, &out)
			require.Error(t, err, "a live-archive worker must refuse a stale archive")
			assert.ErrorContains(t, err, "resync")

			result := decodeSingleResult(t, &out)
			assert.Equal(t, "failed", result.Status)
			assert.False(t, result.DiscoveryComplete)
			assert.Contains(t, result.Error, "resync")

			after, err := os.Stat(cfg.DBPath)
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after),
				"the archive file must not be swapped out from under the daemon")
			assert.NoFileExists(t, cfg.DBPath+"-resync",
				"a refused pass must not stage a replacement archive")
		})
	}
}

// failOnResultWriter fails the Write that carries the terminal result line so a
// test can exercise a dropped terminal-result emit on an otherwise-ok pass.
type failOnResultWriter struct{ attempted bool }

func (w *failOnResultWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"result"`)) {
		w.attempted = true
		return 0, errors.New("terminal write failed")
	}
	return len(p), nil
}

// TestSyncWorkerNonZeroWhenTerminalResultWriteFails pins the exit contract: a
// pass that succeeds but whose terminal result cannot be written must still
// return a non-nil error so the child exits non-zero.
func TestSyncWorkerNonZeroWhenTerminalResultWriteFails(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	w := &failOnResultWriter{}
	err := runSyncWorkerContext(context.Background(), cfg, "startup", w)
	require.Error(t, err, "a dropped terminal result must fail the worker")
	assert.True(t, w.attempted, "the terminal result write was attempted")
	assert.ErrorContains(t, err, "terminal result")
}

// TestSyncWorkerStartupAbortedResyncFallsBackIncremental mirrors the
// in-process startup path: when the required resync safety-aborts (here: all
// sources vanished while the old archive has data), the worker must follow up
// with an incremental sync instead of reporting a bare abort, and the original
// archive must be preserved.
func TestSyncWorkerStartupAbortedResyncFallsBackIncremental(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	require.Equal(t, 3, engine.SyncAll(context.Background(), nil).Synced)
	engine.Close()
	require.NoError(t, database.Close())
	markArchiveStale(t, cfg.DBPath)

	// Empty discovery against an archive with data safety-aborts the resync.
	claudeDir := cfg.AgentDirs[parser.AgentClaude][0]
	entries, err := os.ReadDir(claudeDir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NoError(t, os.RemoveAll(filepath.Join(claudeDir, entry.Name())))
	}

	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, "startup", &out),
		"the incremental fallback must complete the pass")
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "ok", result.Status,
		"a safety-aborted resync must fall back to the incremental sync")
	assert.True(t, result.DiscoveryComplete)

	database, err = db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer database.Close()
	var total int
	require.NoError(t, database.Reader().QueryRow(
		"SELECT COUNT(*) FROM sessions",
	).Scan(&total))
	assert.Equal(t, 3, total, "the aborted resync must leave the archive intact")
}

// TestResyncBuildResultFromStatsToleratesMinorityParseFailures pins the
// resync-build completion semantics: shouldAbortResyncSwap already folds the
// failure-majority judgment into stats.Aborted, so a completed build with a
// minority of permanent parse failures is a valid replacement the daemon must
// not discard.
func TestResyncBuildResultFromStatsToleratesMinorityParseFailures(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name       string
		ctx        context.Context
		stats      sync.SyncStats
		buildErr   error
		wantStatus string
	}{
		{
			name:       "minority parse failures still ok",
			ctx:        context.Background(),
			stats:      sync.SyncStats{Synced: 10, Failed: 2},
			wantStatus: "ok",
		},
		{
			name:       "safety abort",
			ctx:        context.Background(),
			stats:      sync.SyncStats{Aborted: true},
			wantStatus: "aborted",
		},
		{
			name:       "build error",
			ctx:        context.Background(),
			stats:      sync.SyncStats{Synced: 10},
			buildErr:   errors.New("build boom"),
			wantStatus: "failed",
		},
		{
			name:       "operational failure that also set aborted",
			ctx:        context.Background(),
			stats:      sync.SyncStats{Aborted: true},
			buildErr:   errors.New("create resync temp db: boom"),
			wantStatus: "failed",
		},
		{
			name:       "cancelled context",
			ctx:        cancelled,
			stats:      sync.SyncStats{Synced: 10},
			wantStatus: "aborted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resyncBuildResultFromStats(tt.ctx, tt.stats, tt.buildErr)
			assert.Equal(t, tt.wantStatus, result.Status)
			if tt.wantStatus == "ok" {
				assert.True(t, result.DiscoveryComplete)
				require.NotNil(t, result.Stats)
				assert.Equal(t, tt.stats.Synced, result.Stats.Synced)
			} else {
				assert.NotEqual(t, "ok", result.Status)
			}
		})
	}
}

func TestSyncWorkerSyncModeSyncsLikeStartup(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, "sync", &out))
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "ok", result.Status)
	assert.True(t, result.DiscoveryComplete)
	assert.Equal(t, 3, result.Synced)
}
