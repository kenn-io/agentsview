package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
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

// TestReconcileTombstoneSchedulesDuplicateGroupRebuild pins the audit-facing
// reconciliation path: tombstoning a session removes it from duplicate
// membership, so the reconciliation must schedule the background rebuild —
// otherwise the archive audit leaves stale badges, roles, and usage
// suppression behind until an unrelated change triggers one.
func TestReconcileTombstoneSchedulesDuplicateGroupRebuild(t *testing.T) {
	emitter := &fakeEmitter{}
	env := setupTestEnv(t, WithEmitter(emitter))
	t.Cleanup(func() { env.db.Close() })

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-08-10T08:00:00Z",
			"I need you to research and create a comprehensive plan.").
		String()
	env.writeClaudeSession(t, "twin-project", "twin-a.jsonl", content)
	env.writeClaudeSession(t, "twin-project", "twin-b.jsonl", content)

	stats := env.engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	result, err := env.db.RebuildDuplicateGroups(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, result.Members, "setup must establish membership")
	// Drain the rebuild SyncAll scheduled so it cannot re-run after the
	// tombstone below and mask a missing reconcile-triggered rebuild.
	env.engine.WaitDuplicateGroupRebuildDrained()

	// Delete one twin's source and run the authoritative reconciliation:
	// the tombstone must remove it from membership via the scheduled
	// background rebuild.
	require.NoError(t, os.Remove(filepath.Join(
		env.claudeDir, "twin-project", "twin-b.jsonl")))
	_, tombstoned, err := env.engine.ReconcileWatchRootsWithStats(
		t.Context(), []string{env.claudeDir}, false, nil,
	)
	require.NoError(t, err)
	require.Equal(t, 1, tombstoned)

	// The rebuild is asynchronous; drain it before asserting.
	env.engine.WaitDuplicateGroupRebuildDrained()

	_, member := duplicateMembershipSync(t, env.db, "claude:twin-b")
	assert.False(t, member,
		"the scheduled rebuild must drop the tombstoned twin from membership")
}

// duplicateMembershipSync reads membership through the exported API for the
// sync_test package.
func duplicateMembershipSync(
	t *testing.T, d *db.DB, sessionID string,
) (db.DuplicateGroupMember, bool) {
	t.Helper()
	groups, err := d.ListDuplicateGroups(t.Context())
	require.NoError(t, err)
	for _, group := range groups {
		for _, member := range group.Members {
			if member.SessionID == sessionID {
				return member.DuplicateGroupMember, true
			}
		}
	}
	return db.DuplicateGroupMember{}, false
}
