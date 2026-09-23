//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostedReprovisionRestoresMigratedColumnsAndIndexes(t *testing.T) {
	f := newHostedFixture(t, "tenant-upgrade")
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('preserved','project','device','codex')`)
	require.NoError(t, err)
	_, err = f.admin.ExecContext(t.Context(), `ALTER TABLE sessions DROP COLUMN raw_content_revision; ALTER TABLE raw_ingest_jobs DROP COLUMN projection_selected; ALTER TABLE raw_content_revisions DROP COLUMN recency_state; DROP INDEX idx_tool_result_events_terminal; DROP INDEX raw_session_sources_source; DROP INDEX raw_session_sources_content; DROP INDEX raw_session_sources_physical`)
	require.NoError(t, err)
	_, constructorErr := NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant})
	require.Error(t, constructorErr)
	assert.Contains(t, constructorErr.Error(), "provisioning")
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	var project, revision, tenant string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT project,raw_content_revision,tenant_id FROM sessions WHERE id='preserved'`).Scan(&project, &revision, &tenant))
	assert.Equal(t, "project", project)
	assert.Empty(t, revision)
	assert.Equal(t, f.tenant, tenant)
	var exists bool
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT to_regclass('idx_tool_result_events_terminal') IS NOT NULL`).Scan(&exists))
	assert.True(t, exists)
	for _, index := range []string{"raw_session_sources_source", "raw_session_sources_content", "raw_session_sources_physical"} {
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, index).Scan(&exists))
		assert.True(t, exists, index)
	}
	_, constructorErr = NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant})
	require.NoError(t, constructorErr)
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
}
