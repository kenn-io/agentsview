//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGActivityKeptSpawnTargets(t *testing.T) {
	t.Parallel()
	local, syncer, store := newUsageJournalFixture(t)
	var token string
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `SELECT value FROM sync_metadata WHERE key='usage_history_token'`).Scan(&token))
	_, err := store.DB().ExecContext(t.Context(), `UPDATE sync_metadata SET value='3' WHERE key='usage_change_journal_version'`)
	require.NoError(t, err)
	require.True(t, ensureUsageChangeJournalPG(t.Context(), store.DB()))
	var upgraded string
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `SELECT value FROM sync_metadata WHERE key='usage_history_token'`).Scan(&upgraded))
	assert.NotEqual(t, token, upgraded)
	require.True(t, ensureUsageChangeJournalPG(t.Context(), store.DB()))
	pgKeptSession(t, local, "parent", "project-a", "2026-06-12")
	pgKeptSession(t, local, "owner", "project-b", "2026-06-12")
	for _, id := range []string{"child-a", "child-b"} {
		pgKeptSession(t, local, id, "project-b", "2026-06-14")
		_, err := local.SetSessionExternalParent(t.Context(), id, "parent")
		require.NoError(t, err)
	}
	pgKeptPush(t, syncer, false)
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC", Project: "project-a"}
	_, err = store.DB().ExecContext(t.Context(), `INSERT INTO tool_calls(session_id,tool_name,category,message_ordinal,subagent_session_id) VALUES('owner','Task','Task',0,'child-a')`)
	require.NoError(t, err)
	pgKeptBuild(t, store, f, q, true)
	pgKeptBuild(t, store, f, q, false)
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
		got := pgKeptBuild(t, store, f, q, true)
		store.runtimeState().activityKept.DeleteSlots(func(string) bool { return true })
		store.runtimeState().activityInputs.DeleteSlots(func(string) bool { return true })
		requirePGKeptEqual(t, got, pgKeptBuild(t, store, f, q, true))
	}
}
