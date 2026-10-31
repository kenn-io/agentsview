//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawRolloutOwnerAndRuntimeDiagnosticsAgree(t *testing.T) {
	f, _ := supersessionFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO messages(session_id,ordinal,role,content,source_uuid) VALUES('codex:portable',10,'user','changed','uuid'),('codex:portable',11,'user','missing','absent'); INSERT INTO pinned_messages(session_id,message_id,ordinal,source_uuid) VALUES('codex:portable',10,10,'uuid'),('codex:portable',11,11,'absent')`)
	require.NoError(t, err)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	activateFixture(t, t.Context(), f, "", "")
	scopes, err := ListRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "")
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.EqualValues(t, 1, scopes[0].Served)
	assert.Zero(t, scopes[0].Shadow)
	assert.EqualValues(t, 1, scopes[0].Superseded)
	assert.EqualValues(t, 2, scopes[0].UnresolvedPins)
	identity, err := rawsync.NewAuthIdentity(f.tenant, "device-a")
	require.NoError(t, err)
	status, err := f.jobs.ReadRawSyncStatus(t.Context(), identity)
	require.NoError(t, err)
	require.Len(t, status.Rollout, 1)
	assert.Equal(t, "active", status.Rollout[0].State)
	assert.Equal(t, scopes[0].Served, status.Rollout[0].Served)
	assert.Equal(t, scopes[0].Shadow, status.Rollout[0].Shadow)
	assert.Equal(t, scopes[0].Superseded, status.Rollout[0].Superseded)
	assert.Equal(t, scopes[0].UnresolvedPins, status.Rollout[0].UnresolvedPins)
	require.NoError(t, ChangeRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "", "shadow"))
	scopes, err = ListRawRollout(t.Context(), f.admin, f.schema, f.tenant, "", "")
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Zero(t, scopes[0].Served)
	assert.EqualValues(t, 1, scopes[0].Shadow)
	assert.Zero(t, scopes[0].Superseded)
	assert.Zero(t, scopes[0].UnresolvedPins)
}

// The schema binding supplies the hosted capability; missing control tables remain
// errors rather than being mistaken for an ordinary custody-only installation.
func TestRawRolloutStatusHostedCapability(t *testing.T) {
	f := newProjectionFixture(t)
	identity, err := rawsync.NewAuthIdentity(f.tenant, "device-a")
	require.NoError(t, err)
	status, err := f.jobs.ReadRawSyncStatus(t.Context(), identity)
	require.NoError(t, err)
	assert.NotNil(t, status.Rollout)
	assert.Empty(t, status.Rollout)
	wrong := identity
	wrong.TenantID = "other-tenant"
	_, err = f.jobs.ReadRawSyncStatus(t.Context(), wrong)
	assert.ErrorIs(t, err, rawsync.ErrUnauthorized)
	_, err = f.admin.Exec(`ALTER TABLE raw_rollout_scopes RENAME TO unavailable_rollout_scopes`)
	require.NoError(t, err)
	_, err = f.jobs.ReadRawSyncStatus(t.Context(), identity)
	assert.Error(t, err)
}
