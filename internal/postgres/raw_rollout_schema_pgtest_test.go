//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawRolloutSchema(t *testing.T) {
	f := newHostedFixture(t, "tenant-rollout")
	var hidden bool
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('legacy','p','m','codex')`)
	require.NoError(t, err)
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='legacy'`).Scan(&hidden))
	assert.False(t, hidden)
	_, err = f.runtime.Exec(`INSERT INTO raw_rollout_scopes(device_id,provider,state) VALUES('','','shadow')`)
	require.ErrorContains(t, err, "permission denied", "runtime must only read rollout scopes")
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	// Recreate the pre-rollout catalog, then run the additive owner upgrade.
	_, err = f.admin.Exec(`DROP TABLE raw_rollout_scopes; ALTER TABLE sessions DROP COLUMN rollout_hidden`)
	require.NoError(t, err)
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	_, err = f.admin.Exec(`GRANT SELECT ON raw_rollout_scopes TO "` + f.role + `"`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	require.NoError(t, f.runtime.QueryRow(`SELECT rollout_hidden FROM sessions WHERE id='legacy'`).Scan(&hidden))
	assert.False(t, hidden)
}
