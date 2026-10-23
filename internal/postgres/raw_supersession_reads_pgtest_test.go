//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// The same retained legacy edge must select the same children and tree roots
// that its public parent field names. Different clocks expose literal-target
// timing and accidental aggregation of divergent cohorts.
func TestRawSupersessionLegacyRelationalReads(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "parent", "")
	out := projectionOutcome("parent")
	out.Outcome.Results[0].Result.Session.StartedAt = time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	out.Outcome.Results[0].Result.Session.EndedAt = time.Date(2026, 1, 1, 0, 0, 17, 0, time.UTC)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,message_count,started_at,ended_at) VALUES
 ('codex:portable','legacy','device','codex',1,'2026-01-01T00:00:01Z','2026-01-01T00:00:04Z'),
 ('owner','child','device','claude',2,'2026-01-01T00:00:00Z','2026-01-01T00:01:00Z');
 UPDATE sessions SET parent_session_id='codex:portable',relationship_type='subagent',labels=ARRAY['child-label'] WHERE id='owner';
 INSERT INTO messages(session_id,ordinal,role,content,timestamp,has_tool_use) VALUES
 ('owner',0,'user','hello','2026-01-01T00:00:00Z',false),('owner',1,'assistant','calling','2026-01-01T00:00:01Z',true);
 INSERT INTO tool_calls(session_id,message_ordinal,tool_name,category,subagent_session_id) VALUES('owner',1,'Task','agent','codex:portable')`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	var group, legacy, rawVariant string
	require.NoError(t, f.runtime.QueryRow(`SELECT group_id,base_alias||'~'||(SELECT min(branch_id) FROM raw_session_branches) FROM raw_session_groups`).Scan(&group, &rawVariant))
	require.NoError(t, f.runtime.QueryRow(`SELECT hosted_legacy_alias('codex:portable')`).Scan(&legacy))
	check := func(parents, roots []string) {
		t.Helper()
		child, err := h.GetSession(t.Context(), "owner")
		require.NoError(t, err)
		require.NotNil(t, child)
		if len(parents) == 1 {
			require.Equal(t, new(parents[0]), child.ParentSessionID)
			assert.Empty(t, child.ParentSessionIDs)
		} else {
			assert.Nil(t, child.ParentSessionID)
			assert.ElementsMatch(t, parents, child.ParentSessionIDs)
		}
		for _, parent := range parents {
			children, err := h.GetChildSessions(t.Context(), parent)
			require.NoError(t, err)
			require.Len(t, children, 1)
			assert.Equal(t, "owner", children[0].ID)
			assert.Equal(t, new(parent), children[0].ParentSessionID)
		}
		page, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{IncludeOrphans: true, Limit: 20})
		require.NoError(t, err)
		assert.Equal(t, len(roots), page.Total, "mapped child must not become an orphan root")
		var ids []string
		for _, row := range page.Sessions {
			ids = append(ids, row.ID)
		}
		assert.ElementsMatch(t, append(append([]string{}, roots...), "owner"), ids)
		filter := db.SessionFilter{IncludeChildren: true, IncludeOrphans: true, Labels: []string{"child-label"}, Limit: 20}
		tree, err := h.ListSessions(t.Context(), filter)
		require.NoError(t, err)
		ids = nil
		for _, row := range tree.Sessions {
			ids = append(ids, row.ID)
		}
		assert.ElementsMatch(t, append(append([]string{}, parents...), "owner"), ids, "annotation walk reaches every mapped ancestor")
		annotated, err := h.GetSidebarSessionIndex(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, len(parents), annotated.Total)
		ids = nil
		for _, row := range annotated.Sessions {
			ids = append(ids, row.ID)
		}
		assert.ElementsMatch(t, append(append([]string{}, parents...), "owner"), ids)
		orphans, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Project: "child", IncludeOrphans: true, Limit: 20})
		require.NoError(t, err)
		assert.Zero(t, orphans.Total)
		assert.Empty(t, orphans.Sessions)
	}
	checkTiming := func(parent string, milliseconds int64) {
		t.Helper()
		timing, err := h.GetSessionTiming(t.Context(), "owner")
		require.NoError(t, err)
		require.NotNil(t, timing)
		require.NotNil(t, timing.SlowestCall)
		assert.Equal(t, new(parent), timing.SlowestCall.SubagentSessionID)
		require.NotNil(t, timing.SlowestCall.DurationMs)
		assert.Equal(t, milliseconds, *timing.SlowestCall.DurationMs)
	}
	check([]string{legacy}, []string{legacy, rawVariant})
	checkTiming(legacy, 3000)
	children, err := h.GetChildSessions(t.Context(), rawVariant)
	require.NoError(t, err)
	assert.Empty(t, children, "no map cannot attach a legacy child to raw")
	applySupersessionFixture(t, f, map[string]bool{group: true}, true)
	check([]string{"codex:portable"}, []string{"codex:portable"})
	checkTiming("codex:portable", 7000)
	second, _ := f.accept(t, "device-b", "divergent", "")
	divergent := projectionOutcome("different")
	divergent.Outcome.Results[0].Result.Session.StartedAt = time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	divergent.Outcome.Results[0].Result.Session.EndedAt = time.Date(2026, 1, 1, 0, 0, 41, 0, time.UTC)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, second), second, divergent))
	_, err = h.GetSession(t.Context(), "codex:portable")
	var ambiguous *db.SessionIdentityError
	require.ErrorAs(t, err, &ambiguous)
	require.Len(t, ambiguous.Variants, 2)
	check(ambiguous.Variants, ambiguous.Variants)
	_, err = h.GetSessionTiming(t.Context(), "owner")
	require.ErrorAs(t, err, &ambiguous)
	// Inspect the actual timing join before public singular-ID rejection, so a
	// bogus min(start)/max(end) interval cannot hide behind the ambiguity error.
	calls, err := h.physical.queryCallRowsWithJoin(t.Context(), "owner", hostedLegacyCallJoinSQL)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Empty(t, calls[0].SubagentStart)
	assert.Empty(t, calls[0].SubagentEnd)
	applySupersessionFixture(t, f, nil, false)
	check(ambiguous.Variants, ambiguous.Variants)
	applySupersessionFixture(t, f, nil, true)
	check([]string{legacy}, append(append([]string{}, ambiguous.Variants...), legacy))
	checkTiming(legacy, 3000)
	for _, parent := range ambiguous.Variants {
		children, err := h.GetChildSessions(t.Context(), parent)
		require.NoError(t, err)
		assert.Empty(t, children)
	}
}
