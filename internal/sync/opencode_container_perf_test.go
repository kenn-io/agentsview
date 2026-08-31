package sync_test

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newOpenCodeTestEngine(t *testing.T, env *testEnv) *sync.Engine {
	t.Helper()
	engine := sync.NewEngine(env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenCode: {env.opencodeDir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	return engine
}

// TestOpenCodeSessionPrefilterIssue1557 reproduces the archive-wide child scan
// caused by one changed session in a changed OpenCode container.
func TestOpenCodeSessionPrefilterIssue1557(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	oc.inTransaction(t, func(oc *openCodeTestDB) {
		for i := range 123 {
			seedOpenCodeSQLiteTextSession(
				t, oc, "proj", fmt.Sprintf("ses%05d", i),
				1779012000000, 1779012030000,
				"prompt", "answer",
			)
		}
	})

	first := env.engine.SyncAll(context.Background(), nil)
	require.False(t, first.Aborted, "initial sync aborted: %+v", first)
	require.Equal(t, 123, first.Synced)

	oc.updateSessionTime(t, "ses00000", 1779015630000)
	oc.replaceTextContent(
		t, "ses00000", "changed prompt", "changed answer",
		1779015600000,
	)

	scansBefore := parser.OpenCodeContainerChildScans()
	lookupsBefore := parser.OpenCodeSessionChildLookups()
	second := env.engine.SyncAll(context.Background(), nil)
	scans := parser.OpenCodeContainerChildScans() - scansBefore
	lookups := parser.OpenCodeSessionChildLookups() - lookupsBefore

	require.False(t, second.Aborted, "changed sync aborted: %+v", second)
	assert.Equal(t, 1, second.Synced,
		"the changed session must be archived")
	assert.Equal(t, 122, second.Skipped,
		"unchanged sessions must be skipped")
	if runtime.GOOS == "windows" {
		assert.Equal(t, int64(1), scans,
			"unavailable file identity must fail closed to full digest listing")
	} else {
		assert.Zero(t, scans,
			"a changed full pass must not scan every child row")
	}
	assert.LessOrEqual(t, lookups, int64(1),
		"the changed session's child lookup must stay bounded")
	assertMessageContent(
		t, env.db, "opencode:ses00000", "changed prompt", "changed answer",
	)
}

func TestOpenCodeChangedContainerStreamRehydratesWatermarkMetadata(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("file identity is unavailable; container fast path is disabled")
	}
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	for i := range 123 {
		seedOpenCodeSQLiteTextSession(
			t, oc, "proj", fmt.Sprintf("ses%05d", i),
			1779012000000, 1779012030000, "prompt", "answer",
		)
	}
	require.Equal(t, 123, env.engine.SyncAll(context.Background(), nil).Synced)

	oc.updateSessionTime(t, "ses00000", 1779015630000)
	oc.replaceTextContent(
		t, "ses00000", "changed prompt", "changed answer", 1779015600000,
	)
	scansBefore := parser.OpenCodeContainerChildScans()
	lookupsBefore := parser.OpenCodeSessionChildLookups()
	require.NoError(t, env.engine.ReconcileProviderRoots(
		t.Context(), parser.AgentOpenCode, []string{env.opencodeDir},
	))
	stats := env.engine.LastSyncStats()
	assert.Equal(t, 1, stats.Synced)
	assert.Equal(t, 122, stats.Skipped)
	assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore,
		"streamed changed-container reconciliation must avoid a full child scan")
	assert.LessOrEqual(t,
		parser.OpenCodeSessionChildLookups()-lookupsBefore, int64(1),
		"only the changed streamed member may resolve its child digest")
	assertMessageContent(
		t, env.db, "opencode:ses00000", "changed prompt", "changed answer",
	)
}

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
	childScans := make(map[int]int64)
	childLookups := make(map[int]int64)
	for _, n := range []int{20, 200} {
		t.Run(fmt.Sprintf("sessions_%d", n), func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
			oc := createOpenCodeDB(t, env.opencodeDir)
			oc.addProject(t, "proj", "/home/user/code/app")
			oc.inTransaction(t, func(oc *openCodeTestDB) {
				for i := range n {
					seedOpenCodeSQLiteTextSession(
						t, oc, "proj", fmt.Sprintf("ses%05d", i),
						1779012000000, 1779012030000,
						"prompt", "answer",
					)
				}
			})
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

			scansBefore := parser.OpenCodeContainerChildScans()
			lookupsBefore := parser.OpenCodeSessionChildLookups()
			stats := env.engine.SyncAll(context.Background(), nil)
			childScans[n] = parser.OpenCodeContainerChildScans() - scansBefore
			childLookups[n] = parser.OpenCodeSessionChildLookups() - lookupsBefore
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
	assert.Equal(t, childScans[20], childScans[200],
		"container child scans must not grow with container size")
	assert.Equal(t, childLookups[20], childLookups[200],
		"changed-session child lookups must not grow with container size")
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
			oc.inTransaction(t, func(oc *openCodeTestDB) {
				for i := range n {
					seedOpenCodeSQLiteTextSession(
						t, oc, "proj", fmt.Sprintf("ses%05d", i),
						1779012000000, 1779012030000,
						"prompt", "answer",
					)
				}
			})
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
// stored composite — and stays archived as-is until the next full digest
// verification, represented here by an explicit force pass. Both variants are
// pinned here: a replacement below the stored composite and an append above it.
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

	fullStats := env.engine.SyncAllForceParse(context.Background(), nil)
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

	fullStats = env.engine.SyncAllForceParse(context.Background(), nil)
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

// TestOpenCodeWatcherCatchesMetadataUpdateUnderChildDominatedComposite pins
// the like-for-like watermark comparison. The stored composite is a MAX over
// session, project, and child times, so when a child timestamp dominates it,
// a later metadata update (title, session/project time) can advance the
// session row while staying below the composite. Comparing the session-row
// watermark against the composite would wrongly skip that session on the
// watcher pass; comparing against the stored session/project metadata
// watermark recovered from the persisted digest catches it — still without
// touching the container's child tables.
func TestOpenCodeWatcherCatchesMetadataUpdateUnderChildDominatedComposite(
	t *testing.T,
) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	seedOpenCodeSQLiteTextSession(
		t, oc, "proj", "meta-mark",
		1779012000000, 1779012030000,
		"prompt", "answer",
	)
	// Children exceed both the previous and the soon-to-advance metadata
	// timestamps, so the stored composite is child-dominated.
	oc.mustExec(t, "raise children above all metadata times",
		"UPDATE part SET time_updated = ? WHERE session_id = ?",
		1779099999000, "meta-mark")
	require.Equal(t, 1, env.engine.SyncAll(context.Background(), nil).Synced)

	// Metadata advances past its own stored value but stays below the
	// child-dominated composite.
	oc.mustExec(t, "retitle session below the composite",
		"UPDATE session SET title = ?, time_updated = ? WHERE id = ?",
		"renamed by watcher", 1779012040000, "meta-mark")

	scansBefore := parser.OpenCodeContainerChildScans()
	require.NoError(t, env.engine.SyncPathsContext(
		context.Background(), []string{oc.path},
	))
	stats := env.engine.LastSyncStats()
	assert.Equal(t, 1, stats.Synced,
		"a metadata update below the child-dominated composite must "+
			"re-parse on the watcher pass")
	assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore,
		"the watcher pass must still not scan the container's child tables")

	// OpenCode's LLM-generated title lands in first_message.
	var firstMessage string
	require.NoError(t, env.db.Reader().QueryRow(
		"SELECT first_message FROM sessions WHERE id = ?",
		"opencode:meta-mark",
	).Scan(&firstMessage))
	assert.Equal(t, "renamed by watcher", firstMessage,
		"the watcher pass must archive the metadata update")
}

// TestOpenCodeIdleReconcilePassSkipsContainerChildScan pins the same
// trusted-container bound on the streamed reconciliation path: an idle
// ReconcileWatchRoots pass over a trusted, untouched container must not
// aggregate the child tables (its candidates all gate-skip). Child-only writes
// remain deferred during the verification interval; the force pass below
// represents the next complete digest verification.
func TestOpenCodeIdleReconcilePassSkipsContainerChildScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file identity is unavailable; idle passes require full digest listing")
	}
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "proj", "/home/user/code/app")
	for i := range 5 {
		seedOpenCodeSQLiteTextSession(
			t, oc, "proj", fmt.Sprintf("ses%05d", i),
			1779012000000, 1779099999000,
			"prompt", "answer",
		)
	}
	require.Equal(t, 5, env.engine.SyncAll(context.Background(), nil).Synced)

	scansBefore := parser.OpenCodeContainerChildScans()
	lookupsBefore := parser.OpenCodeSessionChildLookups()
	require.NoError(t, env.engine.ReconcileWatchRoots(
		context.Background(), []string{env.opencodeDir}, false,
	))
	assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore,
		"an idle reconcile pass must not aggregate the container's child "+
			"tables")
	assert.Zero(t, parser.OpenCodeSessionChildLookups()-lookupsBefore,
		"an idle reconcile pass must not pay per-session child lookups")
	assertMessageContent(
		t, env.db, "opencode:ses00000", "prompt", "answer",
	)

	// A child-only replacement below every watermark is deferred by the
	// interval policy. The explicit force pass carries the digest immediately.
	oc.replaceTextContent(
		t, "ses00000", "swapped prompt", "swapped answer", 1779012500000,
	)
	require.NoError(t, env.engine.ReconcileWatchRoots(
		context.Background(), []string{env.opencodeDir}, false,
	))
	forceStats := env.engine.SyncAllForceParse(context.Background(), nil)
	require.False(t, forceStats.Aborted, "force digest pass aborted: %+v", forceStats)
	assertMessageContent(
		t, env.db, "opencode:ses00000",
		"swapped prompt", "swapped answer",
	)
}

// TestOpenCodeIdleFullPassSkipsContainerChildScan pins that a periodic full
// pass over a trusted, untouched container does not aggregate the child
// tables at all: the container gate will skip every member before
// fingerprinting, so discovery lists the bounded watermark form instead of
// computing archive-sized child identities nothing reads. Child-only edits
// remain deferred during the interval. A fresh ordinary engine below has no
// verification timestamp, so its full digest pass reconciles the edit.
func TestOpenCodeIdleFullPassSkipsContainerChildScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file identity is unavailable; idle passes require full digest listing")
	}
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

	// A child-only replacement below every watermark is deferred by an ordinary
	// pass until the verification boundary.
	oc.replaceTextContent(
		t, "ses00000", "swapped prompt", "swapped answer", 1779012500000,
	)
	stats = newOpenCodeTestEngine(t, env).SyncAll(context.Background(), nil)
	assert.Equal(t, 1, stats.Synced,
		"the ordinary full digest pass must reconcile the child-only edit")
	assertMessageContent(
		t, env.db, "opencode:ses00000",
		"swapped prompt", "swapped answer",
	)
}

// TestOpenCodeHiddenChildChangesAreDetected drives the child digest's
// detection duties through one table. Every mutation here is invisible to
// the composite watermark — the session or project row already holds the
// highest timestamp, and counts or extrema are preserved where noted — so
// only the complete child identity carried in the stored digest can catch
// it on the next ordinary full pass (run on a fresh engine, whose missing
// verification stamp makes the digest listing due immediately).
func TestOpenCodeHiddenChildChangesAreDetected(t *testing.T) {
	deleteAssistant := func(t *testing.T, oc *openCodeTestDB) {
		oc.mustExec(t, "delete assistant parts",
			"DELETE FROM part WHERE session_id = ? AND message_id LIKE ?",
			"probe", "%assistant%")
		oc.mustExec(t, "delete assistant message",
			"DELETE FROM message WHERE session_id = ? AND id LIKE ?",
			"probe", "%assistant%")
	}
	for _, tc := range []struct {
		name string
		// seed builds session "probe"; nil seeds one text session whose
		// session row sits far above every child timestamp, so no later
		// child mutation can move the composite MAX.
		seed   func(t *testing.T, oc *openCodeTestDB)
		mutate func(t *testing.T, oc *openCodeTestDB)
		// viaReconcile routes the mutation through a reconciliation pass
		// first: sources rebuilt by FindSource carry no child digest, so
		// the empty fingerprint hash must read as no constraint, not as
		// fresh.
		viaReconcile bool
		// wantAbsent asserts removed content is gone; rows without it
		// assert the session re-parses (Synced == 1).
		wantAbsent string
	}{
		{
			// Deleting a child cannot lower the composite MAX, so without a
			// deletion-sensitive digest component the removed content would
			// stay archived indefinitely.
			name:       "deleted child under an unchanged composite max",
			mutate:     deleteAssistant,
			wantAbsent: "drop answer",
		},
		{
			name:         "deleted child via reconciliation",
			mutate:       deleteAssistant,
			viaReconcile: true,
			wantAbsent:   "drop answer",
		},
		{
			// Same number of messages and parts, timestamps still below the
			// session row's watermark, but different rows and content.
			name: "same-count child replacement below the watermark",
			mutate: func(t *testing.T, oc *openCodeTestDB) {
				oc.replaceTextContent(
					t, "probe", "swapped prompt", "swapped answer",
					1779012500000,
				)
			},
		},
		{
			// The children hold the highest timestamp, so a project rename
			// below it leaves MAX(...) unchanged; the digest has to carry
			// the session and project timestamps in their own right.
			name: "project rename below the child watermark",
			seed: func(t *testing.T, oc *openCodeTestDB) {
				oc.addProject(t, "proj", "/home/user/code/original-app")
				seedOpenCodeSQLiteTextSession(
					t, oc, "proj", "probe",
					1779012000000, 1779012030000,
					"stable prompt", "stable answer",
				)
				oc.mustExec(t, "raise child watermark",
					"UPDATE part SET time_updated = ? WHERE session_id = ?",
					1779099999000, "probe")
			},
			mutate: func(t *testing.T, oc *openCodeTestDB) {
				oc.updateProjectWorktree(
					t, "proj", "/home/user/code/renamed-app", 1779013000000,
				)
			},
		},
		{
			// The swapped middle row keeps every aggregate a digest could
			// reduce to — counts, timestamp sums, and min/max ids — so only
			// a complete child identity can tell the two states apart.
			name: "middle-row replacement preserving counts, sums and extrema",
			seed: func(t *testing.T, oc *openCodeTestDB) {
				oc.addProject(t, "proj", "/home/user/code/app")
				oc.addSession(t, "probe", "proj", 1779012000000, 1779099999000)
				oc.addMessage(t, "probe-msg-a", "probe", "user", 1779012000000)
				oc.addTextPart(t, "probe-part-a", "probe", "probe-msg-a",
					"alpha", 1779012000000)
				oc.addTextPart(t, "probe-part-m", "probe", "probe-msg-a",
					"middle", 1779012000001)
				oc.addTextPart(t, "probe-part-z", "probe", "probe-msg-a",
					"zulu", 1779012000002)
			},
			mutate: func(t *testing.T, oc *openCodeTestDB) {
				oc.mustExec(t, "delete middle part",
					"DELETE FROM part WHERE id = ?", "probe-part-m")
				oc.addTextPart(t, "probe-part-n", "probe", "probe-msg-a",
					"replaced", 1779012000001)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
			oc := createOpenCodeDB(t, env.opencodeDir)
			if tc.seed != nil {
				tc.seed(t, oc)
			} else {
				oc.addProject(t, "proj", "/home/user/code/app")
				seedOpenCodeSQLiteTextSession(
					t, oc, "proj", "probe",
					1779012000000, 1779099999000,
					"keep prompt", "drop answer",
				)
			}
			initial := env.engine.SyncAll(context.Background(), nil)
			require.False(t, initial.Aborted, "initial sync aborted: %+v", initial)
			require.Equal(t, 1, initial.Synced)
			if tc.wantAbsent != "" {
				// The absence assertion below is only meaningful if the
				// content was archived before the mutation removed it.
				archived := false
				for _, m := range fetchMessages(t, env.db, "opencode:probe") {
					archived = archived ||
						strings.Contains(m.Content, tc.wantAbsent)
				}
				require.True(t, archived,
					"the baseline must archive the soon-deleted content")
			}

			tc.mutate(t, oc)

			if tc.viaReconcile {
				require.NoError(t, env.engine.ReconcileWatchRoots(
					context.Background(), []string{env.opencodeDir}, false,
				))
			}
			stats := newOpenCodeTestEngine(t, env).SyncAll(
				context.Background(), nil,
			)
			require.False(t, stats.Aborted, "full pass aborted: %+v", stats)
			if !tc.viaReconcile {
				// The reconcile pass may already have done the write, so the
				// count only binds on the direct rows.
				assert.Equal(t, 1, stats.Synced,
					"a hidden child change must re-parse the session")
			}
			if tc.wantAbsent != "" {
				// Assert the observable outcome rather than which pass did
				// the write: the removed content must no longer be archived.
				for _, m := range fetchMessages(t, env.db, "opencode:probe") {
					assert.NotContains(t, m.Content, tc.wantAbsent,
						"deleted child content must not remain archived")
				}
			}
		})
	}
}
