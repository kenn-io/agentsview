//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
)

func TestMigrationParityProvisionFreshAndUpgrade(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	require.NoError(t, checkMigrationParityCatalog(t.Context(), f.admin, f.schema))

	var first string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&first))
	require.NotEmpty(t, first)
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	require.NoError(t, checkMigrationParityCatalog(t.Context(), f.admin, f.schema))
	var second string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&second))
	assert.Equal(t, first, second)

	for _, statement := range []string{
		`UPDATE migration_parity_identity SET target_id='00000000-0000-4000-8000-000000000001'`,
		`DELETE FROM migration_parity_identity`,
		`TRUNCATE migration_parity_identity`,
	} {
		_, err := f.runtime.ExecContext(t.Context(), statement)
		assert.Error(t, err)
	}

	for _, table := range migrationParityTables {
		var protected bool
		require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT c.relrowsecurity AND c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2`, f.schema, table.Name).Scan(&protected))
		assert.True(t, protected, table.Name)
	}

	// Simulate a tenant adopted by the prior release, then exercise the owner
	// upgrade path. Its generated identity must again survive a rerun.
	_, err := f.admin.ExecContext(t.Context(), `DROP TABLE migration_parity_dependencies,migration_parity_members,migration_parity_sources,migration_parity_runs,migration_parity_identity CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	require.NoError(t, checkMigrationParityCatalog(t.Context(), f.admin, f.schema))
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA "`+f.schema+`" TO "`+f.role+`"`)
	require.NoError(t, err)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&first))
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&second))
	assert.Equal(t, first, second)
}

func TestMigrationParityUpgradeMarksUnreconstructableEvidenceStale(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	runID := "00000000-0000-4000-8000-000000000091"
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_runs(run_id,request,request_digest,batch_size,budget_sources)
		VALUES($1,'request',$2,1,1)`, runID, make([]byte, 32))
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,dependency_digest,verdict,code)
		VALUES($1,1,'source-old','raw','device-a','codex','root-a','manifest-a',1,'receipt-a',$2,'matched','pending')`, runID, make([]byte, 32))
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_members(run_id,init_epoch,source_id,member_key,member_digest,kind,mapping_state,baseline_fingerprint,verdict)
		VALUES($1,1,'source-old','key',$2,'session','exact',$3,'matched')`, runID, make([]byte, 32), make([]byte, parityFingerprintSize))
	require.NoError(t, err)
	_, err = f.admin.ExecContext(t.Context(), `ALTER TABLE migration_parity_sources
		DROP COLUMN source_key_sha256,DROP COLUMN captured_verdict,DROP COLUMN captured_code;
		ALTER TABLE migration_parity_members DROP COLUMN captured_verdict`)
	require.NoError(t, err)

	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	require.NoError(t, checkMigrationParityCatalog(t.Context(), f.admin, f.schema))
	var sourceKey, sourceCaptured, sourceCode, sourceVerdict string
	require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT source_key_sha256,captured_verdict,captured_code,verdict FROM migration_parity_sources WHERE run_id=$1`, runID).
		Scan(&sourceKey, &sourceCaptured, &sourceCode, &sourceVerdict))
	assert.Empty(t, sourceKey)
	assert.Equal(t, string(rawderive.ParityStale), sourceCaptured)
	assert.Equal(t, "dependency_changed", sourceCode)
	assert.Equal(t, string(rawderive.ParityStale), sourceVerdict)
	var memberCaptured, memberVerdict string
	require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT captured_verdict,verdict FROM migration_parity_members WHERE run_id=$1`, runID).
		Scan(&memberCaptured, &memberVerdict))
	assert.Equal(t, string(rawderive.ParityStale), memberCaptured)
	assert.Equal(t, string(rawderive.ParityStale), memberVerdict)
}
