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

// TestOpenCodeWatcherEventIsWatermarkBounded pins the same rule for the
// watcher's changed-path pass, one level deeper: a one-session write must
// not read the container's child tables at all, and must not even
// materialize the unchanged sessions. Changed-path classification lists
// candidates through the bounded session-row watermark (no message/part
// aggregation) filtered by the container's newest stored watermark, so the
// sources processed and the child rows examined per event both scale with
// the changed batch and not with the archive.
func TestOpenCodeWatcherEventIsWatermarkBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	lookups := make(map[int]int64)
	processed := make(map[int]int)
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

			oc.updateSessionTime(t, "ses00000", 1779015630000)
			oc.replaceTextContent(
				t, "ses00000", "changed prompt", "changed answer",
				1779015600000,
			)

			scansBefore := parser.OpenCodeContainerChildScans()
			lookupsBefore := parser.OpenCodeSessionChildLookups()
			require.NoError(t, env.engine.SyncPathsContext(
				context.Background(), []string{oc.path},
			))
			stats := env.engine.LastSyncStats()
			assert.Equal(t, 1, stats.Synced,
				"only the changed session may be rewritten")
			assert.Zero(t, stats.Skipped,
				"unchanged sessions must not even be materialized as sources")
			assert.Zero(t,
				parser.OpenCodeContainerChildScans()-scansBefore,
				"a watcher event must not aggregate the whole container's "+
					"child tables")
			lookups[n] = parser.OpenCodeSessionChildLookups() - lookupsBefore
			processed[n] = stats.Synced + stats.Skipped + stats.Failed
			assertMessageContent(
				t, env.db, "opencode:ses00000",
				"changed prompt", "changed answer",
			)
		})
	}

	assert.Equal(t, lookups[20], lookups[200],
		"per-session child lookups for one changed session must not grow "+
			"with container size")
	assert.Equal(t, processed[20], processed[200],
		"sources processed for one changed session must not grow with "+
			"container size")
}

// TestOpenCodeWatcherPassDefersChildOnlyEditToFullDiscovery documents the
// staleness contract the watermark-only watcher pass trades on: a child-only
// write that leaves the session and project rows untouched is invisible to
// the session-row watermark — wherever its timestamps land relative to the
// stored composite — and stays archived as-is until the next full-discovery
// pass, whose child digest still reconciles it. Both variants are pinned
// here: a replacement below the stored composite and an append above it.
// Actively watched sessions do not rely on this path; the per-session
// watcher poll resolves the composite directly.
func TestOpenCodeWatcherPassDefersChildOnlyEditToFullDiscovery(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	// Session row far ahead of every child, so the replacement below stays
	// under the stored composite.
	seedOpenCodeSQLiteTextSession(
		t, oc, "proj", "below-mark",
		1779012000000, 1779099999000,
		"original prompt", "original answer",
	)
	require.Equal(t, 1, env.engine.SyncAll(context.Background(), nil).Synced)

	// Child-only replacement: same counts, new rows and content, timestamps
	// below the session row's watermark, session and project rows untouched.
	oc.replaceTextContent(
		t, "below-mark", "swapped prompt", "swapped answer", 1779012500000,
	)

	scansBefore := parser.OpenCodeContainerChildScans()
	lookupsBefore := parser.OpenCodeSessionChildLookups()
	require.NoError(t, env.engine.SyncPathsContext(
		context.Background(), []string{oc.path},
	))
	assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore,
		"the watcher pass must not scan child tables for a child-only edit")
	assert.Zero(t, parser.OpenCodeSessionChildLookups()-lookupsBefore,
		"a child-only edit below the watermark yields no candidates")
	assertMessageContent(
		t, env.db, "opencode:below-mark",
		"original prompt", "original answer",
	)

	fullStats := env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, fullStats.Synced,
		"full discovery must reconcile the deferred child-only edit")
	assertMessageContent(
		t, env.db, "opencode:below-mark",
		"swapped prompt", "swapped answer",
	)

	// Same deferral when the child write lands ABOVE the stored composite:
	// a new message appended with a fresh timestamp while the session row
	// stays untouched still cannot move the session-row watermark.
	oc.addMessage(
		t, "below-mark-msg-late", "below-mark", "assistant", 1779200000000,
	)
	oc.addTextPart(
		t, "below-mark-part-late", "below-mark", "below-mark-msg-late",
		"late answer", 1779200000000,
	)

	scansBefore = parser.OpenCodeContainerChildScans()
	lookupsBefore = parser.OpenCodeSessionChildLookups()
	require.NoError(t, env.engine.SyncPathsContext(
		context.Background(), []string{oc.path},
	))
	assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore,
		"the watcher pass must not scan child tables for an above-composite "+
			"child append")
	assert.Zero(t, parser.OpenCodeSessionChildLookups()-lookupsBefore,
		"an above-composite child-only append yields no candidates")
	assertMessageContent(
		t, env.db, "opencode:below-mark",
		"swapped prompt", "swapped answer",
	)

	fullStats = env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, fullStats.Synced,
		"full discovery must reconcile the deferred above-composite append")
	assertMessageContent(
		t, env.db, "opencode:below-mark",
		"swapped prompt", "swapped answer", "late answer",
	)
}

// TestOpenCodeFullPassSkipsAfterWatcherPassParse pins that a session parsed
// through the watermark-only watcher pass stores the full composite
// watermark and digest, not the cheap session-row watermark it was
// discovered with. The children deliberately end above the session row so
// the two values differ; if the cheap watermark leaked into the stored
// fingerprint, the next full pass would see a mismatch and re-parse an
// unchanged session with a fresh child lookup.
func TestOpenCodeFullPassSkipsAfterWatcherPassParse(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	for i := range 3 {
		seedOpenCodeSQLiteTextSession(
			t, oc, "proj", fmt.Sprintf("ses%05d", i),
			1779012000000, 1779012030000,
			"prompt", "answer",
		)
	}
	require.Equal(t, 3, env.engine.SyncAll(context.Background(), nil).Synced)

	oc.updateSessionTime(t, "ses00000", 1779015630000)
	oc.replaceTextContent(
		t, "ses00000", "changed prompt", "changed answer", 1779015600000,
	)
	oc.mustExec(t, "raise children above the session row",
		"UPDATE part SET time_updated = ? WHERE session_id = ?",
		1779099999000, "ses00000")

	require.NoError(t, env.engine.SyncPathsContext(
		context.Background(), []string{oc.path},
	))
	require.Equal(t, 1, env.engine.LastSyncStats().Synced)

	lookupsBefore := parser.OpenCodeSessionChildLookups()
	stats := env.engine.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Synced,
		"the full pass must not rewrite sessions the watcher pass stored")
	assert.Equal(t, 3, stats.Skipped)
	assert.Zero(t, parser.OpenCodeSessionChildLookups()-lookupsBefore,
		"full-pass skips must not pay per-session child lookups")
}

// TestOpenCodeIdleFullPassSkipsContainerChildScan pins that a periodic full
// pass over a trusted, untouched container does not aggregate the child
// tables at all: the container gate will skip every member before
// fingerprinting, so discovery lists the bounded watermark form instead of
// computing archive-sized child identities nothing reads. Any write breaks
// container trust, and the next full pass carries the complete digest again
// — including for child-only edits below every watermark.
func TestOpenCodeIdleFullPassSkipsContainerChildScan(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	for i := range 5 {
		// Session rows far ahead of every child, so the later child-only
		// replacement stays below the stored composite.
		seedOpenCodeSQLiteTextSession(
			t, oc, "proj", fmt.Sprintf("ses%05d", i),
			1779012000000, 1779099999000,
			"prompt", "answer",
		)
	}
	require.Equal(t, 5, env.engine.SyncAll(context.Background(), nil).Synced)

	scansBefore := parser.OpenCodeContainerChildScans()
	lookupsBefore := parser.OpenCodeSessionChildLookups()
	stats := env.engine.SyncAll(context.Background(), nil)
	assert.Zero(t, stats.Synced)
	assert.Equal(t, 5, stats.Skipped,
		"every session of a trusted container must gate-skip")
	assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore,
		"an idle full pass must not aggregate the container's child tables")
	assert.Zero(t, parser.OpenCodeSessionChildLookups()-lookupsBefore,
		"an idle full pass must not pay per-session child lookups")

	// A child-only replacement below every watermark breaks trust via the
	// container state, and the next full pass carries the digest again.
	oc.replaceTextContent(
		t, "ses00000", "swapped prompt", "swapped answer", 1779012500000,
	)
	stats = env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, stats.Synced,
		"a write breaks container trust and full discovery reconciles it")
	assertMessageContent(
		t, env.db, "opencode:ses00000",
		"swapped prompt", "swapped answer",
	)
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

// TestOpenCodeMetadataUpdateBelowWatermarkIsDetected covers a project worktree
// rename whose timestamp lands below an already-higher child watermark. The
// composite MAX cannot move in that case, so the digest has to carry the
// session and project timestamps in their own right.
func TestOpenCodeMetadataUpdateBelowWatermarkIsDetected(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/original-app")
	// Children hold the highest timestamp, so a later project rename below
	// that value leaves MAX(...) unchanged.
	seedOpenCodeSQLiteTextSession(
		t, oc, "proj", "below-watermark",
		1779012000000, 1779012030000,
		"stable prompt", "stable answer",
	)
	oc.mustExec(t, "raise child watermark",
		"UPDATE part SET time_updated = ? WHERE session_id = ?",
		1779099999000, "below-watermark")
	require.Equal(t, 1, env.engine.SyncAll(context.Background(), nil).Synced)

	// Rename below the child watermark.
	oc.updateProjectWorktree(
		t, "proj", "/home/user/code/renamed-app", 1779013000000,
	)

	stats := env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, stats.Synced,
		"a metadata update below the child watermark must still be detected")
}

// TestOpenCodeMiddleRowReplacementIsDetected covers a replacement that keeps
// every aggregate the digest currently reduces to: same counts, same timestamp
// sums, and the same min/max ids because the swapped row sorts strictly between
// the extrema. Only a complete child identity can tell these apart.
func TestOpenCodeMiddleRowReplacementIsDetected(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	oc.addSession(t, "mid", "proj", 1779012000000, 1779099999000)
	oc.addMessage(t, "mid-msg-a", "mid", "user", 1779012000000)
	// Three parts: a, m, z. The middle one gets swapped for a different id
	// carrying an identical timestamp, so count, sum and extrema all hold.
	oc.addTextPart(t, "mid-part-a", "mid", "mid-msg-a", "alpha", 1779012000000)
	oc.addTextPart(t, "mid-part-m", "mid", "mid-msg-a", "middle", 1779012000001)
	oc.addTextPart(t, "mid-part-z", "mid", "mid-msg-a", "zulu", 1779012000002)
	require.Equal(t, 1, env.engine.SyncAll(context.Background(), nil).Synced)

	oc.mustExec(t, "delete middle part",
		"DELETE FROM part WHERE id = ?", "mid-part-m")
	oc.addTextPart(
		t, "mid-part-n", "mid", "mid-msg-a", "replaced", 1779012000001,
	)

	stats := env.engine.SyncAll(context.Background(), nil)
	assert.Equal(t, 1, stats.Synced,
		"a middle-row replacement preserving counts, sums and extrema must "+
			"still change the fingerprint")
}
