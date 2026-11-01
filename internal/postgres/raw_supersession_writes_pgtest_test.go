//go:build pgtest

package postgres

import (
	"fmt"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Count rows actually returned to the production identity matcher and eligibility
// reducer. Timing and query text cannot establish that unrelated identities stay
// out of ordinary per-group work.
func TestRawDerivedIdentityWorkIgnoresUnrelatedArchiveGrowth(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	activateFixture(t, t.Context(), f, "", "")
	pool, probe := activationObservedPool(t, f, false)
	sink, err := NewRawProjectionStore(pool, RawProjectionOptions{Tenant: f.tenant})
	require.NoError(t, err)
	measure := func(value bool) map[string]int {
		probe.mu.Lock()
		probe.rows = map[string]int{}
		probe.mu.Unlock()
		require.NoError(t, sink.SetCuration(t.Context(), "codex:portable", "starred", value))
		probe.mu.Lock()
		defer probe.mu.Unlock()
		return maps.Clone(probe.rows)
	}
	before := measure(false)
	assert.Equal(t, 2, before["candidates"], "one physical raw identity and one legacy identity")
	assert.Equal(t, 2, before["eligibility"], "materialization and supersession each reduce one group")
	assert.Equal(t, 1, before["previous"])
	for i := range 24 {
		id := fmt.Sprintf("unrelated-%d", i)
		_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES($1,'retained','device','codex')`, "codex:"+id)
		require.NoError(t, err)
		f.projectSession(t, fmt.Sprintf("device-unrelated-%d", i), id)
	}
	var mapsCount int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&mapsCount))
	require.Equal(t, 25, mapsCount, "growth contains real unrelated supersession pairs")
	assert.Equal(t, before, measure(true), "materialized/matched work stays confined to the identity closure")
}

// Two legacy identities connect three logical groups. Starting at either end
// must include the middle group's other alias and every physical competitor.
func TestRawDerivedClosureIncludesTransitiveCompetitorsAndWithdrawal(t *testing.T) {
	f, originalGroup := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	activateFixture(t, t.Context(), f, "", "")
	var originalAlias string
	require.NoError(t, f.runtime.QueryRow(`SELECT 'codex:portable~'||branch_id FROM raw_session_branches WHERE group_id=$1`, originalGroup).Scan(&originalAlias))
	project := func(device, id, key string) string {
		m, receipt := f.accept(t, device, "capture-"+device, "")
		out := projectionOutcome("competing")
		out.Outcome.Results[0].Result.Session.ID = id
		out.Outcome.Results[0].Result.Session.SourceSessionID = key
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, out))
		return receipt.Receipt
	}
	project("device-b", "codex:portable", "alternate")
	project("device-c", "codex:other", "alternate")
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('codex:other','legacy','device','codex')`)
	require.NoError(t, err)
	parent := project("device-d", "codex:other", "third")
	pool, probe := activationObservedPool(t, f, false)
	sink, err := NewRawProjectionStore(pool, RawProjectionOptions{Tenant: f.tenant})
	require.NoError(t, err)
	var physical int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE provenance_kind='raw'`).Scan(&physical))
	require.NoError(t, sink.SetCuration(t.Context(), originalAlias, "starred", false))
	assert.Equal(t, physical+2, probe.rows["candidates"], "all physical competitors and both connected legacy identities enter matching")
	var n int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&n))
	assert.Zero(t, n, "neither ambiguous identity may be recreated")
	// Removing the far end must still revisit the former owner and its old alias;
	// both legacy identities remain in the connected ambiguous component.
	m, _ := f.accept(t, "device-d", "withdraw-third", parent)
	out := projectionOutcome("unused")
	out.Outcome.Results = nil
	probe.rows = map[string]int{}
	require.NoError(t, sink.Project(t.Context(), f.lease(t, m), m, out))
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM sessions WHERE provenance_kind='raw'`).Scan(&physical))
	assert.Equal(t, physical+2, probe.rows["candidates"], "removed memberships retain the full former identity closure")
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions WHERE legacy_session_id='codex:portable'`).Scan(&n))
	assert.Zero(t, n)
}

func TestRawDerivedEmptyTrashDoesNoIdentityWork(t *testing.T) {
	f, _ := supersessionFixture(t)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	activateFixture(t, t.Context(), f, "", "")
	require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", false))
	pool, probe := activationObservedPool(t, f, false)
	sink, err := NewRawProjectionStore(pool, RawProjectionOptions{Tenant: f.tenant})
	require.NoError(t, err)
	n, err := sink.EmptyTrash(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Zero(t, probe.rows["candidates"])
	assert.Zero(t, probe.rows["eligibility"])
	assert.Zero(t, probe.rows["previous"])
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&n))
	assert.Equal(t, 1, n)
}
