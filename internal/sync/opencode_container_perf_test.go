package sync_test

import (
	"context"
	"fmt"
	"testing"

	"go.kenn.io/agentsview/internal/parser"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenCodeSharedContainerChangeIsPerSessionBounded pins the "background
// sync work is bounded by the changed batch, not total archive size" rule for
// shared SQLite containers.
//
// Every session in an OpenCode root lives in one physical opencode.db. Stamping
// that container's size onto each session's fingerprint made any single
// session's write change every other session's fingerprint, so one changed
// session re-parsed the whole root — on a production container that is
// thousands of sessions re-read out of a multi-GB database every time the
// watcher fires. The per-session composite mtime (session, project, and child
// message/part time_updated) replaces it, so a one-session change must leave
// every other session skipped regardless of how many there are.
func TestOpenCodeSharedContainerChangeIsPerSessionBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	rewritten := make(map[int]int)
	for _, n := range []int{20, 200} {
		t.Run(fmt.Sprintf("sessions_%d", n), func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
			oc := createOpenCodeDB(t, env.opencodeDir)
			oc.addProject(t, "proj", "/home/user/code/app")
			for i := range n {
				seedOpenCodeSQLiteTextSession(
					t, oc, "proj", fmt.Sprintf("ses%05d", i),
					1779012000000, 1779012030000,
					"prompt", "answer",
				)
			}
			require.Equal(t, n,
				env.engine.SyncAll(context.Background(), nil).Synced)

			// Change exactly one session. This also grows the shared
			// container file, which is precisely the signal that used to
			// invalidate every other session in it.
			oc.updateSessionTime(t, "ses00000", 1779015630000)
			oc.replaceTextContent(
				t, "ses00000", "changed prompt", "changed answer",
				1779015600000,
			)

			stats := env.engine.SyncAll(context.Background(), nil)
			require.False(t, stats.Aborted, "sync aborted: %+v", stats)
			assert.Equal(t, 1, stats.Synced,
				"only the changed session may be rewritten")
			assert.Equal(t, n-1, stats.Skipped,
				"every unchanged session in the shared container must skip")
			rewritten[n] = stats.Synced
		})
	}

	assert.Equal(t, rewritten[20], rewritten[200],
		"sessions rewritten for one changed session must not grow with "+
			"container size")
}

// TestOpenCodeDeletedChildIsDetected pins deletion sensitivity. The composite
// mtime is a MAX over session/project/child timestamps, so when the session or
// project row already holds the higher value — the common case on a real
// container — deleting a message or part does not move the max at all. Without
// a deletion-sensitive component the session looks fresh and the removed
// content stays archived indefinitely.
func TestOpenCodeDeletedChildIsDetected(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	// Session row timestamp is deliberately far ahead of every child, so a
	// deleted child cannot lower the composite.
	seedOpenCodeSQLiteTextSession(
		t, oc, "proj", "del-session",
		1779012000000, 1779099999000,
		"keep prompt", "drop answer",
	)

	stats := env.engine.SyncAll(context.Background(), nil)
	require.False(t, stats.Aborted)
	require.Equal(t, 1, stats.Synced)
	assertMessageContent(
		t, env.db, "opencode:del-session", "keep prompt", "drop answer",
	)

	// Remove the assistant message and its parts, leaving session and project
	// timestamps untouched.
	oc.mustExec(t, "delete assistant parts",
		"DELETE FROM part WHERE session_id = ? AND message_id LIKE ?",
		"del-session", "%assistant%")
	oc.mustExec(t, "delete assistant message",
		"DELETE FROM message WHERE session_id = ? AND id LIKE ?",
		"del-session", "%assistant%")

	stats = env.engine.SyncAll(context.Background(), nil)
	require.False(t, stats.Aborted)
	assert.Equal(t, 1, stats.Synced,
		"a deleted child must not be hidden behind an unchanged composite max")
}

// TestOpenCodeDeletedChildDetectedViaReconciliation covers the same deletion
// hole on the reconciliation path. Sources rebuilt by FindSource rather than
// carried from discovery metadata have no child digest, so the fingerprint hash
// is empty and the freshness gate treats it as no constraint.
func TestOpenCodeDeletedChildDetectedViaReconciliation(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	seedOpenCodeSQLiteTextSession(
		t, oc, "proj", "recon-del",
		1779012000000, 1779099999000,
		"keep prompt", "drop answer",
	)
	require.Equal(t, 1, env.engine.SyncAll(context.Background(), nil).Synced)

	oc.mustExec(t, "delete assistant parts",
		"DELETE FROM part WHERE session_id = ? AND message_id LIKE ?",
		"recon-del", "%assistant%")
	oc.mustExec(t, "delete assistant message",
		"DELETE FROM message WHERE session_id = ? AND id LIKE ?",
		"recon-del", "%assistant%")

	require.NoError(t, env.engine.ReconcileWatchRoots(
		context.Background(), []string{env.opencodeDir}, false,
	))
	env.engine.SyncAll(context.Background(), nil)

	// Assert the observable outcome rather than which pass did the write:
	// the removed assistant turn must no longer be archived.
	for _, m := range fetchMessages(t, env.db, "opencode:recon-del") {
		assert.NotContains(t, m.Content, "drop answer",
			"deleted child content must not remain archived")
	}
}

// TestOpenCodeSameCountChildReplacementIsDetected covers a replacement that
// preserves both child counts and leaves every new timestamp below the session
// row's already-higher watermark, so neither the watermark nor the counts move.
func TestOpenCodeSameCountChildReplacementIsDetected(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	seedOpenCodeSQLiteTextSession(
		t, oc, "proj", "swap-session",
		1779012000000, 1779099999000,
		"original prompt", "original answer",
	)
	require.Equal(t, 1, env.engine.SyncAll(context.Background(), nil).Synced)

	// Same number of messages and parts, timestamps still below the session
	// row's watermark, but different rows and different content.
	oc.replaceTextContent(
		t, "swap-session", "swapped prompt", "swapped answer", 1779012500000,
	)

	stats := env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, stats.Synced,
		"a same-count child replacement below the session watermark must "+
			"still change the fingerprint")
}
