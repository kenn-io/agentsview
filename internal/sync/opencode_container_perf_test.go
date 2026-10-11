package sync_test

import (
	"fmt"
	"strings"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeSessionPrefilterIssue1557(t *testing.T) {
	watcherLookups := make(map[int]int64)
	watcherProcessed := make(map[int]int)
	for _, n := range []int{20, 200} {
		for _, mode := range []string{"full", "streamed", "watcher"} {
			t.Run(fmt.Sprintf("sessions_%d/%s", n, mode), func(t *testing.T) {
				env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
				oc := createOpenCodeDB(t, env.opencodeDir)
				seedOpenCodeSQLiteTextSessions(t, oc, n, 1779012030000)
				require.Equal(t, n, env.engine.SyncAll(t.Context(), nil).Synced)
				before := openCodeLocalModifiedSnapshot(t, env.db, "opencode:ses00001")
				oc.updateSessionTime(t, "ses00000", 1779015630000)
				oc.replaceTextContent(t, "ses00000", "changed prompt", "changed answer", 1779015600000)
				engine := env.engine
				if mode != "watcher" {
					engine = newOpenCodeFamilySQLiteTestEngine(t, parser.AgentOpenCode, env.opencodeDir, env.db)
					t.Cleanup(engine.Close)
				}
				scansBefore := parser.OpenCodeContainerChildScans()
				lookupsBefore := parser.OpenCodeSessionChildLookups()
				var stats sync.SyncStats
				switch mode {
				case "streamed":
					require.NoError(t, engine.ReconcileProviderRoots(t.Context(), parser.AgentOpenCode, []string{env.opencodeDir}))
					stats = engine.LastSyncStats()
				case "watcher":
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{oc.path}))
					stats = engine.LastSyncStats()
				default:
					stats = engine.SyncAll(t.Context(), nil)
				}
				require.False(t, stats.Aborted)
				assert.Equal(t, 1, stats.Synced)
				if mode == "watcher" {
					assert.Zero(t, stats.Skipped)
					watcherProcessed[n] = stats.Synced + stats.Skipped + stats.Failed
				} else {
					assert.Equal(t, n-1, stats.Skipped)
				}
				assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore)
				lookups := parser.OpenCodeSessionChildLookups() - lookupsBefore
				if mode == "watcher" {
					watcherLookups[n] = lookups
				} else if mode == "streamed" {
					assert.LessOrEqual(t, lookups, int64(1))
				} else {
					assert.Equal(t, int64(1), lookups)
				}
				assertMessageContent(t, env.db, "opencode:ses00000", "changed prompt", "changed answer")
				assert.Equal(t, before, openCodeLocalModifiedSnapshot(t, env.db, "opencode:ses00001"))
				assertMessageContent(t, env.db, "opencode:ses00001", "prompt", "answer")
			})
		}
	}
	assert.Equal(t, watcherLookups[20], watcherLookups[200])
	assert.Equal(t, watcherProcessed[20], watcherProcessed[200])
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
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)

	// Child-only replacement: same counts, new rows and content, timestamps
	// below the session row's watermark, session and project rows untouched.
	oc.replaceTextContent(
		t, "below-mark", "swapped prompt", "swapped answer", 1779012500000,
	)

	scansBefore := parser.OpenCodeContainerChildScans()
	lookupsBefore := parser.OpenCodeSessionChildLookups()
	require.NoError(t, env.engine.SyncPathsContext(
		t.Context(), []string{oc.path},
	))
	assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore,
		"the watcher pass must not scan child tables for a child-only edit")
	assert.Zero(t, parser.OpenCodeSessionChildLookups()-lookupsBefore,
		"a child-only edit below the watermark yields no candidates")
	assertMessageContent(
		t, env.db, "opencode:below-mark",
		"original prompt", "original answer",
	)

	fullStats := env.engine.SyncAllForceParse(t.Context(), nil)
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
		t.Context(), []string{oc.path},
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

	fullStats = env.engine.SyncAllForceParse(t.Context(), nil)
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
	seedOpenCodeSQLiteTextSessions(t, oc, 3, 1779012030000)
	require.Equal(t, 3, env.engine.SyncAll(t.Context(), nil).Synced)

	oc.updateSessionTime(t, "ses00000", 1779015630000)
	oc.replaceTextContent(
		t, "ses00000", "changed prompt", "changed answer", 1779015600000,
	)
	oc.mustExec(t, "raise children above the session row",
		"UPDATE part SET time_updated = ? WHERE session_id = ?",
		1779099999000, "ses00000")

	require.NoError(t, env.engine.SyncPathsContext(
		t.Context(), []string{oc.path},
	))
	require.Equal(t, 1, env.engine.LastSyncStats().Synced)

	lookupsBefore := parser.OpenCodeSessionChildLookups()
	stats := env.engine.SyncAll(t.Context(), nil)
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
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)

	// Metadata advances past its own stored value but stays below the
	// child-dominated composite.
	oc.mustExec(t, "retitle session below the composite",
		"UPDATE session SET title = ?, time_updated = ? WHERE id = ?",
		"renamed by watcher", 1779012040000, "meta-mark")

	scansBefore := parser.OpenCodeContainerChildScans()
	require.NoError(t, env.engine.SyncPathsContext(
		t.Context(), []string{oc.path},
	))
	stats := env.engine.LastSyncStats()
	assert.Equal(t, 1, stats.Synced,
		"a metadata update below the child-dominated composite must "+
			"re-parse on the watcher pass")
	assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore,
		"the watcher pass must still not scan the container's child tables")

	// OpenCode's session title lands in session_name.
	var sessionName string
	require.NoError(t, env.db.Reader().QueryRow(t.Context(),
		"SELECT session_name FROM sessions WHERE id = ?",
		"opencode:meta-mark",
	).Scan(&sessionName))
	assert.Equal(t, "renamed by watcher", sessionName,
		"the watcher pass must archive the metadata update")
}

// Idle passes reuse verified container state through both discovery entry points.
func TestOpenCodeIdleFullPassSkipsContainerChildScan(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed_%t", streamed), func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
			oc := createOpenCodeDB(t, env.opencodeDir)
			seedOpenCodeSQLiteTextSessions(t, oc, 5, 1779099999000)
			require.Equal(t, 5, env.engine.SyncAll(t.Context(), nil).Synced)
			scans := parser.OpenCodeContainerChildScans()
			lookups := parser.OpenCodeSessionChildLookups()
			if streamed {
				require.NoError(t, env.engine.ReconcileWatchRoots(t.Context(), []string{env.opencodeDir}, false))
			} else {
				stats := env.engine.SyncAll(t.Context(), nil)
				assert.Zero(t, stats.Synced)
				assert.Equal(t, 5, stats.Skipped)
			}
			assert.Zero(t, parser.OpenCodeContainerChildScans()-scans)
			assert.Zero(t, parser.OpenCodeSessionChildLookups()-lookups)
			assertMessageContent(t, env.db, "opencode:ses00000", "prompt", "answer")
		})
	}
}

// TestOpenCodeHiddenChildChangesAreDetected drives the child digest's
// detection duties through one table. Every mutation here is invisible to
// the composite watermark — the session or project row already holds the
// highest timestamp, and counts or extrema are preserved where noted — so
// only the complete child identity carried in the stored digest can catch
// it on a complete pass after persisted verification is cleared.
func TestOpenCodeHiddenChildChangesAreDetected(t *testing.T) {
	deleteAssistant := func(t *testing.T, oc *openCodeTestDB) {
		t.Helper()

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
				t.Helper()

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
				t.Helper()

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
				t.Helper()

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
				t.Helper()

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
				t.Helper()

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
			initial := env.engine.SyncAll(t.Context(), nil)
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

			require.NoError(t, env.db.DeleteSyncStateByPrefix(t.Context(), "sqlite-container:"))
			if tc.viaReconcile {
				require.NoError(t, env.engine.ReconcileWatchRoots(t.Context(), []string{env.opencodeDir}, false))
			} else {
				unverified := newOpenCodeFamilySQLiteTestEngine(t, parser.AgentOpenCode, env.opencodeDir, env.db)
				t.Cleanup(unverified.Close)
				stats := unverified.SyncAll(t.Context(), nil)
				require.False(t, stats.Aborted, "full pass aborted: %+v", stats)
				assert.Equal(t, 1, stats.Synced, "a hidden child change must re-parse the session")
			}
			if tc.wantAbsent != "" {
				// Deletion must be visible immediately after the selected pass.
				for _, m := range fetchMessages(t, env.db, "opencode:probe") {
					assert.NotContains(t, m.Content, tc.wantAbsent,
						"deleted child content must not remain archived")
				}
			}
		})
	}
}

func seedOpenCodeSQLiteTextSessions(t *testing.T, oc *openCodeTestDB, n int, updated int64) {
	t.Helper()
	oc.inTransaction(t, func(oc *openCodeTestDB) {
		oc.addProject(t, "proj", "/home/user/code/app")
		for i := range n {
			seedOpenCodeSQLiteTextSession(t, oc, "proj", fmt.Sprintf("ses%05d", i), 1779012000000, updated, "prompt", "answer")
		}
	})
}
