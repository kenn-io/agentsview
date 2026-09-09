package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncengine "go.kenn.io/agentsview/internal/sync"
)

func TestOpenCodeVirtualEventDoesNotRecheckUnrelatedMembers(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2_%t", v2), func(t *testing.T) {
			var allocations []float64
			for _, count := range []int{8, 800} {
				t.Run(fmt.Sprint(count), func(t *testing.T) {
					env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
					oc := createOpenCodeDB(t, env.opencodeDir)
					oc.addProject(t, "project-a", "/workspace/project-a")
					oc.inTransaction(t, func(oc *openCodeTestDB) {
						for i := range count {
							seedOpenCodeSQLiteTextSession(t, oc, "project-a", fmt.Sprintf("ses%05d", i),
								1779012000000, 1779012030000, "prompt", "answer")
						}
					})
					if v2 {
						// The current beta has session_v2 and no v1 child tables.
						_, err := oc.db.Exec(`DROP TABLE part; DROP TABLE message;
 ALTER TABLE session RENAME TO session_v2;
 CREATE TABLE session_message (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES session_v2(id) ON DELETE CASCADE,
 type TEXT NOT NULL, seq INTEGER NOT NULL, time_created INTEGER NOT NULL,
 time_updated INTEGER NOT NULL, data TEXT NOT NULL);
 CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id, seq);
 CREATE INDEX session_message_session_type_seq_idx ON session_message(session_id, type, seq);
 CREATE INDEX session_message_session_time_created_id_idx ON session_message(session_id, time_created, id);
 CREATE INDEX session_message_time_created_idx ON session_message(time_created);
 INSERT INTO session_message SELECT 'v2_user_' || id, id, 'user', 1, time_created, time_updated,
 '{"text":"prompt"}' FROM session_v2;
 INSERT INTO session_message SELECT 'v2_assistant_' || id, id, 'assistant', 2, time_created, time_updated,
 '{"content":[{"type":"text","id":"text-a","text":"answer"}]}' FROM session_v2;`)
						require.NoError(t, err)
					}
					require.Equal(t, count, env.engine.SyncAll(t.Context(), nil).Synced)
					path := parser.OpenCodeSQLiteVirtualPath(oc.path, "ses00000")
					var syncErr error
					allocations = append(allocations, testing.AllocsPerRun(3, func() {
						syncErr = env.engine.SyncPathsContext(t.Context(), []string{path})
					}))
					require.NoError(t, syncErr)
					assertMessageContent(t, env.db, "opencode:ses00000", "prompt", "answer")
					// A genuinely removed virtual member still needs source-missing
					// reconciliation, and the persistent archive must retain its content.
					table := "session"
					if v2 {
						table = "session_v2"
					}
					_, err := oc.db.Exec("DELETE FROM " + table + " WHERE id = 'ses00000'")
					require.NoError(t, err)
					require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{path}))
					stored, err := env.db.GetSessionFull(t.Context(), "opencode:ses00000")
					require.NoError(t, err)
					require.NotNil(t, stored)
					assert.NotNil(t, stored.SourceMissingAt)
					assertMessageContent(t, env.db, "opencode:ses00000", "prompt", "answer")
					other, err := env.db.GetSessionFull(t.Context(), "opencode:ses00001")
					require.NoError(t, err)
					require.NotNil(t, other)
					assert.Nil(t, other.SourceMissingAt)
				})
			}
			assert.Less(t, allocations[1], allocations[0]*3,
				"an existing virtual member must not trigger archive-wide absence checks")
		})
	}
}

func TestOpenCodeMissingSidecarWorkStaysBounded(t *testing.T) {
	allocations := map[string][]float64{"-wal": {}, "-shm": {}}
	for _, count := range []int{8, 800} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
			oc := createOpenCodeDB(t, env.opencodeDir)
			_, err := oc.db.Exec("PRAGMA journal_mode=WAL")
			require.NoError(t, err)
			oc.addProject(t, "project-a", "/workspace/project-a")
			oc.inTransaction(t, func(oc *openCodeTestDB) {
				for i := range count {
					seedOpenCodeSQLiteTextSession(t, oc, "project-a", fmt.Sprintf("ses%05d", i),
						1779012000000, 1779012030000, "prompt", "answer")
				}
			})
			require.Equal(t, count, env.engine.SyncAll(t.Context(), nil).Synced)
			require.NoError(t, oc.db.Close())
			for _, suffix := range []string{"-wal", "-shm"} {
				work := testing.AllocsPerRun(3, func() {
					// Reader probes may recreate sidecars. Close a producer
					// connection before every sample, including warmup, so
					// every measured event still names a missing sidecar.
					writer, err := sql.Open("sqlite3", oc.path)
					require.NoError(t, err)
					_, err = writer.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
					require.NoError(t, err)
					require.NoError(t, writer.Close())
					require.NoFileExists(t, oc.path+suffix)
					require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{oc.path + suffix}))
				})
				allocations[suffix] = append(allocations[suffix], work)
			}
		})
	}
	for suffix, work := range allocations {
		require.Len(t, work, 2)
		t.Logf("%s: 8 sessions %.0f allocations; 800 sessions %.0f allocations", suffix, work[0], work[1])
		assert.Less(t, work[1], work[0]*3, "missing sidecar work must not scale with archive size")
	}
}

func TestOpenCodeFamilySidecarDeletionReconciliation(t *testing.T) {
	for _, provider := range []struct {
		agent    parser.AgentType
		filename string
	}{
		{parser.AgentOpenCode, "opencode.db"}, {parser.AgentKilo, "kilo.db"},
		{parser.AgentMiMoCode, "mimocode.db"}, {parser.AgentIcodemate, "icodemate.db"},
	} {
		t.Run(string(provider.agent), func(t *testing.T) {
			root := t.TempDir()
			archive := dbtest.OpenTestDB(t)
			engine := syncengine.NewEngine(archive, syncengine.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{provider.agent: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			oc := createOpenCodeLikeDB(t, filepath.Join(root, provider.filename), string(provider.agent))
			_, err := oc.db.Exec("PRAGMA journal_mode=WAL")
			require.NoError(t, err)
			oc.addProject(t, "project-a", "/workspace/project-a")
			for i := range 2 {
				seedOpenCodeSQLiteTextSession(t, oc, "project-a", fmt.Sprintf("ses%05d", i),
					1779012000000, 1779012030000, "prompt", "answer")
			}
			require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
			// Deleting a member then checkpointing the writer does not make the
			// surviving container disappear. The authoritative pass detects it.
			_, err = oc.db.Exec("DELETE FROM session WHERE id = 'ses00000'")
			require.NoError(t, err)
			require.NoError(t, oc.db.Close())
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{oc.path, oc.path + "-wal", oc.path + "-shm"}))
			id := string(provider.agent) + ":ses00000"
			stored, err := archive.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Nil(t, stored.SourceMissingAt)
			_, _, err = engine.ReconcileWatchRootsWithStats(t.Context(), []string{root}, true, nil)
			require.NoError(t, err)
			stored, err = archive.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.NotNil(t, stored.SourceMissingAt)
			assertMessageContent(t, archive, id, "prompt", "answer")
			// Container removal still marks the surviving member missing, without
			// removing its archived messages.
			require.NoError(t, os.Rename(oc.path, oc.path+".removed"))
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{oc.path, oc.path + "-wal", oc.path + "-shm"}))
			other := string(provider.agent) + ":ses00001"
			stored, err = archive.GetSessionFull(t.Context(), other)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.NotNil(t, stored.SourceMissingAt)
			assertMessageContent(t, archive, other, "prompt", "answer")
		})
	}
}
