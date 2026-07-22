package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	require.NoError(t, os.MkdirAll(aiderDir, 0o755))
	require.NoError(t, os.MkdirAll(coworkDir, 0o755))
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))

	cfg := config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentAider:  {aiderDir},
			parser.AgentCowork: {coworkDir},
			parser.AgentClaude: {claudeDir},
		},
	}
	targets := scheduledReconcileTargets(cfg)
	require.Len(t, targets, 1, "only the opted-in provider is scheduled")
	assert.Equal(t, parser.AgentAider, targets[0].Agent)
	assert.Equal(t, []string{aiderDir}, targets[0].Roots)
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
