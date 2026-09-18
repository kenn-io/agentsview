package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	assert := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	aiderDir := filepath.Join(home, "aider")
	coworkDir := filepath.Join(home, "cowork")
	claudeDir := filepath.Join(home, "claude")
	omnigentDir := filepath.Join(home, "omnigent")
	require.NoError(os.MkdirAll(aiderDir, 0o755))
	require.NoError(os.MkdirAll(coworkDir, 0o755))
	require.NoError(os.MkdirAll(claudeDir, 0o755))
	require.NoError(os.MkdirAll(omnigentDir, 0o755))

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentAider:    {aiderDir},
			parser.AgentCowork:   {coworkDir},
			parser.AgentClaude:   {claudeDir},
			parser.AgentOmnigent: {omnigentDir},
		},
	}
	targets := scheduledReconcileTargets(cfg)
	require.Len(targets, 2, "only opted-in providers are scheduled")
	assert.Equal(parser.AgentAider, targets[0].Agent)
	assert.Equal([]string{aiderDir}, targets[0].Roots)
	assert.Equal(parser.AgentOmnigent, targets[1].Agent)
	assert.Equal([]string{omnigentDir}, targets[1].Roots)
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
			assert := assert.New(t)
			require := require.New(t)

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
			require.NoError(database.SetSessionDataVersion(sessionID, db.CurrentDataVersion()))
			require.NoError(database.BaselineActiveSessionSourcePaths(
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

			assert.Empty(targets,
				"an unavailable physical root must defer its authoritative scope")
			preserved, err := database.GetSession(
				t.Context(), sessionID,
			)
			require.NoError(err)
			assert.NotNil(preserved,
				"scheduled reconciliation must preserve the archived session")
		})
	}
}

func TestScheduledReconcileDefersNestedUnavailableRoots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	parent := filepath.Join(t.TempDir(), "aider")
	require.NoError(os.MkdirAll(parent, 0o755))
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
	require.NoError(database.SetSessionDataVersion(sessionID, db.CurrentDataVersion()))
	require.NoError(database.BaselineActiveSessionSourcePaths(
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

	assert.Empty(targets,
		"a present root must defer with a missing nested same-agent scope: "+
			"the engine expands it back to the missing dir")
	preserved, err := database.GetSession(t.Context(), sessionID)
	require.NoError(err)
	assert.NotNil(preserved,
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
	assert := assert.New(t)

	engine := &fakeRemoteSourceSyncEngine{}
	runRemoteSourceSyncPass(t.Context(), engine, nil)
	assert.Empty(engine.calls, "no remote roots -> no scoped sync")

	roots := []string{"s3://bucket/machine/raw/claude"}
	runRemoteSourceSyncPass(t.Context(), engine, roots)
	require.Len(t, engine.calls, 1)
	assert.Equal(roots, engine.calls[0])
	assert.True(engine.since[0].IsZero(),
		"the pass must cover the full remote scope; unchanged objects skip on fingerprints")
}

func TestRunScheduledSyncPassCallsPerAgent(t *testing.T) {
	assert := assert.New(t)

	engine := &fakeScheduledEngine{}
	runScheduledSyncPass(t.Context(), engine, nil)
	assert.Empty(engine.calls, "no targets -> no reconciliation")

	runScheduledSyncPass(t.Context(), engine,
		[]scheduledReconcileTarget{{Agent: parser.AgentAider, Roots: []string{"/a"}}})
	require.Len(t, engine.calls, 1)
	assert.Equal(parser.AgentAider, engine.calls[0].Agent)
	assert.Equal([]string{"/a"}, engine.calls[0].Roots)
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
			assert := assert.New(t)

			logs := captureLogOutput(t)
			engine := &fakeScheduledEngine{err: tc.err}
			runScheduledSyncPass(t.Context(), engine,
				[]scheduledReconcileTarget{{
					Agent: parser.AgentAider, Roots: []string{"/a"},
				}},
			)

			output := logs.String()
			assert.Contains(output,
				"scheduled reconciliation started: targets=1")
			assert.Contains(output,
				"scheduled reconciliation finished: targets=1")
			assert.Contains(output, "duration=")
			assert.Contains(output, "outcome="+tc.wantOutcome)
		})
	}
}
