//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGActivityKeptSpawnTargets(t *testing.T) {
	t.Parallel()
	local, syncer, store := newUsageJournalFixture(t)
	pgKeptSession(t, local, "parent-a", "project-a", "2026-06-12")
	pgKeptSession(t, local, "parent-b", "project-b", "2026-06-12")
	pgKeptSession(t, local, "owner", "project-c", "2026-06-12")
	for _, suffix := range []string{"a", "b"} {
		id := "child-" + suffix
		pgKeptSession(t, local, id, "project-c", "2026-06-14")
		_, err := local.SetSessionExternalParent(t.Context(), id, "parent-"+suffix)
		require.NoError(t, err)
	}
	pgKeptPush(t, syncer, false)
	_, err := store.DB().ExecContext(t.Context(), `INSERT INTO tool_calls(session_id,tool_name,category,message_ordinal,subagent_session_id) VALUES('owner','Task','Task',0,'child-a')`)
	require.NoError(t, err)
	q := pgDayQuery(t, "2026-06-14", "UTC")
	filters := []db.AnalyticsFilter{{Timezone: "UTC", Project: "project-a"}, {Timezone: "UTC", Project: "project-b"}}
	for _, change := range []struct {
		query   string
		targets []string
		before  [2]int
		after   [2]int
	}{
		{`UPDATE tool_calls SET subagent_session_id='child-b' WHERE session_id='owner'`, []string{"child-a", "child-b"}, [2]int{0, 1}, [2]int{1, 0}},
		{`DELETE FROM tool_calls WHERE session_id='owner'`, []string{"child-b"}, [2]int{1, 0}, [2]int{1, 1}},
		{`INSERT INTO tool_calls(session_id,tool_name,category,message_ordinal,subagent_session_id) VALUES('owner','Task','Task',0,'child-a')`, []string{"child-a"}, [2]int{1, 1}, [2]int{0, 1}},
	} {
		store.runtimeState().activityKept.DeleteSlots(func(string) bool { return true })
		store.runtimeState().activityInputs.DeleteSlots(func(string) bool { return true })
		for i, f := range filters {
			before := pgKeptBuild(t, store, f, q, true)
			assert.Equal(t, change.before[i], before.Report.InteractivePeak.Agents)
			pgKeptBuild(t, store, f, q, false)
		}
		revision, err := store.UsageChangeRevision(t.Context())
		require.NoError(t, err)
		_, err = store.DB().ExecContext(t.Context(), change.query)
		require.NoError(t, err)
		_, ids, err := store.LoadUsageChanges(t.Context(), revision)
		require.NoError(t, err)
		for _, id := range change.targets {
			assert.Contains(t, ids, id)
		}
		var cached [2]activity.CandidateArtifacts
		for i, f := range filters {
			cached[i] = pgKeptBuild(t, store, f, q, true)
			assert.Equal(t, change.after[i], cached[i].Report.InteractivePeak.Agents)
		}
		store.runtimeState().activityKept.DeleteSlots(func(string) bool { return true })
		store.runtimeState().activityInputs.DeleteSlots(func(string) bool { return true })
		for i, f := range filters {
			requirePGKeptEqual(t, cached[i], pgKeptBuild(t, store, f, q, true))
		}
	}
}
