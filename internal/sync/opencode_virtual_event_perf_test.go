package sync_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestOpenCodeVirtualEventDoesNotRecheckUnrelatedMembers(t *testing.T) {
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
			_, err := oc.db.Exec("DELETE FROM session WHERE id = 'ses00000'")
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
}

func TestOpenCodeMissingWALDoesNotRecheckUnrelatedMembers(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentOpenCode)
	oc := createOpenCodeDB(t, env.opencodeDir)
	oc.addProject(t, "project-a", "/workspace/project-a")
	oc.inTransaction(t, func(oc *openCodeTestDB) {
		for i := range 800 {
			seedOpenCodeSQLiteTextSession(t, oc, "project-a", fmt.Sprintf("ses%05d", i),
				1779012000000, 1779012030000, "prompt", "answer")
		}
	})
	require.Equal(t, 800, env.engine.SyncAll(t.Context(), nil).Synced)
	require.NoFileExists(t, oc.path+"-wal")
	var syncErr error
	baseline := testing.AllocsPerRun(3, func() {
		syncErr = env.engine.SyncPathsContext(t.Context(), []string{oc.path})
	})
	require.NoError(t, syncErr)
	removedWAL := testing.AllocsPerRun(3, func() {
		syncErr = env.engine.SyncPathsContext(t.Context(), []string{oc.path + "-wal"})
	})
	require.NoError(t, syncErr)
	t.Logf("container %.0f allocations; missing WAL %.0f allocations", baseline, removedWAL)
	assert.Less(t, removedWAL, baseline*3, "a vanished SQLite sidecar does not prove any session disappeared")
	stored, err := env.db.GetSessionFull(t.Context(), "opencode:ses00000")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Nil(t, stored.SourceMissingAt)
	assertMessageContent(t, env.db, "opencode:ses00000", "prompt", "answer")
	require.NoError(t, oc.db.Close())
	require.NoError(t, os.Rename(oc.path, oc.path+".removed"))
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{oc.path + "-wal"}))
	stored, err = env.db.GetSessionFull(t.Context(), "opencode:ses00000")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.NotNil(t, stored.SourceMissingAt)
	assertMessageContent(t, env.db, "opencode:ses00000", "prompt", "answer")
}
