package sync

// ABOUTME: Tests that agent remap rules apply to every full-parse write path.
// ABOUTME: Covers the ordinary batch loop and the bulk batch, staged and not.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// remapRuleHarness opens a database with one enabled remap rule that maps
// goose sessions with an ossington-* model onto the augure-desktop agent.
func remapRuleHarness(t *testing.T) *db.DB {
	t.Helper()
	database := dbtest.OpenTestDB(t)
	_, err := database.CreateAgentRemapRule(
		t.Context(),
		db.AgentRemapRule{
			SourceAgent: "goose", ModelGlob: "ossington-*",
			TargetAgent: "augure-desktop", Enabled: true,
		},
	)
	require.NoError(t, err)
	return database
}

// gooseWrite builds a full-parse pendingWrite for a goose session whose
// single assistant message carries the given model.
func gooseWrite(id string, model string) pendingWrite {
	recordedAt := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	return pendingWrite{
		sess: parser.ParsedSession{
			ID:        id,
			Agent:     parser.AgentGoose,
			StartedAt: recordedAt,
			EndedAt:   recordedAt,
		},
		msgs: []parser.ParsedMessage{{
			Ordinal: 0, Role: parser.RoleUser, Content: "hi",
			Timestamp: recordedAt,
		}, {
			Ordinal: 1, Role: parser.RoleAssistant, Model: model,
			Timestamp: recordedAt,
		}},
	}
}

// TestWriteBatchOrdinaryLoopAppliesAgentRemapRules covers the ordinary
// per-session batch loop: it bypasses both writeIncremental and the bulk
// batch, so it must apply remap rules itself or fresh sessions keep the
// parser agent.
func TestWriteBatchOrdinaryLoopAppliesAgentRemapRules(t *testing.T) {
	database := remapRuleHarness(t)
	engine := NewEngine(database, EngineConfig{})
	t.Cleanup(engine.Close)

	outcome := engine.writeBatchWithOutcome(
		[]pendingWrite{
			gooseWrite("goose:remap", "ossington-5"),
			gooseWrite("goose:keep", "other-model"),
		},
		syncWriteDefault, true,
	)
	require.Equal(t, 2, outcome.writtenSessions)

	remapped, err := database.GetSession(t.Context(), "goose:remap")
	require.NoError(t, err)
	require.NotNil(t, remapped)
	assert.Equal(t, "augure-desktop", remapped.Agent,
		"a full-parse session matching a rule must land remapped")

	untouched, err := database.GetSession(t.Context(), "goose:keep")
	require.NoError(t, err)
	require.NotNil(t, untouched)
	assert.Equal(t, "goose", untouched.Agent,
		"a session whose models match no rule must keep the parser agent")
}

// TestWriteBatchBulkAppliesAgentRemapRules covers the bulk branch, both with
// and without the staged fast path, since staged writes continue past the
// post-commit batch remap.
func TestWriteBatchBulkAppliesAgentRemapRules(t *testing.T) {
	database := remapRuleHarness(t)
	engine := NewEngine(database, EngineConfig{})
	t.Cleanup(engine.Close)

	outcome := engine.writeBatchWithOutcome(
		[]pendingWrite{
			gooseWrite("goose:bulk", "ossington-5"),
		},
		syncWriteBulk, true,
	)
	require.Equal(t, 1, outcome.writtenSessions)

	stored, err := database.GetSession(t.Context(), "goose:bulk")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "augure-desktop", stored.Agent,
		"a bulk-path session matching a rule must land remapped")
}

// TestWriteBatchStagedOnlyBulkAppliesAgentRemapRules covers the staged-only
// bulk batch: a staged write never creates a db.SessionBatchWrite row, so the
// batch has zero entries and the function returned before the post-batch
// remap ran. The staged session must still land remapped.
func TestWriteBatchStagedOnlyBulkAppliesAgentRemapRules(t *testing.T) {
	database := remapRuleHarness(t)
	engine := NewEngine(database, EngineConfig{
		// The staged write runs signal recomputation through a closure that
		// needs the full engine; skipping it keeps the fixture to the write
		// path under test.
		DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)

	staged, err := newCodexStagingSink("", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = staged.Close() })

	write := gooseWrite("goose:staged", "ossington-5")
	write.staged = staged

	outcome := engine.writeBatchBulkWithOutcome(
		[]pendingWrite{write}, true,
	)
	require.Equal(t, 1, outcome.writtenSessions)

	stored, err := database.GetSession(t.Context(), "goose:staged")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "augure-desktop", stored.Agent,
		"a staged-only bulk batch must apply remap rules before returning")
}
