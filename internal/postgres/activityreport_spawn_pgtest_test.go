//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPGActivityKeptSpawnTargets(t *testing.T) {
	t.Parallel()
	local, syncer, store := newUsageJournalFixture(t)
	pgKeptSession(t, local, "parent", "project-a", "2026-06-12")
	pgKeptSession(t, local, "owner", "project-b", "2026-06-12")
	for _, id := range []string{"child-a", "child-b"} {
		pgKeptSession(t, local, id, "project-b", "2026-06-14")
		_, err := local.SetSessionExternalParent(t.Context(), id, "parent")
		require.NoError(t, err)
	}
	pgKeptPush(t, syncer, false)
	_, err := store.DB().ExecContext(t.Context(), `INSERT INTO tool_calls(session_id,tool_name,category,message_ordinal,subagent_session_id) VALUES('owner','Task','Task',0,'child-a')`)
	require.NoError(t, err)
	for _, change := range []struct {
		query   string
		targets []string
	}{
		{`UPDATE tool_calls SET subagent_session_id='child-b' WHERE session_id='owner'`, []string{"child-a", "child-b"}},
		{`DELETE FROM tool_calls WHERE session_id='owner'`, []string{"child-b"}},
		{`INSERT INTO tool_calls(session_id,tool_name,category,message_ordinal,subagent_session_id) VALUES('owner','Task','Task',0,'child-a')`, []string{"child-a"}},
	} {
		revision, err := store.UsageChangeRevision(t.Context())
		require.NoError(t, err)
		_, err = store.DB().ExecContext(t.Context(), change.query)
		require.NoError(t, err)
		_, ids, err := store.LoadUsageChanges(t.Context(), revision)
		require.NoError(t, err)
		for _, id := range change.targets {
			assert.Contains(t, ids, id)
		}
	}
}
