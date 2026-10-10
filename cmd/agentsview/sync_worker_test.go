package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
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
		DataDir:        dataDir,
		DBPath:         filepath.Join(dataDir, "sessions.db"),
		InstallationID: "0123456789abcdef0123456789abcdef",
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
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "startup"}, &out))

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

func TestSyncWorkerAuditModeForwardsReconciliationProgress(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "audit"}, &out))

	sawActiveProgress := false
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var line workerLine
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line),
			"every stdout line must be a workerLine JSON object")
		if line.Progress != nil &&
			line.Progress.Phase == sync.PhaseSyncing &&
			line.Progress.SessionsDone > 0 {
			sawActiveProgress = true
		}
	}
	require.NoError(t, sc.Err())
	assert.True(t, sawActiveProgress,
		"audit worker must forward active reconciliation progress")
}

func TestSyncWorkerStartupUsesConfiguredSourceMachine(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	claudeRoot := cfg.AgentDirs[parser.AgentClaude][0]
	cfg.SourceMachines = map[parser.AgentType]map[string]string{
		parser.AgentClaude: {claudeRoot: "archivebox"},
	}

	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "startup"}, &out))
	assert.Equal(t, "ok", decodeSingleResult(t, &out).Status)

	database, err := db.OpenReadOnly(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	defer database.Close()
	page, err := database.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 3)
	for _, sess := range page.Sessions {
		assert.Equal(t, "archivebox", sess.Machine)
	}
}

func TestSyncWorkerReportsAbortAsFailure(t *testing.T) {
	for _, mode := range []string{"startup", "resync-build"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfigWithClaudeFixture(t)
			database, err := openDB(t.Context(), cfg)
			require.NoError(t, err)
			require.NoError(t, database.Close())
			ctx, cancel := context.WithCancel(t.Context())
			cancel() // aborted before work starts
			var out bytes.Buffer
			err = runSyncWorkerContext(ctx, cfg, syncWorkerRequest{Mode: mode}, &out)
			require.Error(t, err, "aborted work must not exit zero")
			result := decodeSingleResult(t, &out)
			assert.Equal(t, "aborted", result.Status)
			assert.False(t, result.DiscoveryComplete)
		})
	}
}

func TestSyncWorkerResyncBuildReportsMissingArchive(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		DataDir: dir, DBPath: filepath.Join(dir, "missing.db"),
		InstallationID: "0123456789abcdef0123456789abcdef",
	}
	var out bytes.Buffer
	require.Error(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "resync-build"}, &out))
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "failed", result.Status)
	assert.False(t, result.DiscoveryComplete)
}

func TestWorkerResultPreservesTombstonesAcrossProtocol(t *testing.T) {
	result := workerResultFromStats(t.Context(), sync.SyncStats{
		Tombstoned: 2,
		Aborted:    true,
	})
	var wire bytes.Buffer
	require.NoError(t, json.MarshalWrite(&wire, workerLine{Result: &result}))

	decoded, err := readWorkerResult(&wire, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, decoded.Tombstoned,
		"the terminal summary must carry committed tombstones")
	assert.Equal(t, 2, statsFromWorkerResult(decoded).Tombstoned,
		"the daemon-side stats must retain tombstones after JSON decoding")
}

func TestSyncWorkerFailsWhenWriteLockHeld(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	holdWriteOwnerLockForTest(t, cfg.DataDir) // hold db.write.lock like a daemon
	var out bytes.Buffer
	err := runSyncWorker(cfg, syncWorkerRequest{Mode: "startup"}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "write lock")
}

func TestSyncWorkerRejectsUnknownMode(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	var out bytes.Buffer
	err := runSyncWorker(cfg, syncWorkerRequest{Mode: "bogus"}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown sync-worker mode")
}

func TestSyncWorkerResyncBuildModeBuildsReplacement(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	// Seed the archive the worker rebuilds from, then close it so the worker
	// opens it read-only exactly as it does under the daemon's write barrier.
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	engine := sync.NewEngine(t.Context(), database, workerEngineConfig(cfg))
	require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	require.NoError(t, database.Close())

	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "resync-build"}, &out))
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "ok", result.Status)
	assert.True(t, result.DiscoveryComplete)
	assert.Equal(t, 3, result.Synced)
	assert.FileExists(t, cfg.DBPath+"-resync",
		"worker must leave the built replacement for the daemon to swap")
}

func TestSyncWorkerResyncBuildUsesConfiguredImagePolicy(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	claudeRoot := cfg.AgentDirs[parser.AgentClaude][0]
	imageSession := filepath.Join(claudeRoot, "-home-proj0", "session0.jsonl")
	imageContent := `[ {"type":"text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"after"} ]`
	require.NoError(t, os.WriteFile(imageSession, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-01-01T00:00:00Z", "hello").
			AddRaw(`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","message":{"content":[{"type":"tool_use","id":"call-image","name":"Read","input":{}}]}}`).
			AddRaw(fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T00:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-image","content":%s}]}}`, imageContent)).
			String(),
	), 0o644))

	seedCfg := cfg
	seedCfg.ToolResultImages = config.ToolResultImagesKeep
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	engine := sync.NewEngine(t.Context(), database, workerEngineConfig(seedCfg))
	require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	require.NoError(t, database.Close())

	cfg.ToolResultImages = config.ToolResultImagesDrop
	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "resync-build"}, &out))
	require.Equal(t, "ok", decodeSingleResult(t, &out).Status)

	replacement, err := db.Open(t.Context(), cfg.DBPath+"-resync")
	require.NoError(t, err)
	defer func() { require.NoError(t, replacement.Close()) }()
	page, err := replacement.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(t, err)
	var foundImageCall bool
	for _, session := range page.Sessions {
		messages, err := replacement.GetAllMessages(t.Context(), session.ID)
		require.NoError(t, err)
		for _, message := range messages {
			for _, call := range message.ToolCalls {
				if call.ToolUseID != "call-image" {
					continue
				}
				foundImageCall = true
				assert.NotContains(t, call.ResultContent, "input_image")
				assert.NotContains(t, call.ResultContent, "data:image")
				for _, event := range call.ResultEvents {
					assert.NotContains(t, event.Content, "input_image")
				}
			}
		}
	}
	assert.True(t, foundImageCall, "fixture must reach the normalized tool-result tables")
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
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	engine := sync.NewEngine(t.Context(), database, workerEngineConfig(cfg))
	require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
	engine.Close()
	require.NoError(t, database.Close())

	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "resync-build"}, &out))
	require.Equal(t, "ok", decodeSingleResult(t, &out).Status)

	conn, err := sql.Open("sqlite3", cfg.DBPath+"-resync")
	require.NoError(t, err)
	defer conn.Close()
	var automated int
	require.NoError(t, conn.QueryRowContext(t.Context(),
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
	_, err = conn.ExecContext(t.Context(), "PRAGMA user_version = 0")
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
			database, err := db.Open(t.Context(), cfg.DBPath)
			require.NoError(t, err)
			engine := sync.NewEngine(t.Context(), database, workerEngineConfig(cfg))
			require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
			engine.Close()
			require.NoError(t, database.Close())
			markArchiveStale(t, cfg.DBPath)

			before, err := os.Stat(cfg.DBPath)
			require.NoError(t, err)

			var out bytes.Buffer
			err = runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: mode}, &out)
			require.Error(t, err, "a live-archive worker must refuse a stale archive")
			require.ErrorContains(t, err, "resync")

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
	err := runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, w)
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
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	engine := sync.NewEngine(t.Context(), database, workerEngineConfig(cfg))
	require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
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
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "startup"}, &out),
		"the incremental fallback must complete the pass")
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "ok", result.Status,
		"a safety-aborted resync must fall back to the incremental sync")
	assert.True(t, result.DiscoveryComplete)

	database, err = db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	defer database.Close()
	var total int
	require.NoError(t, database.Reader().QueryRow(t.Context(),
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
	cancelled, cancel := context.WithCancel(t.Context())
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
			ctx:        t.Context(),
			stats:      sync.SyncStats{Synced: 10, Failed: 2},
			wantStatus: "ok",
		},
		{
			name:       "safety abort",
			ctx:        t.Context(),
			stats:      sync.SyncStats{Aborted: true},
			wantStatus: "aborted",
		},
		{
			name:       "deferred processing",
			ctx:        t.Context(),
			stats:      sync.SyncStats{Deferred: 1},
			wantStatus: "failed",
		},
		{
			name:       "build error",
			ctx:        t.Context(),
			stats:      sync.SyncStats{Synced: 10},
			buildErr:   errors.New("build boom"),
			wantStatus: "failed",
		},
		{
			name:       "operational failure that also set aborted",
			ctx:        t.Context(),
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
	require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "sync"}, &out))
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "ok", result.Status)
	assert.True(t, result.DiscoveryComplete)
	assert.Equal(t, 3, result.Synced)
}

// Run the real worker body and protocol through the daemon's writer handoff.
// Only process creation is replaced, keeping both sides on temporary archives.
func TestWorkerParentLinkHandoff(t *testing.T) {
	for _, tc := range []struct {
		name             string
		mode             string
		failLink         bool
		loseResult       bool
		retryWithWorker  bool
		failSource       bool
		wantIdlePasses   int
		discardBuild     bool
		cancelAfterBuild bool
	}{
		{name: "sync repair", mode: "sync"},
		{name: "sync retry", mode: "sync", failLink: true},
		{name: "audit retry", mode: "audit", failLink: true},
		{name: "lost sync result", mode: "sync", failLink: true, loseResult: true},
		{name: "sync completes pending links", mode: "sync", failLink: true, retryWithWorker: true},
		{name: "unchanged audit completes pending links", mode: "audit", failLink: true, retryWithWorker: true},
		{name: "failed sync completes pending links", mode: "sync", failLink: true, retryWithWorker: true, failSource: true},
		{name: "failed audit completes pending links", mode: "audit", failLink: true, retryWithWorker: true, failSource: true},
		{name: "installed rebuild completes pending links", mode: "resync-build", failLink: true, retryWithWorker: true},
		{name: "installed rebuild with canceled cache reload", mode: "resync-build", failLink: true, retryWithWorker: true, cancelAfterBuild: true},
		{name: "discarded rebuild retains pending links", mode: "resync-build", failLink: true, retryWithWorker: true, discardBuild: true, wantIdlePasses: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfigWithClaudeFixture(t)
			var initial bytes.Buffer
			require.NoError(t, runSyncWorker(cfg, syncWorkerRequest{Mode: "startup"}, &initial))
			database, lock := openTestWriteDB(t, cfg)
			for _, id := range []string{"worker-parent", "worker-child"} {
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{
					ID: id, Agent: "zencoder", Project: "project", Machine: "local",
					RelationshipType: "continuation",
				}))
			}
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
				SessionID: "worker-parent", Ordinal: 0, Role: "assistant",
				Content: "spawn child", HasToolUse: true,
				ToolCalls: []db.ToolCall{{
					ToolUseID: "spawn", ToolName: "Task", SubagentSessionID: "worker-child",
				}},
			}}))
			if tc.mode == "audit" {
				// An audit links after a source change; unlike full sync it skips
				// global linking when every source is unchanged.
				path := filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "-home-proj0", "new-session.jsonl")
				content := testjsonl.NewSessionBuilder().
					AddClaudeUser("2026-01-01T00:00:00Z", "changed transcript").
					AddClaudeAssistant("2026-01-01T00:00:01Z", "new reply").String()
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			}
			raw, err := sql.Open("sqlite3", cfg.DBPath)
			require.NoError(t, err)
			defer raw.Close()
			if tc.failLink {
				_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER fail_worker_link
					BEFORE UPDATE OF parent_session_id ON sessions WHEN NEW.id = 'worker-child'
					BEGIN SELECT RAISE(FAIL, 'injected worker link failure'); END`)
				require.NoError(t, err)
			}
			em := &scopedEmitter{scopes: make(chan string, 8)}
			engineConfig := workerEngineConfig(cfg)
			engineConfig.Emitter = em
			engine := sync.NewEngine(t.Context(), database, engineConfig)
			defer engine.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var terminal workerResult
			restore := stubLaunchSyncWorker(t, func(
				ctx context.Context, cfg config.Config, request syncWorkerRequest, _ func(workerLine),
			) (workerResult, error) {
				mode := request.Mode
				var wire bytes.Buffer
				workerErr := runSyncWorkerContext(ctx, cfg, request, &wire)
				terminal = decodeSingleResult(t, &wire)
				if mode == "resync-build" {
					if tc.discardBuild {
						require.NoError(t, os.Remove(engine.ResyncTempPath()))
					}
					if tc.cancelAfterBuild {
						cancel()
					}
				}
				if tc.loseResult {
					// The worker has finished its writes, but the daemon receives
					// no terminal line, as when cancellation kills a started worker.
					return readWorkerResult(&bytes.Buffer{}, nil)
				}
				return terminal, workerErr
			})
			defer restore()
			if tc.mode != "audit" {
				_, _, err = runWorkerSyncPass(t.Context(), t.Context(), cfg, engine, database, lock, false, nil)
				require.Zero(t, terminal.Synced)
			} else {
				err = runArchiveAudit(t.Context(), cfg, engine, database, lock, em)
				require.Equal(t, 1, terminal.Synced)
			}
			require.Zero(t, terminal.Tombstoned)
			require.NotNil(t, terminal.Stats)
			require.Zero(t, terminal.Stats.CwdUpdated)
			if tc.failLink {
				require.Error(t, err)
				if tc.loseResult {
					require.ErrorContains(t, err, "0 terminal results")
				}
				_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_worker_link`)
				require.NoError(t, err)
				require.NoError(t, raw.Close())
				if tc.retryWithWorker {
					if tc.failSource {
						root := t.TempDir()
						cfg.AgentDirs[parser.AgentGemini] = []string{root}
						path := filepath.Join(root, "tmp", "project", "chats", "session-broken.json")
						require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
						require.NoError(t, os.WriteFile(path, []byte(`{"messages": invalid}`), 0o600))
					}
					switch tc.mode {
					case "sync":
						_, _, err = runWorkerSyncPass(ctx, t.Context(), cfg, engine, database, lock, false, nil)
					case "audit":
						err = runArchiveAudit(ctx, cfg, engine, database, lock, em)
					case "resync-build":
						_, err, _ = runWorkerResyncBuild(ctx, t.Context(), cfg, engine, database, nil, true)
					}
					switch {
					case tc.failSource:
						require.Error(t, err)
						require.Positive(t, terminal.Failed)
						require.Equal(t, 1, terminal.Stats.LinksUpdated,
							"source failure must not hide the completed repair")
					case tc.discardBuild:
						require.ErrorContains(t, err, "swap resync database")
					case tc.cancelAfterBuild:
						require.ErrorIs(t, err, context.Canceled)
						require.ErrorContains(t, err, "reloading skip cache after swap")
					default:
						require.NoError(t, err)
					}
					if tc.mode != "resync-build" {
						require.Zero(t, terminal.Synced)
					}
					require.False(t, terminal.Stats.LinksPending)
					child, err := database.GetSession(t.Context(), "worker-child")
					require.NoError(t, err)
					require.NotNil(t, child)
					if tc.discardBuild {
						require.Nil(t, child.ParentSessionID,
							"the worker has not completed linking in the live archive")
					} else {
						require.Equal(t, new("worker-parent"), child.ParentSessionID)
					}
					require.NoError(t, engine.ReconcileProviderRoots(t.Context(),
						parser.AgentClaude, cfg.AgentDirs[parser.AgentClaude]))
					assert.Equal(t, tc.wantIdlePasses, engine.LastReconciliationResult().Metrics.GlobalLinkPasses,
						"only unfinished linking should require an idle global pass")
				} else {
					require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(), []sync.ProviderRootsGroup{
						{Agent: parser.AgentClaude, Roots: cfg.AgentDirs[parser.AgentClaude]},
					}))
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, 1, statsFromWorkerResult(terminal).LinksUpdated)
				require.Len(t, em.scopes, 1, "a link-only worker repair must refresh clients")
				assert.Equal(t, "sync", <-em.scopes)
			}
			child, err := database.GetSession(t.Context(), "worker-child")
			require.NoError(t, err)
			require.NotNil(t, child)
			assert.Equal(t, new("worker-parent"), child.ParentSessionID,
				"the unchanged poll must finish links left by a failed worker")
			assert.False(t, engine.PendingSubagentLinks(), "a successful retry must clear pending work")
		})
	}
}
