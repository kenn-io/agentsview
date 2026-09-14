package sync_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSyncTriggersDuplicateGroupDetection(t *testing.T) {
	env := setupTestEnv(t)
	t.Cleanup(func() { env.db.Close() })

	// Two sessions with the same first prompt and start second, written
	// through the same agent source: the migration-twin shape.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-08-10T08:00:00Z",
			"I need you to research and create a comprehensive plan.").
		String()
	env.writeClaudeSession(t, "twin-project", "twin-a.jsonl", content)
	env.writeClaudeSession(t, "twin-project", "twin-b.jsonl", content)

	stats := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)

	// The end-of-sync rebuild may still be in flight; run it inline to
	// assert on the deterministic outcome.
	result, err := env.db.RebuildDuplicateGroups(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Groups)
	assert.Equal(t, 2, result.Members)
}

func TestSyncDuplicateGroupRebuildIsIdempotent(t *testing.T) {
	env := setupTestEnv(t)
	t.Cleanup(func() { env.db.Close() })

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-08-10T08:00:00Z",
			"I need you to research and create a comprehensive plan.").
		String()
	env.writeClaudeSession(t, "twin-project", "twin-a.jsonl", content)
	env.writeClaudeSession(t, "twin-project", "twin-b.jsonl", content)

	stats := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)

	// First inline pass establishes membership; the second must observe
	// no changes so the usage cache is not re-notified needlessly.
	first, err := env.db.RebuildDuplicateGroups(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, first.Groups)

	second, err := env.db.RebuildDuplicateGroups(t.Context())
	require.NoError(t, err)
	assert.Zero(t, second.NotifiedIDs)
	assert.Equal(t, 1, second.Groups)
}
