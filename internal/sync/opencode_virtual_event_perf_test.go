package sync_test

import (
	"fmt"
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
