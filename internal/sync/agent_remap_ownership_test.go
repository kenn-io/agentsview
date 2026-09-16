package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// These tests pin the source-owner contract behind agent remap: rules rewrite
// the display agent only, while freshness, baselines, and source-missing
// reconciliation keep resolving the session through its parser agent
// (sessions.source_agent). Before the column existed, remapping rewrote the
// only ownership key, so unchanged sources were reparsed forever and removed
// sources could never be marked missing.

func remapGooseToAugure(t *testing.T, database *db.DB) {
	t.Helper()
	_, err := database.CreateAgentRemapRule(context.Background(), db.AgentRemapRule{
		SourceAgent: string(parser.AgentGoose),
		TargetAgent: string(parser.AgentAugure),
		Enabled:     true,
	})
	require.NoError(t, err)
	preview, err := database.ApplyAgentRemapRulesFromSync(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, preview.MatchedSessions,
		"the goose session must be remapped to augure")
}

func TestAgentRemapKeepsUnchangedSourceSkipped(t *testing.T) {
	pathRoot, _, _ := writeSyncGooseDB(t)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGoose: {pathRoot},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	remapGooseToAugure(t, database)

	// The display agent changed; the source owner must not follow it.
	session, err := database.GetSession(context.Background(), "goose:session-001")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, string(parser.AgentAugure), session.Agent)
	assert.Equal(t, string(parser.AgentGoose), session.SourceAgent)

	// Freshness and baseline reconciliation key on the source owner, so an
	// unchanged source must still classify as skipped instead of being
	// reparsed (or worse, tombstoned as source-missing) every sync.
	runSyncAndAssert(t, engine, SyncStats{})
	require.NoError(t, engine.ReconcileProviderRoots(
		context.Background(), parser.AgentGoose, []string{pathRoot},
	))

	stored, err := database.GetSessionFull(
		context.Background(), "goose:session-001",
	)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Nil(t, stored.SourceMissingAt,
		"an unchanged remapped session must not lose its baseline proof")
	assert.Equal(t, string(parser.AgentGoose), stored.SourceAgent)
	assert.Equal(t, string(parser.AgentAugure), stored.Agent)
}

func TestAgentRemapSourceRemovalMarksMissing(t *testing.T) {
	pathRoot, _, sourceDB := writeSyncGooseDB(t)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGoose: {pathRoot},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	remapGooseToAugure(t, database)

	// Remove the source, then reconcile: the source-missing tombstone must
	// still find the remapped session through its original owner.
	_, err := sourceDB.Exec(`
		DELETE FROM usage_ledger WHERE session_id = 'session-001';
		DELETE FROM messages WHERE session_id = 'session-001';
		DELETE FROM sessions WHERE id = 'session-001';
	`)
	require.NoError(t, err)
	require.NoError(t, engine.ReconcileProviderRoots(
		context.Background(), parser.AgentGoose, []string{pathRoot},
	))

	archived, err := database.GetSessionFull(
		context.Background(), "goose:session-001",
	)
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
	assert.Equal(t, string(parser.AgentAugure), archived.Agent,
		"the remapped display agent survives source removal")
	assert.Equal(t, string(parser.AgentGoose), archived.SourceAgent,
		"the source owner survives source removal")
}

func TestAgentRemapResyncReappliesRules(t *testing.T) {
	pathRoot, dbPath, sourceDB := writeSyncGooseDB(t)
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGoose: {pathRoot},
		},
		Machine: "devbox",
	})
	t.Cleanup(engine.Close)
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	remapGooseToAugure(t, database)

	// A parser write refreshes the row: the upsert carries the parser agent
	// as the source owner (unchanged), and the post-write remap pass
	// rewrites the display agent again.
	insertSyncGooseMessage(t, sourceDB, "assistant",
		`[{"type":"text","text":"Review complete."}]`, 1_700_000_002)
	require.NoError(t, engine.SyncPathsContext(context.Background(), []string{dbPath}))

	session, err := database.GetSession(context.Background(), "goose:session-001")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, string(parser.AgentAugure), session.Agent,
		"a reparsed remapped session must land with rules applied")
	assert.Equal(t, string(parser.AgentGoose), session.SourceAgent)
}
