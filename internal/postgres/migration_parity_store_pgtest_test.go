//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func newParityReadRole(t *testing.T, f hostedFixture) *sql.DB {
	t.Helper()
	role := fmt.Sprintf("parity_reader_%x", rand.Uint64())
	_, err := f.admin.ExecContext(t.Context(), `CREATE ROLE "`+role+`" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := f.admin.ExecContext(context.Background(), `DROP OWNED BY "`+role+`"`)
		assert.NoError(t, cleanupErr)
		_, cleanupErr = f.admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS "`+role+`"`)
		assert.NoError(t, cleanupErr)
	})
	_, err = f.admin.ExecContext(t.Context(), `GRANT USAGE ON SCHEMA "`+f.schema+`" TO "`+role+`"; GRANT SELECT ON ALL TABLES IN SCHEMA "`+f.schema+`" TO "`+role+`"`)
	require.NoError(t, err)
	password := setHostedFixturePassword(t, f.admin, role)
	dsn, err := appendConnParams(testPGURL(t), map[string]string{"password": password, "user": role})
	require.NoError(t, err)
	reader, err := OpenHosted(dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reader.Close()) })
	return reader
}

func parityTestRequest() rawderive.ParityRequest {
	return rawderive.ParityRequest{
		RunID:           "00000000-0000-4000-8000-000000000001",
		RuntimeID:       "00000000-0000-4000-8000-000000000002",
		BaselineProfile: "before",
		Cohort: rawderive.ParityCohort{
			DeviceID: "device-a", Provider: parser.AgentClaude, RootID: "root-a",
		},
	}
}

func TestMigrationParityCreateResumeAndBindingAreImmutable(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, err)

	report, err := store.CreateOrResumeParity(t.Context(), parityTestRequest(), 32)
	require.NoError(t, err)
	assert.Equal(t, int64(1), report.RequestGeneration)
	assert.Equal(t, "requested", report.State)

	again, err := store.CreateOrResumeParity(t.Context(), parityTestRequest(), 1)
	require.NoError(t, err)
	assert.Equal(t, report.RequestGeneration, again.RequestGeneration)
	var runs, batch, budget int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*),max(batch_size),max(budget_sources) FROM migration_parity_runs`).Scan(&runs, &batch, &budget))
	assert.Equal(t, 1, runs)
	assert.Equal(t, 1, batch)
	assert.Equal(t, 1, budget)

	for _, changed := range []rawderive.ParityRequest{
		func() rawderive.ParityRequest { r := parityTestRequest(); r.Cohort.RootID = "root-b"; return r }(),
		func() rawderive.ParityRequest { r := parityTestRequest(); r.BaselineProfile = "after"; return r }(),
		func() rawderive.ParityRequest {
			r := parityTestRequest()
			r.RuntimeID = "00000000-0000-4000-8000-000000000004"
			return r
		}(),
	} {
		_, err = store.CreateOrResumeParity(t.Context(), changed, 1)
		require.ErrorContains(t, err, "binding_conflict")
	}
	unsafe := parityTestRequest()
	unsafe.RunID = `x' OR TRUE --`
	_, err = store.ReadParityReport(t.Context(), unsafe.RunID)
	require.ErrorContains(t, err, "invalid")

	binding := rawderive.ParityBinding{
		Request: parityTestRequest(), BaselineID: "00000000-0000-4000-8000-000000000003",
		Tenant: "finder-a", ObservedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
		Versions: rawderive.ParityVersions{
			Preparation: rawderive.ParityPreparationVersion, Projection: rawderive.ParityProjectionVersion,
			Comparison: rawderive.ParitySchemaVersion, Data: 1, Quality: 1,
		},
	}
	binding.Tenant = f.tenant
	otherTenant := binding
	otherTenant.Tenant = "tenant-b"
	lease := claimedParityLease(t, f, binding.Request, time.Hour)
	_, err = store.BindParity(t.Context(), lease, otherTenant)
	require.ErrorContains(t, err, "binding_conflict")
	lease, err = store.BindParity(t.Context(), lease, binding)
	require.NoError(t, err)
	assert.NotEqual(t, rawderive.ParityDigest{}, lease.BindingDigest)
	assert.Equal(t, binding.ObservedAt, lease.ObservedAt)
	binding.Versions.Data++
	_, err = store.BindParity(t.Context(), lease, binding)
	require.ErrorContains(t, err, "binding_conflict")
}

func claimedParityLease(t *testing.T, f hostedFixture, request rawderive.ParityRequest, duration time.Duration) rawderive.ParityLease {
	t.Helper()
	const token = "00000000-0000-4000-8000-000000000077"
	const owner = "parity-owner"
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET lease_token=$2,lease_owner=$3,lease_expires_at=clock_timestamp()+make_interval(secs=>$4) WHERE run_id=$1`, request.RunID, token, owner, duration.Seconds())
	require.NoError(t, err)
	return rawderive.ParityLease{RunID: request.RunID, Request: request, RequestGeneration: 1, InitEpoch: 1, Token: token, Owner: owner}
}

func TestMigrationParityBindRequiresCurrentLeaseAndStableObservationClock(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, err)
	_, err = store.CreateOrResumeParity(t.Context(), parityTestRequest(), 1)
	require.NoError(t, err)
	binding := parityPhysicalBinding(f.tenant)
	binding.Request = parityTestRequest()
	binding.ObservedAt = binding.ObservedAt.In(time.FixedZone("synthetic-offset", 2*60*60))
	valid := claimedParityLease(t, f, binding.Request, time.Hour)

	for _, test := range []struct {
		name  string
		lease rawderive.ParityLease
	}{
		{"empty token", func() rawderive.ParityLease { l := valid; l.Token = ""; return l }()},
		{"wrong token", func() rawderive.ParityLease { l := valid; l.Token = "00000000-0000-4000-8000-000000000078"; return l }()},
		{"wrong owner", func() rawderive.ParityLease { l := valid; l.Owner = "replacement"; return l }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, bindErr := store.BindParity(t.Context(), test.lease, binding)
			require.ErrorContains(t, bindErr, "binding_conflict")
		})
	}

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1`, binding.Request.RunID)
	require.NoError(t, err)
	_, err = store.BindParity(t.Context(), valid, binding)
	require.ErrorContains(t, err, "binding_conflict")

	valid = claimedParityLease(t, f, binding.Request, time.Hour)
	submicrosecond := binding
	submicrosecond.ObservedAt = submicrosecond.ObservedAt.Add(time.Nanosecond)
	_, err = store.BindParity(t.Context(), valid, submicrosecond)
	require.ErrorContains(t, err, "microsecond")

	inconsistent := valid
	inconsistent.ObservedAt = binding.ObservedAt.Add(time.Microsecond)
	_, err = store.BindParity(t.Context(), inconsistent, binding)
	require.ErrorContains(t, err, "binding_conflict")

	fixed, err := store.BindParity(t.Context(), valid, binding)
	require.NoError(t, err)
	assert.Equal(t, binding.ObservedAt.UTC(), fixed.ObservedAt)
	var stored time.Time
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT observed_at FROM migration_parity_runs WHERE run_id=$1`, binding.Request.RunID).Scan(&stored))
	assert.Equal(t, binding.ObservedAt.UTC(), stored.UTC())
}

func TestMigrationParityHistoricalReportNeverClaimsCurrent(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, err)
	_, err = store.CreateOrResumeParity(t.Context(), parityTestRequest(), 32)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET state='complete',completed_generation=1,baseline_sealed=TRUE,validation_state='checked',baseline_observed_at=transaction_timestamp(),runtime_observed_at=transaction_timestamp() WHERE run_id=$1`, parityTestRequest().RunID)
	require.NoError(t, err)

	ordinary, err := store.ReadParityReport(t.Context(), parityTestRequest().RunID)
	require.NoError(t, err)
	assert.False(t, ordinary.Passing)
	assert.Equal(t, "historical_evidence", ordinary.Code)

	requested, err := store.ReadParityRequestReport(t.Context(), parityTestRequest().RunID, 2)
	require.NoError(t, err)
	assert.False(t, requested.Passing)
	assert.Equal(t, "generation_not_available", requested.Code)
}

func TestMigrationParityReportUsesOnlyLatestCompletedValidationDelta(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, err)
	_, err = store.CreateOrResumeParity(t.Context(), parityTestRequest(), 32)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_receipt,head_generation,dependency_digest,verdict,code,required)
		VALUES($1,1,'source-a','raw','device-a','claude','root-a','manifest-a','receipt-a',1,decode(repeat('01',32),'hex'),'matched','pending',TRUE)`, parityTestRequest().RunID)
	require.NoError(t, err)
	for _, row := range []struct {
		id      string
		epoch   int
		verdict string
	}{
		{"source-validation-old", 1, "stale"},
		{"source-validation-current", 2, "missing"},
	} {
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_receipt,head_generation,dependency_digest,verdict,code,required,candidate_complete,validation_epoch,inventory_kind)
			VALUES($1,1,$2,'raw','device-a','claude','root-a','manifest','receipt',1,decode(repeat('02',32),'hex'),$3,'dependency_changed',TRUE,TRUE,$4,'validation_delta')`,
			parityTestRequest().RunID, row.id, row.verdict, row.epoch)
		require.NoError(t, err)
	}
	for i, row := range []struct {
		inventory string
		epoch     int
		verdict   string
	}{
		{"baseline", 0, "matched"},
		{"candidate_only", 0, "mismatched"},
		{"validation_delta", 1, "stale"},
		{"validation_delta", 2, "missing"},
	} {
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_members(run_id,init_epoch,source_id,member_key,member_digest,kind,mapping_state,verdict,validation_epoch,inventory_kind,required)
			VALUES($1,1,'source-a',$2::text::bytea,decode(md5($2::text)||md5($2::text),'hex'),'session','exact',$3,$4,$5,TRUE)`, parityTestRequest().RunID, fmt.Sprintf("member-%d", i), row.verdict, row.epoch, row.inventory)
		require.NoError(t, err)
	}
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET state='complete',completed_generation=1,baseline_sealed=TRUE,validation_state='checked',validation_epoch=2,baseline_observed_at=transaction_timestamp(),runtime_observed_at=transaction_timestamp() WHERE run_id=$1`, parityTestRequest().RunID)
	require.NoError(t, err)
	report, err := store.ReadParityRequestReport(t.Context(), parityTestRequest().RunID, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), report.Members.Matched)
	assert.Equal(t, int64(1), report.Members.Mismatched)
	assert.Equal(t, int64(1), report.Members.Missing)
	assert.Zero(t, report.Members.Stale)
	assert.Equal(t, int64(1), report.Sources.Matched)
	assert.Equal(t, int64(1), report.Sources.Missing)
	assert.Zero(t, report.Sources.Stale)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET validation_state='validating' WHERE run_id=$1`, parityTestRequest().RunID)
	require.NoError(t, err)
	report, err = store.ReadParityRequestReport(t.Context(), parityTestRequest().RunID, 1)
	require.NoError(t, err)
	assert.Zero(t, report.Members.Missing, "incomplete validation deltas are staging, not report evidence")
	assert.Equal(t, int64(1), report.Sources.Matched)
	assert.Zero(t, report.Sources.Missing, "incomplete source deltas are staging, not report evidence")
	assert.Zero(t, report.Sources.Stale)
}

func TestMigrationParityCompletedReportRequiresCompleteMembersAndSources(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, err)
	_, err = store.CreateOrResumeParity(t.Context(), parityTestRequest(), 2)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_receipt,head_generation,dependency_digest,verdict,code,candidate_complete,required)
		VALUES($1,1,'source-a','raw','device-a','claude','root-a','manifest-a','receipt-a',1,decode(repeat('01',32),'hex'),'matched','pending',TRUE,TRUE)`, parityTestRequest().RunID)
	require.NoError(t, err)
	for _, member := range []string{"member-a", "member-b"} {
		_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_members(run_id,init_epoch,source_id,member_key,member_digest,kind,mapping_state,verdict,inventory_kind,required)
			VALUES($1,1,'source-a',$2::text::bytea,decode(md5($2::text)||md5($2::text),'hex'),'session','exact','matched','baseline',TRUE)`, parityTestRequest().RunID, member)
		require.NoError(t, err)
	}
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET state='complete',completed_generation=1,baseline_sealed=TRUE,validation_state='checked',baseline_observed_at=transaction_timestamp(),runtime_observed_at=transaction_timestamp() WHERE run_id=$1`, parityTestRequest().RunID)
	require.NoError(t, err)

	report, err := store.ReadParityRequestReport(t.Context(), parityTestRequest().RunID, 1)
	require.NoError(t, err)
	assert.True(t, report.Complete)
	assert.True(t, report.Passing)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_members SET verdict=NULL WHERE member_key='member-b'::bytea`)
	require.NoError(t, err)
	report, err = store.ReadParityRequestReport(t.Context(), parityTestRequest().RunID, 1)
	require.NoError(t, err)
	assert.False(t, report.Complete, "a required member without a verdict remains pending")
	assert.False(t, report.Passing)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_members SET verdict='matched'; UPDATE migration_parity_sources SET candidate_complete=FALSE`)
	require.NoError(t, err)
	report, err = store.ReadParityRequestReport(t.Context(), parityTestRequest().RunID, 1)
	require.NoError(t, err)
	assert.False(t, report.Complete, "a source without complete candidate evidence remains pending")
	assert.False(t, report.Passing)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_sources SET candidate_complete=TRUE; DELETE FROM migration_parity_members`)
	require.NoError(t, err)
	report, err = store.ReadParityRequestReport(t.Context(), parityTestRequest().RunID, 1)
	require.NoError(t, err)
	assert.False(t, report.Complete)
	assert.False(t, report.Passing)

	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET request_generation=2`)
	require.NoError(t, err)
	report, err = store.ReadParityRequestReport(t.Context(), parityTestRequest().RunID, 2)
	require.NoError(t, err)
	assert.False(t, report.Complete)
	assert.False(t, report.Passing)
	assert.Equal(t, "pending", report.Code)
}

func TestMigrationParityRestrictedBaselinePrivileges(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	seedParityPhysicalMember(t, f)
	reader := newParityReadRole(t, f)
	var identity string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&identity))
	require.NoError(t, checkMigrationParityBaseline(t.Context(), reader, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant}, identity))
	require.ErrorContains(t, checkMigrationParityBaseline(t.Context(), reader, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant}, "00000000-0000-4000-8000-000000000099"), "identity")
	tx, err := reader.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	_, err = readParityPhysicalMember(t.Context(), tx, parityPhysicalBinding(f.tenant), "physical", rawderive.ParityMemberKey{SourceID: "source-a", LogicalKey: "logical-a", Kind: "session"})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	_, err = NewMigrationParityStore(t.Context(), reader, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant})
	require.ErrorContains(t, err, "privileges")

	var role string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&role))
	_, err = f.admin.ExecContext(t.Context(), `GRANT UPDATE ON "`+f.schema+`".messages TO "`+role+`"`)
	require.NoError(t, err)
	require.ErrorContains(t, checkMigrationParityBaseline(t.Context(), reader, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant}, identity), "unsafe")
	_, err = f.admin.ExecContext(t.Context(), `REVOKE UPDATE ON "`+f.schema+`".messages FROM "`+role+`"; GRANT UPDATE(content) ON "`+f.schema+`".messages TO "`+role+`"`)
	require.NoError(t, err)
	require.ErrorContains(t, checkMigrationParityBaseline(t.Context(), reader, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant}, identity), "unsafe")
}

func TestMigrationParityBaselineIdentityQueryErrorIsPreserved(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	reader := newParityReadRole(t, f)
	var identity string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&identity))
	var role string
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&role))
	_, err := f.admin.ExecContext(t.Context(), `REVOKE SELECT ON "`+f.schema+`".migration_parity_identity FROM "`+role+`"`)
	require.NoError(t, err)
	err = checkMigrationParityBaseline(t.Context(), reader, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant}, identity)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
	assert.NotContains(t, err.Error(), "baseline_unprovisioned")
}

func TestMigrationParityRestrictedBaselineRejectsExcludedCorpusWrites(t *testing.T) {
	for _, test := range []struct {
		name, grant string
		public      bool
	}{
		{"table update", "UPDATE", false},
		{"column update", "UPDATE(display_name)", false},
		{"public update", "UPDATE", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedFixture(t, "tenant-a")
			reader := newParityReadRole(t, f)
			var identity, role string
			require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&identity))
			require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&role))
			recipient := `"` + role + `"`
			if test.public {
				recipient = "PUBLIC"
			}
			_, err := f.admin.ExecContext(t.Context(), `GRANT `+test.grant+` ON "`+f.schema+`".raw_devices TO `+recipient)
			require.NoError(t, err)
			require.ErrorContains(t, checkMigrationParityBaseline(t.Context(), reader, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant}, identity), "unsafe")
		})
	}
}

func TestMigrationParityDeleteUnsealedEpochOnly(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant})
	require.NoError(t, err)
	_, err = store.CreateOrResumeParity(t.Context(), parityTestRequest(), 1)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_receipt,head_generation,dependency_digest,code)
		VALUES($1,1,'source-a','raw','device-a','claude','root-a','manifest-a','receipt-a',1,decode(repeat('01',32),'hex'),'pending')`, parityTestRequest().RunID)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, deleteUnsealedParityEpoch(t.Context(), tx, parityTestRequest().RunID, 1))
	require.NoError(t, tx.Commit())
	var sources int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources`).Scan(&sources))
	assert.Zero(t, sources)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET baseline_sealed=TRUE WHERE run_id=$1`, parityTestRequest().RunID)
	require.NoError(t, err)
	tx, err = f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.ErrorContains(t, deleteUnsealedParityEpoch(t.Context(), tx, parityTestRequest().RunID, 1), "sealed")
	require.NoError(t, tx.Rollback())
}

func TestMigrationParityConstructorRejectsCatalogDamageBeforeWork(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, f hostedFixture)
	}{
		{"tenant literal", func(t *testing.T, f hostedFixture) {
			_, err := f.admin.ExecContext(t.Context(), `ALTER TABLE "`+f.schema+`".migration_parity_runs DROP CONSTRAINT hosted_tenant_check, ADD CONSTRAINT hosted_tenant_check CHECK(tenant_id IN ('tenant-a','other'))`)
			require.NoError(t, err)
		}},
		{"policy", func(t *testing.T, f hostedFixture) {
			_, err := f.admin.ExecContext(t.Context(), `DROP POLICY hosted_tenant_policy ON "`+f.schema+`".migration_parity_runs; CREATE POLICY hosted_tenant_policy ON "`+f.schema+`".migration_parity_runs USING(true) WITH CHECK(true)`)
			require.NoError(t, err)
		}},
		{"foreign key", func(t *testing.T, f hostedFixture) {
			var constraint string
			require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT conname FROM pg_constraint WHERE conrelid=to_regclass(format('%I.migration_parity_sources',$1::text)) AND contype='f' LIMIT 1`, f.schema).Scan(&constraint))
			_, err := f.admin.ExecContext(t.Context(), `ALTER TABLE "`+f.schema+`".migration_parity_sources DROP CONSTRAINT "`+constraint+`"`)
			require.NoError(t, err)
		}},
		{"index", func(t *testing.T, f hostedFixture) {
			_, err := f.admin.ExecContext(t.Context(), `DROP INDEX "`+f.schema+`".migration_parity_sources_pending`)
			require.NoError(t, err)
		}},
		{"identity trigger", func(t *testing.T, f hostedFixture) {
			_, err := f.admin.ExecContext(t.Context(), `DROP TRIGGER migration_parity_identity_immutable ON "`+f.schema+`".migration_parity_identity`)
			require.NoError(t, err)
		}},
		{"identity trigger conditional bypass", func(t *testing.T, f hostedFixture) {
			_, err := f.admin.ExecContext(t.Context(), `DROP TRIGGER migration_parity_identity_immutable ON "`+f.schema+`".migration_parity_identity;
				CREATE TRIGGER migration_parity_identity_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON "`+f.schema+`".migration_parity_identity FOR EACH STATEMENT WHEN (false) EXECUTE FUNCTION "`+f.schema+`".migration_parity_reject_identity_mutation()`)
			require.NoError(t, err)
		}},
		{"identity function early return", func(t *testing.T, f hostedFixture) {
			_, err := f.admin.ExecContext(t.Context(), `CREATE OR REPLACE FUNCTION "`+f.schema+`".migration_parity_reject_identity_mutation() RETURNS trigger LANGUAGE plpgsql AS $identity$ BEGIN RETURN NULL; RAISE EXCEPTION 'migration parity identity is immutable' USING ERRCODE='55000'; END; $identity$`)
			require.NoError(t, err)
		}},
		{"named check definition", func(t *testing.T, f hostedFixture) {
			_, err := f.admin.ExecContext(t.Context(), `ALTER TABLE "`+f.schema+`".migration_parity_runs DROP CONSTRAINT migration_parity_runs_code_check, ADD CONSTRAINT migration_parity_runs_code_check CHECK(last_code IS NOT NULL)`)
			require.NoError(t, err)
		}},
		{"privilege", func(t *testing.T, f hostedFixture) {
			_, err := f.admin.ExecContext(t.Context(), `REVOKE UPDATE ON "`+f.schema+`".migration_parity_runs FROM "`+f.role+`"`)
			require.NoError(t, err)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedFixture(t, "tenant-a")
			test.damage(t, f)
			_, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{Schema: f.schema, Tenant: f.tenant})
			require.Error(t, err)
			var runs int
			require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM "`+f.schema+`".migration_parity_runs`).Scan(&runs))
			assert.Zero(t, runs)
		})
	}
}
