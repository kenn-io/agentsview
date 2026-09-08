package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestScheduledReconcileTargetsSelectsOnlyOptedInProviders(t *testing.T) {
	home := t.TempDir()
	aiderDir := filepath.Join(home, "aider")
	coworkDir := filepath.Join(home, "cowork")
	claudeDir := filepath.Join(home, "claude")
	omnigentDir := filepath.Join(home, "omnigent")
	require.NoError(t, os.MkdirAll(aiderDir, 0o755))
	require.NoError(t, os.MkdirAll(coworkDir, 0o755))
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.MkdirAll(omnigentDir, 0o755))

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentAider:    {aiderDir},
			parser.AgentCowork:   {coworkDir},
			parser.AgentClaude:   {claudeDir},
			parser.AgentOmnigent: {omnigentDir},
		},
	}
	targets := scheduledReconcileTargets(cfg)
	require.Len(t, targets, 2, "only opted-in providers are scheduled")
	assert.Equal(t, parser.AgentAider, targets[0].Agent)
	assert.Equal(t, []string{aiderDir}, targets[0].Roots)
	assert.Equal(t, parser.AgentOmnigent, targets[1].Agent)
	assert.Equal(t, []string{omnigentDir}, targets[1].Roots)
}

func TestScheduledReconcileDefersUnavailableOptedInRoots(t *testing.T) {
	tests := []struct {
		name       string
		agent      parser.AgentType
		sourcePath func(string) string
	}{
		{
			name:  "aider",
			agent: parser.AgentAider,
			sourcePath: func(root string) string {
				return filepath.Join(root, "project", ".aider.chat.history.md#0")
			},
		},
		{
			name:  "openhands",
			agent: parser.AgentOpenHands,
			sourcePath: func(root string) string {
				return filepath.Join(root, "conversation-1")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			missingRoot := filepath.Join(t.TempDir(), "unavailable")
			sourcePath := tc.sourcePath(missingRoot)
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
				tc.agent: {missingRoot},
			}}
			database := dbtest.OpenTestDB(t)
			sessionID := string(tc.agent) + ":archived"
			dbtest.SeedSession(t, database, sessionID, "project",
				func(session *db.Session) {
					session.Agent = string(tc.agent)
					session.FilePath = &sourcePath
				})
			require.NoError(t,
				database.SetSessionDataVersion(sessionID, db.CurrentDataVersion()))
			require.NoError(t, database.BaselineActiveSessionSourcePaths(
				t.Context(), "local", []db.SessionSourcePath{{
					Agent: string(tc.agent), FilePath: sourcePath,
				}},
			))
			engine := agentsync.NewEngine(database, agentsync.EngineConfig{
				AgentDirs: cfg.AgentDirs,
				Machine:   "local",
			})
			t.Cleanup(engine.Close)

			targets := scheduledReconcileTargets(cfg)
			runScheduledSyncPass(t.Context(), engine, targets)

			assert.Empty(t, targets,
				"an unavailable physical root must defer its authoritative scope")
			preserved, err := database.GetSession(
				t.Context(), sessionID,
			)
			require.NoError(t, err)
			assert.NotNil(t, preserved,
				"scheduled reconciliation must preserve the archived session")
		})
	}
}

func TestScheduledReconcileDefersNestedUnavailableRoots(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "aider")
	require.NoError(t, os.MkdirAll(parent, 0o755))
	child := filepath.Join(parent, "unavailable")
	sourcePath := filepath.Join(child, "project", ".aider.chat.history.md#0")

	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentAider: {parent, child},
	}}
	database := dbtest.OpenTestDB(t)
	sessionID := "aider:nested-archived"
	dbtest.SeedSession(t, database, sessionID, "project",
		func(session *db.Session) {
			session.Agent = string(parser.AgentAider)
			session.FilePath = &sourcePath
		})
	require.NoError(t,
		database.SetSessionDataVersion(sessionID, db.CurrentDataVersion()))
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "local", []db.SessionSourcePath{{
			Agent: string(parser.AgentAider), FilePath: sourcePath,
		}},
	))
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: cfg.AgentDirs,
		Machine:   "local",
	})
	t.Cleanup(engine.Close)

	targets := scheduledReconcileTargets(cfg)
	runScheduledSyncPass(t.Context(), engine, targets)

	assert.Empty(t, targets,
		"a present root must defer with a missing nested same-agent scope: "+
			"the engine expands it back to the missing dir")
	preserved, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.NotNil(t, preserved,
		"scheduled reconciliation must preserve sessions under the missing nested root")
}

type fakeScheduledEngine struct {
	calls []scheduledReconcileTarget
	err   error
}

func (f *fakeScheduledEngine) ReconcileProviderRoots(
	_ context.Context, agent parser.AgentType, roots []string,
) error {
	f.calls = append(f.calls, scheduledReconcileTarget{Agent: agent, Roots: roots})
	return f.err
}

func TestRemoteSourceSyncRootsSelectsSchemeRoots(t *testing.T) {
	local := t.TempDir()
	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {local, "s3://bucket/machine/raw/claude"},
			parser.AgentCodex: {
				"s3://bucket/machine/raw/codex",
				"S3://bucket/upper/raw/codex",
			},
		},
	}
	assert.Equal(t, []string{
		"s3://bucket/machine/raw/claude",
		"s3://bucket/machine/raw/codex",
	}, remoteSourceSyncRoots(cfg),
		"remote roots are selected by exact lowercase scheme, deduplicated, "+
			"and sorted; provider discovery recognizes only lowercase s3://, "+
			"so an uppercase root is a filesystem path here as it is at startup")
}

func TestRemoteSourceSyncRootsEmptyForLocalOnlyConfig(t *testing.T) {
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentClaude: {t.TempDir()},
	}}
	assert.Empty(t, remoteSourceSyncRoots(cfg))
}

type fakeRemoteSourceSyncEngine struct {
	calls [][]string
	since []time.Time
	stats agentsync.SyncStats
}

func (f *fakeRemoteSourceSyncEngine) SyncRootsSince(
	_ context.Context, roots []string, since time.Time, _ agentsync.ProgressFunc,
) agentsync.SyncStats {
	f.calls = append(f.calls, append([]string(nil), roots...))
	f.since = append(f.since, since)
	return f.stats
}

func TestRunRemoteSourceSyncPassSyncsConfiguredRemoteRoots(t *testing.T) {
	engine := &fakeRemoteSourceSyncEngine{}
	runRemoteSourceSyncPass(context.Background(), engine, nil)
	assert.Empty(t, engine.calls, "no remote roots -> no scoped sync")

	roots := []string{"s3://bucket/machine/raw/claude"}
	runRemoteSourceSyncPass(context.Background(), engine, roots)
	require.Len(t, engine.calls, 1)
	assert.Equal(t, roots, engine.calls[0])
	assert.True(t, engine.since[0].IsZero(),
		"the pass must cover the full remote scope; unchanged objects skip on fingerprints")
}

func TestRunScheduledSyncPassCallsPerAgent(t *testing.T) {
	engine := &fakeScheduledEngine{}
	runScheduledSyncPass(context.Background(), engine, nil)
	assert.Empty(t, engine.calls, "no targets -> no reconciliation")

	runScheduledSyncPass(context.Background(), engine,
		[]scheduledReconcileTarget{{Agent: parser.AgentAider, Roots: []string{"/a"}}})
	require.Len(t, engine.calls, 1)
	assert.Equal(t, parser.AgentAider, engine.calls[0].Agent)
	assert.Equal(t, []string{"/a"}, engine.calls[0].Roots)
}

func TestRunScheduledSyncPassLogsLifecycle(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantOutcome string
	}{
		{name: "completed", wantOutcome: "completed"},
		{name: "failed", err: errors.New("provider unavailable"), wantOutcome: "failed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogOutput(t)
			engine := &fakeScheduledEngine{err: tc.err}
			runScheduledSyncPass(context.Background(), engine,
				[]scheduledReconcileTarget{{
					Agent: parser.AgentAider, Roots: []string{"/a"},
				}},
			)

			output := logs.String()
			assert.Contains(t, output,
				"scheduled reconciliation started: targets=1")
			assert.Contains(t, output,
				"scheduled reconciliation finished: targets=1")
			assert.Contains(t, output, "duration=")
			assert.Contains(t, output, "outcome="+tc.wantOutcome)
		})
	}
}

// A single store edit must eventually reach the archive without another file
// event. Fake time advances through the real scheduled-pass interval; neither
// the provider cache nor its verification timestamp is modified by the test.
func TestScheduledCopilotReconcilesDeferredUsage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "session-state", "scheduled", "events.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(`{"type":"session.start","timestamp":"2026-09-04T17:00:00Z","data":{"sessionId":"scheduled"}}
{"type":"user.message","timestamp":"2026-09-04T17:00:01Z","data":{"content":"Question"}}
{"type":"assistant.message","timestamp":"2026-09-04T17:00:02Z","data":{"content":"Answer","outputTokens":3}}
`), 0o644))
		storePath := filepath.Join(root, "session-store.db")
		store, err := sql.Open("sqlite3", storePath)
		require.NoError(t, err)
		defer store.Close()
		_, err = store.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE sessions(id TEXT PRIMARY KEY);
INSERT INTO sessions VALUES('scheduled');
CREATE TABLE assistant_usage_events(id INTEGER PRIMARY KEY AUTOINCREMENT,
session_id TEXT,model TEXT,input_tokens INTEGER,output_tokens INTEGER,
cache_read_tokens INTEGER,cache_write_tokens INTEGER,reasoning_tokens INTEGER,created_at TEXT);
CREATE INDEX idx_assistant_usage_events_session ON assistant_usage_events(session_id,id);
INSERT INTO assistant_usage_events VALUES(1,'scheduled','gpt-5.4',100,3,0,0,0,'2026-09-04T17:00:02Z');
INSERT INTO assistant_usage_events VALUES(2,'scheduled','gpt-5.4',100,7,0,0,0,'2026-09-04T17:00:03Z');`)
		require.NoError(t, err)
		archive := dbtest.OpenTestDB(t)
		cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}}
		engine := agentsync.NewEngine(archive, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "local"})
		defer engine.Close()
		require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
		_, err = store.Exec(`UPDATE assistant_usage_events SET output_tokens=9 WHERE id=1`)
		require.NoError(t, err)
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{storePath + "-wal"}))
		usage, err := archive.GetUsageEvents(t.Context(), "copilot:scheduled")
		require.NoError(t, err)
		require.Len(t, usage, 2)
		assert.Equal(t, 3, usage[0].OutputTokens, "the event fast path defers this older-row edit")
		time.Sleep(periodicSyncInterval)
		runScheduledSyncPass(t.Context(), engine, scheduledReconcileTargets(cfg))
		usage, err = archive.GetUsageEvents(t.Context(), "copilot:scheduled")
		require.NoError(t, err)
		require.Len(t, usage, 2)
		assert.Equal(t, 9, usage[0].OutputTokens, "scheduled reconciliation must publish the deferred edit")
		assert.Equal(t, 7, usage[1].OutputTokens)
	})
}
