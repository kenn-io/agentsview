package sync_test

import (
	"database/sql"
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
	for _, n := range []int{20, 200} {
		for _, mode := range []string{"full", "streamed", "watcher"} {
			t.Run(fmt.Sprintf("sessions_%d/%s", n, mode), func(t *testing.T) {
				watcher := mode == "watcher"
				env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
				oc := createOpenCodeDB(t, env.opencodeDir)
				seedOpenCodeSQLiteTextSessions(t, oc, n, 1779012030000)
				require.Equal(t, n, env.engine.SyncAll(t.Context(), nil).Synced)
				before := openCodeLocalModifiedSnapshot(t, env.db, "opencode:ses00001")
				oc.updateSessionTime(t, "ses00000", 1779015630000)
				oc.replaceTextContent(t, "ses00000", "changed prompt", "changed answer", 1779015600000)
				engine := env.engine
				if !watcher {
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
				if watcher {
					assert.Zero(t, stats.Skipped)
				} else {
					assert.Equal(t, n-1, stats.Skipped)
				}
				assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore)
				lookups := parser.OpenCodeSessionChildLookups() - lookupsBefore
				switch mode {
				case "watcher":
					watcherLookups[n] = lookups
				case "streamed":
					assert.LessOrEqual(t, lookups, int64(1))
				default:
					assert.Equal(t, int64(1), lookups)
				}
				assertMessageContent(t, env.db, "opencode:ses00000", "changed prompt", "changed answer")
				assert.Equal(t, before, openCodeLocalModifiedSnapshot(t, env.db, "opencode:ses00001"))
				assertMessageContent(t, env.db, "opencode:ses00001", "prompt", "answer")
				if watcher {
					lookupsBefore = parser.OpenCodeSessionChildLookups()
					settled := engine.SyncAll(t.Context(), nil)
					assert.Zero(t, settled.Synced)
					assert.Equal(t, n, settled.Skipped)
					assert.Zero(t, parser.OpenCodeSessionChildLookups()-lookupsBefore)
				}
			})
		}
	}
	assert.Equal(t, watcherLookups[20], watcherLookups[200])
}

func TestOpenCodeWatcherPassDefersChildOnlyEditToFullDiscovery(t *testing.T) {
	for _, mode := range []string{"child below composite", "child above composite", "metadata below children"} {
		t.Run(mode, func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
			oc := createOpenCodeDB(t, env.opencodeDir)
			oc.addProject(t, "proj", "/workspace/app")
			updated := int64(1779099999000)
			if mode == "metadata below children" {
				updated = 1779012030000
			}
			seedOpenCodeSQLiteTextSession(t, oc, "proj", "probe", 1779012000000, updated, "original prompt", "original answer")
			if mode == "metadata below children" {
				oc.mustExec(t, "raise child timestamps", "UPDATE part SET time_updated = ? WHERE session_id = ?", 1779099999000, "probe")
			}
			require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
			switch mode {
			case "child below composite":
				oc.replaceTextContent(t, "probe", "swapped prompt", "swapped answer", 1779012500000)
			case "child above composite":
				oc.addMessage(t, "late", "probe", "assistant", 1779200000000)
				oc.addTextPart(t, "part-late", "probe", "late", "late answer", 1779200000000)
			default:
				oc.mustExec(t, "retitle below child timestamps", "UPDATE session SET title = ?, time_updated = ? WHERE id = ?", "renamed by watcher", 1779012040000, "probe")
			}
			scansBefore := parser.OpenCodeContainerChildScans()
			lookupsBefore := parser.OpenCodeSessionChildLookups()
			require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{oc.path}))
			assert.Zero(t, parser.OpenCodeContainerChildScans()-scansBefore)
			stats := env.engine.LastSyncStats()
			if mode == "metadata below children" {
				assert.Equal(t, 1, stats.Synced)
				name := openCodeStoredSession(t, env.db, "opencode:probe").SessionName
				require.NotNil(t, name)
				assert.Equal(t, "renamed by watcher", *name)
			} else {
				assert.Zero(t, parser.OpenCodeSessionChildLookups()-lookupsBefore)
			}
			assertMessageContent(t, env.db, "opencode:probe", "original prompt", "original answer")
		})
	}
}

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
		wantAbsent    string
		fingerprint   bool
		projectRename bool
	}{
		{
			name: "project rename advances composite", fingerprint: true, projectRename: true,
			seed: func(t *testing.T, oc *openCodeTestDB) {
				t.Helper()
				oc.addProject(t, "proj", "/workspace/original-app")
				seedOpenCodeSQLiteTextSession(t, oc, "proj", "probe", 1779012000000, 1779012030000, "stable prompt", "stable answer")
			},
			mutate: func(t *testing.T, oc *openCodeTestDB) {
				t.Helper()
				oc.updateProjectWorktree(t, "proj", "/workspace/renamed-app", 1779015630000)
			},
		},
		{
			name: "child replacement above the watermark", fingerprint: true,
			seed: func(t *testing.T, oc *openCodeTestDB) {
				t.Helper()
				oc.addProject(t, "proj", "/workspace/app")
				seedOpenCodeSQLiteTextSession(t, oc, "proj", "probe", 1779012000000, 1779012030000, "original prompt", "original answer")
			},
			mutate: func(t *testing.T, oc *openCodeTestDB) {
				t.Helper()
				oc.replaceTextContent(t, "probe", "changed prompt", "changed answer", 1779012600000)
			},
		},
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

			if tc.fingerprint {
				require.NoError(t, env.db.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET local_modified_at = ? WHERE id = ?", "2000-01-01T00:00:00.000Z", "opencode:probe")
					return err
				}))
			}
			before := openCodeStoredSession(t, env.db, "opencode:probe")
			if tc.projectRename {
				assert.Equal(t, "/workspace/original-app", before.Cwd)
				assert.Equal(t, "original_app", before.Project)
			}
			tc.mutate(t, oc)

			if !tc.projectRename {
				require.NoError(t, env.db.DeleteSyncStateByPrefix(t.Context(), "sqlite-container:"))
			}
			if tc.viaReconcile {
				require.NoError(t, env.engine.ReconcileWatchRoots(t.Context(), []string{env.opencodeDir}, false))
			} else {
				unverified := env.engine
				if !tc.projectRename {
					unverified = newOpenCodeFamilySQLiteTestEngine(t, parser.AgentOpenCode, env.opencodeDir, env.db)
					t.Cleanup(unverified.Close)
				}
				stats := unverified.SyncAll(t.Context(), nil)
				require.False(t, stats.Aborted, "full pass aborted: %+v", stats)
				assert.Equal(t, 1, stats.Synced, "a hidden child change must re-parse the session")
			}
			if tc.fingerprint {
				after := openCodeStoredSession(t, env.db, "opencode:probe")
				require.NotNil(t, before.FileMtime)
				require.NotNil(t, before.FileHash)
				require.NotNil(t, before.LocalModifiedAt)
				require.NotNil(t, after.FileMtime)
				require.NotNil(t, after.FileHash)
				require.NotNil(t, after.LocalModifiedAt)
				assert.Greater(t, *after.FileMtime, *before.FileMtime)
				assert.NotEqual(t, *before.FileHash, *after.FileHash)
				assert.Greater(t, *after.LocalModifiedAt, *before.LocalModifiedAt)
				if tc.projectRename {
					assert.Equal(t, "/workspace/renamed-app", after.Cwd)
					assert.Equal(t, "renamed_app", after.Project)
					assertMessageContent(t, env.db, "opencode:probe", "stable prompt", "stable answer")
				} else {
					assertMessageContent(t, env.db, "opencode:probe", "changed prompt", "changed answer")
				}
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
