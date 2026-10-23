//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestRawSupersessionSchemaFreshUpgradeAndPrivileges(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "schema", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("raw")))
	var group string
	require.NoError(t, f.runtime.QueryRow(`SELECT group_id FROM raw_session_groups`).Scan(&group))
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('legacy','retained','device','codex')`)
	require.NoError(t, err)
	for _, phase := range []string{"fresh", "upgrade"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "upgrade" {
				_, err = f.admin.Exec(`DROP TABLE raw_legacy_supersessions`)
				require.NoError(t, err)
				require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
				_, err = f.admin.Exec(`GRANT SELECT,INSERT,UPDATE,DELETE ON raw_legacy_supersessions TO "` + f.role + `"`)
				require.NoError(t, err)
			}
			require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
			_, err = f.runtime.Exec(`INSERT INTO raw_legacy_supersessions(legacy_session_id,group_id) VALUES('legacy',$1)`, group)
			require.NoError(t, err)
			for _, args := range [][3]string{{f.tenant, "absent", group}, {f.tenant, "legacy", "absent"}, {"other", "legacy", group}} {
				_, err = f.runtime.Exec(`INSERT INTO raw_legacy_supersessions(tenant_id,legacy_session_id,group_id) VALUES($1,$2,$3) ON CONFLICT(tenant_id,legacy_session_id) DO UPDATE SET group_id=EXCLUDED.group_id`, args[0], args[1], args[2])
				require.Error(t, err)
			}
			_, err = f.runtime.Exec(`DELETE FROM raw_session_groups WHERE group_id=$1`, group)
			require.Error(t, err)
			_, err = f.runtime.Exec(`DELETE FROM sessions WHERE id='legacy'`)
			require.Error(t, err)
			for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
				_, err = f.admin.Exec(`REVOKE ` + privilege + ` ON raw_legacy_supersessions FROM "` + f.role + `"`)
				require.NoError(t, err)
				require.ErrorContains(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull), "raw_legacy_supersessions")
				_, err = f.admin.Exec(`GRANT ` + privilege + ` ON raw_legacy_supersessions TO "` + f.role + `"`)
				require.NoError(t, err)
			}
			require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
		})
	}
	var project string
	var rls bool
	require.NoError(t, f.admin.QueryRow(`SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='raw_legacy_supersessions'::regclass`).Scan(&rls))
	assert.True(t, rls)
	wrong, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = wrong.Rollback() }()
	_, err = wrong.Exec(`SELECT set_config('agentsview.tenant_id','other',true)`)
	require.NoError(t, err)
	var hiddenCount int
	require.NoError(t, wrong.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&hiddenCount))
	assert.Zero(t, hiddenCount)
	_, err = wrong.Exec(`INSERT INTO raw_legacy_supersessions(legacy_session_id,group_id) VALUES('legacy',$1)`, group)
	require.Error(t, err)
	require.NoError(t, wrong.Rollback())
	require.NoError(t, f.runtime.QueryRow(`SELECT project FROM sessions WHERE id='legacy'`).Scan(&project))
	assert.Equal(t, "retained", project)
}

func supersessionFixture(t testing.TB) (projectionFixture, string) {
	t.Helper()
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "carry", "")
	outcome := projectionOutcome("retained")
	outcome.Outcome.Results[0].Result.Messages[0].SourceUUID = "uuid"
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,display_name,deleted_at,labels) VALUES('codex:portable','legacy','device','codex','legacy title',clock_timestamp(),ARRAY['retained-label']); INSERT INTO starred_sessions(session_id) VALUES('codex:portable'); INSERT INTO messages(session_id,ordinal,role,content,content_length,source_uuid) VALUES('codex:portable',9,'user','retained',8,'uuid'); INSERT INTO pinned_messages(session_id,message_id,ordinal,source_uuid,note) VALUES('codex:portable',9,9,'uuid','legacy note')`)
	require.NoError(t, err)
	var group string
	require.NoError(t, f.runtime.QueryRow(`SELECT group_id FROM raw_session_groups`).Scan(&group))
	return f, group
}

// This test-only orchestrator exercises the internal caller-owned transaction;
// production activation and its lock/fence protocol belong to the next unit.
func applySupersessionFixture(t *testing.T, f projectionFixture, served map[string]bool, commit bool) rawSupersessionPlan {
	t.Helper()
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, lockRawRolloutShared(t.Context(), tx))
	plan, err := prepareRawSupersessions(t.Context(), tx, served)
	require.NoError(t, err)
	for _, change := range plan.Changes {
		if change.After != "" {
			carry, err := prepareRawLegacyCarry(t.Context(), tx, change.Legacy, change.After)
			require.NoError(t, err)
			require.NoError(t, f.sink.applyRawLegacyCarry(t.Context(), tx, carry))
		}
	}
	require.NoError(t, writeRawSupersessionCache(t.Context(), tx, plan))
	require.NoError(t, applyRawLegacyVisibility(t.Context(), tx, plan))
	if commit {
		require.NoError(t, tx.Commit())
	}
	return plan
}

func TestRawSupersessionCarryRollbackAndRawOverrides(t *testing.T) {
	f, group := supersessionFixture(t)
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	carry, err := prepareRawLegacyCarry(t.Context(), tx, "codex:portable", group)
	require.NoError(t, err)
	require.Zero(t, carry.UnresolvedPins)
	require.Len(t, carry.Pins, 1)
	assert.Equal(t, 0, carry.Pins[0].Ordinal, "legacy ordinal 9 maps to the target ordinal")
	key := carry.Pins[0].MessageKey
	require.NoError(t, tx.Rollback())
	// A caller rollback must undo map, overlays, curation and derived visibility.
	plan := applySupersessionFixture(t, f, map[string]bool{group: true}, false)
	assert.True(t, plan.IdentityChanged)
	var n int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_curation`).Scan(&n))
	assert.Zero(t, n)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_legacy_supersessions`).Scan(&n))
	assert.Zero(t, n)
	var hidden bool
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='codex:portable'`).Scan(&hidden))
	assert.False(t, hidden)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM pinned_messages WHERE session_id<>'codex:portable'`).Scan(&n))
	assert.Zero(t, n)
	var originalName *string
	require.NoError(t, f.runtime.QueryRow(`SELECT display_name FROM sessions WHERE provenance_kind='raw'`).Scan(&originalName))
	assert.Nil(t, originalName)
	_, err = f.runtime.Exec(`INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,'','starred','false'),($1,'','trashed','false'),($1,'','display_name','null')`, group)
	require.NoError(t, err)
	_, err = f.runtime.Exec(`INSERT INTO raw_pins(group_id,branch_id,message_key,ordinal,content_revision,pinned,note) VALUES($1,'',$2,0,'edited',false,'raw unpin')`, group, key)
	require.NoError(t, err)
	applySupersessionFixture(t, f, map[string]bool{group: true}, true)
	var name *string
	var trashed, starred bool
	require.NoError(t, f.runtime.QueryRow(`SELECT display_name,deleted_at IS NOT NULL,EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=s.id) FROM sessions s WHERE provenance_kind='raw'`).Scan(&name, &trashed, &starred))
	assert.Nil(t, name)
	assert.False(t, trashed)
	assert.False(t, starred)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM pinned_messages WHERE session_id<>'codex:portable'`).Scan(&n))
	assert.Zero(t, n)
	applySupersessionFixture(t, f, nil, true)
	var labels []string
	require.NoError(t, f.runtime.QueryRow(`SELECT labels FROM sessions WHERE id='codex:portable'`).Scan(&labels))
	assert.Equal(t, []string{"retained-label"}, labels)
	require.NoError(t, f.runtime.QueryRow(`SELECT display_name,deleted_at IS NOT NULL,EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=s.id),rollout_hidden FROM sessions s WHERE id='codex:portable'`).Scan(&name, &trashed, &starred, &hidden))
	require.NotNil(t, name)
	assert.Equal(t, "legacy title", *name)
	assert.True(t, trashed)
	assert.True(t, starred)
	assert.False(t, hidden)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM messages WHERE session_id='codex:portable' AND content='retained'`).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM pinned_messages WHERE session_id='codex:portable' AND note='legacy note'`).Scan(&n))
	assert.Equal(t, 1, n)
	applySupersessionFixture(t, f, map[string]bool{group: true}, true)
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM raw_pins WHERE NOT pinned AND note='raw unpin'`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestRawSupersessionCarryPreservesBranchOverrides(t *testing.T) {
	f, group := supersessionFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO raw_curation(group_id,branch_id,field,value) SELECT group_id,branch_id,'display_name','"branch title"' FROM raw_session_branches; INSERT INTO raw_curation(group_id,branch_id,field,value) SELECT group_id,branch_id,'trashed','false' FROM raw_session_branches`)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	carry, err := prepareRawLegacyCarry(t.Context(), tx, "codex:portable", group)
	require.NoError(t, err)
	require.Len(t, carry.Pins, 1)
	require.NoError(t, tx.Rollback())
	_, err = f.runtime.Exec(`INSERT INTO raw_pins(group_id,branch_id,message_key,ordinal,content_revision,pinned,note) SELECT group_id,branch_id,$1,0,content_revision,false,'branch unpin' FROM raw_session_branches`, carry.Pins[0].MessageKey)
	require.NoError(t, err)
	applySupersessionFixture(t, f, map[string]bool{group: true}, true)
	var name string
	var trashed, starred bool
	require.NoError(t, f.runtime.QueryRow(`SELECT display_name,deleted_at IS NOT NULL,EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=s.id) FROM sessions s WHERE provenance_kind='raw'`).Scan(&name, &trashed, &starred))
	assert.Equal(t, "branch title", name)
	assert.False(t, trashed)
	assert.True(t, starred)
	var n int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM pinned_messages WHERE session_id<>'codex:portable'`).Scan(&n))
	assert.Zero(t, n)
}

func TestRawSupersessionHiddenLegacyLinksAndReplacement(t *testing.T) {
	f, group := supersessionFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,parent_session_id) VALUES('owner','p','m','claude','codex:portable')`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	// No-map control retains the existing literal legacy relationship.
	owner, err := h.GetSession(t.Context(), "owner")
	require.NoError(t, err)
	require.NotNil(t, owner.ParentSessionID)
	assert.Contains(t, *owner.ParentSessionID, "legacy~")
	applySupersessionFixture(t, f, map[string]bool{group: true}, true)
	owner, err = h.GetSession(t.Context(), "owner")
	require.NoError(t, err)
	require.Equal(t, new("codex:portable"), owner.ParentSessionID)
	// A stale cache cannot redirect a visible legacy target.
	_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=false WHERE id='codex:portable'`)
	require.NoError(t, err)
	owner, err = h.GetSession(t.Context(), "owner")
	require.NoError(t, err)
	require.NotNil(t, owner.ParentSessionID)
	assert.Contains(t, *owner.ParentSessionID, "legacy~")
	_, err = f.runtime.Exec(`UPDATE sessions SET rollout_hidden=true WHERE id='codex:portable'`)
	require.NoError(t, err)
	// A map cannot invent source-owned authority for a raw owner's absent edge.
	var rawOwner string
	require.NoError(t, f.runtime.QueryRow(`SELECT id FROM sessions WHERE provenance_kind='raw'`).Scan(&rawOwner))
	absentEdge := "codex:portable"
	rawRefs := hostedRefs{}
	rawRefs.link(rawOwner, &absentEdge)
	require.NoError(t, rawRefs.mapIDs(t.Context(), h))
	assert.Empty(t, absentEdge)

	// Replacing physical content leaves the logical map intact, including a
	// divergent second cohort and then a source tombstone retaining membership.
	m, accepted := f.accept(t, "device-b", "split", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("different")))
	owner, err = h.GetSession(t.Context(), "owner")
	require.NoError(t, err)
	assert.Nil(t, owner.ParentSessionID)
	require.Len(t, owner.ParentSessionIDs, 2)
	var privateID string
	require.NoError(t, f.runtime.QueryRow(`SELECT session_id FROM raw_session_branches WHERE source_id=$1`, rawSourceID(m)).Scan(&privateID))
	replacement, receipt := f.accept(t, "device-b", "replace", accepted.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, replacement), replacement, projectionOutcome("replacement")))
	var nextID string
	require.NoError(t, f.runtime.QueryRow(`SELECT session_id FROM raw_session_branches WHERE source_id=$1`, rawSourceID(m)).Scan(&nextID))
	assert.NotEqual(t, privateID, nextID)
	f.tombstone(t, replacement, "gone", receipt.Receipt)
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	plan, err := prepareRawSupersessions(t.Context(), tx, map[string]bool{group: true})
	require.NoError(t, err)
	assert.Empty(t, plan.Changes)
	assert.False(t, plan.IdentityChanged)
	var mapped string
	require.NoError(t, tx.QueryRow(`SELECT group_id FROM raw_legacy_supersessions WHERE legacy_session_id='codex:portable'`).Scan(&mapped))
	assert.Equal(t, group, mapped)
	require.NoError(t, tx.Rollback())
	// Singular references keep the established ambiguity error.
	value := "codex:portable"
	refs := hostedRefs{}
	refs.link("owner", &value)
	var identity *db.SessionIdentityError
	require.ErrorAs(t, refs.mapIDs(t.Context(), h), &identity)
	assert.Equal(t, "ambiguous", identity.State)
}

func TestRawSupersessionUnresolvedPins(t *testing.T) {
	f, group := supersessionFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO messages(session_id,ordinal,role,content,content_length,source_uuid) VALUES('codex:portable',10,'user','changed',7,'uuid'),('codex:portable',11,'user','missing',7,'absent'); INSERT INTO pinned_messages(session_id,message_id,ordinal,source_uuid,note) VALUES('codex:portable',10,10,'uuid','reused'),('codex:portable',11,11,'absent','missing')`)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	carry, err := prepareRawLegacyCarry(t.Context(), tx, "codex:portable", group)
	require.NoError(t, err)
	assert.Equal(t, 2, carry.UnresolvedPins)
	require.Len(t, carry.Pins, 1)
	assert.Equal(t, "legacy note", carry.Pins[0].Note)
}

func TestRawSupersessionCopiesAbsentCurationAndMovedPin(t *testing.T) {
	f, group := supersessionFixture(t)
	applySupersessionFixture(t, f, map[string]bool{group: true}, true)
	var name string
	var trashed bool
	require.NoError(t, f.runtime.QueryRow(`SELECT display_name,deleted_at IS NOT NULL FROM sessions WHERE provenance_kind='raw'`).Scan(&name, &trashed))
	assert.Equal(t, "legacy title", name)
	assert.True(t, trashed)
	var ordinal int
	require.NoError(t, f.runtime.QueryRow(`SELECT ordinal FROM pinned_messages WHERE session_id<>'codex:portable'`).Scan(&ordinal))
	assert.Equal(t, 0, ordinal)
}
