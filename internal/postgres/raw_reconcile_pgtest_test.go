//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/rawderive"
	"hegel.dev/go/hegel"
)

// Unique highest-generation evidence chooses the durable version regardless of
// insertion order. Ties and empty histories must leave all authoritative rows alone.
func TestRawReconcileHistoryProperty(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "history", "")
	hegel.Test(t, func(ht *hegel.T) {
		history := hegel.Draw(ht, hegel.Lists(hegel.Integers[int64](0, math.MaxInt64)))
		history = history[:min(len(history), 8)] // Bound SQL writes at materialization, not the domain.
		failed := hegel.Draw(ht, hegel.Booleans())
		_, err := f.runtime.ExecContext(t.Context(), `DELETE FROM raw_projection_generations;DELETE FROM raw_source_projections;DELETE FROM raw_ingest_jobs;DELETE FROM raw_maintenance_state`)
		require.NoError(ht, err)
		var maximum int64
		var winners []int
		for i, g := range history {
			if len(winners) == 0 || g > maximum {
				maximum = g
				winners = []int{i}
			} else if g == maximum {
				winners = append(winners, i)
			}
			_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_ingest_jobs(tenant_id,manifest_id,stage,processing_version,projection_generation,lease_epoch,attempt_count,state,failed_at) VALUES($1,$2,'parse',$3,$4,17,3,$5,CASE WHEN $5='failed' THEN '2026-01-01'::timestamptz END)`, f.tenant, m.ManifestID, fmt.Sprintf("durable-%d", i), g, map[bool]string{true: "failed", false: "ready"}[failed])
			require.NoError(ht, err)
		}
		before := rawAuthoritativeSnapshot(t, f)
		got, err := f.sink.ReconcileRawSources(t.Context(), true)
		if len(winners) == 1 && maximum == math.MaxInt64 {
			require.NoError(ht, err)
			require.Equal(ht, 1, got.Unrecoverable) // No representable generation above this fence.
			require.Equal(ht, before, rawAuthoritativeSnapshot(t, f))
			return
		}
		require.NoError(ht, err)
		if len(winners) != 1 {
			require.Equal(ht, 1, got.Unrecoverable)
			require.Equal(ht, before, rawAuthoritativeSnapshot(t, f))
			return
		}
		require.Equal(ht, 1, got.Repaired)
		var version, state string
		var generation, epoch int64
		var attempt int
		var failedAt *time.Time
		require.NoError(ht, f.runtime.QueryRowContext(t.Context(), `SELECT p.processing_version,p.projection_generation,j.state,j.lease_epoch,j.attempt_count,j.failed_at FROM raw_source_projections p JOIN raw_ingest_jobs j ON j.tenant_id=p.tenant_id AND j.id=p.selected_job_id WHERE p.source_id=$1`, rawSourceID(m)).Scan(&version, &generation, &state, &epoch, &attempt, &failedAt))
		require.Equal(ht, fmt.Sprintf("durable-%d", winners[0]), version)
		require.Equal(ht, maximum+1, generation)
		if failed {
			require.Equal(ht, "failed", state)
			require.Equal(ht, 3, attempt)
			require.Equal(ht, int64(17), epoch)
			require.NotNil(ht, failedAt)
			require.Equal(ht, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), failedAt.UTC())
		} else {
			require.Equal(ht, "ready", state)
			require.Zero(ht, attempt)
			require.Equal(ht, int64(18), epoch)
			require.Nil(ht, failedAt)
		}
		before = rawAuthoritativeSnapshot(t, f)
		again, err := f.sink.ReconcileRawSources(t.Context(), true)
		require.NoError(ht, err)
		require.Zero(ht, again.Repaired)
		require.Equal(ht, before, rawAuthoritativeSnapshot(t, f))
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestRawReconcileTupleDamageAndStartup(t *testing.T) {
	for _, damage := range []string{"healthy", "version", "manifest", "job", "generation", "selected", "missing-projection", "missing-job-owner-recovery"} {
		t.Run(damage, func(t *testing.T) {
			f := newProjectionFixture(t)
			m, receipt := f.accept(t, "device", "first", "")
			other, _ := f.accept(t, "device", "second", receipt.Receipt)
			_, err := f.sink.SelectSourceGeneration(t.Context(), other, "durable")
			require.NoError(t, err)
			var query string
			switch damage {
			case "version":
				query = `UPDATE raw_source_projections SET processing_version='stored-version'`
			case "manifest":
				query = fmt.Sprintf(`UPDATE raw_source_projections SET selected_manifest_id='%s'`, m.ManifestID)
			case "job":
				query = fmt.Sprintf(`UPDATE raw_source_projections SET selected_job_id=(SELECT id FROM raw_ingest_jobs WHERE manifest_id='%s' LIMIT 1)`, m.ManifestID)
			case "generation":
				query = `UPDATE raw_source_projections SET projection_generation=0`
			case "selected":
				query = `UPDATE raw_ingest_jobs SET projection_selected=false`
			case "missing-projection":
				query = `DELETE FROM raw_projection_generations;DELETE FROM raw_source_projections`
			case "missing-job-owner-recovery":
				// An invalid catalog is not a supported CLI startup. The owner must restore
				// its FK protection first; an already-open primitive alone is not evidence.
				_, err = f.admin.ExecContext(t.Context(), `ALTER TABLE raw_source_projections DROP CONSTRAINT hosted_fk_2`)
				require.NoError(t, err)
				_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM raw_ingest_jobs`)
				require.NoError(t, err)
				require.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
				// Preserve the version evidence while removing the dangling reference.
				_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_ingest_jobs(tenant_id,manifest_id,stage,processing_version) VALUES($1,$2,'parse','durable')`, f.tenant, other.ManifestID)
				require.NoError(t, err)
				_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_source_projections SET selected_job_id=(SELECT id FROM raw_ingest_jobs WHERE manifest_id=$1)`, other.ManifestID)
				require.NoError(t, err)
				require.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
				_, err = f.admin.ExecContext(t.Context(), `ALTER TABLE raw_source_projections ADD CONSTRAINT hosted_fk_2 FOREIGN KEY (tenant_id,selected_job_id) REFERENCES raw_ingest_jobs(tenant_id,id) ON DELETE RESTRICT ON UPDATE NO ACTION NOT VALID`)
				require.NoError(t, err)
				require.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
				_, err = f.admin.ExecContext(t.Context(), `ALTER TABLE raw_source_projections VALIDATE CONSTRAINT hosted_fk_2`)
				require.NoError(t, err)
				// Owner recovery must restore a validated constraint before provisioning.
				var restored bool
				require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='raw_source_projections'::regclass AND conname='hosted_fk_2' AND convalidated)`).Scan(&restored))
				require.True(t, restored)
				require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
			}
			if query != "" {
				_, err = f.runtime.ExecContext(t.Context(), query)
				require.NoError(t, err)
			}
			// Catalog protections are intact on a freshly opened runtime connection.
			runtime, err := OpenHosted(f.dsn, f.schema, f.tenant, false)
			require.NoError(t, err)
			defer runtime.Close()
			sink, err := NewRawProjectionStore(runtime, RawProjectionOptions{Tenant: f.tenant})
			require.NoError(t, err)
			before := rawAuthoritativeSnapshot(t, f)
			got, err := sink.ReconcileRawSources(t.Context(), true)
			require.NoError(t, err)
			if damage == "healthy" {
				require.Zero(t, got.Repaired)
				require.Equal(t, before, rawAuthoritativeSnapshot(t, f))
			} else {
				require.Equal(t, 1, got.Repaired)
			}
			before = rawAuthoritativeSnapshot(t, f)
			again, err := sink.ReconcileRawSources(t.Context(), true)
			require.NoError(t, err)
			require.Zero(t, again.Repaired)
			require.Equal(t, before, rawAuthoritativeSnapshot(t, f))
		})
	}
}

func TestRawReconcileMakesMissingProjectionClaimable(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "claimable", "")
	old := f.lease(t, m)
	_, err := f.runtime.ExecContext(t.Context(), `DELETE FROM raw_projection_generations;DELETE FROM raw_source_projections;UPDATE raw_ingest_jobs SET state='ready',lease_owner='',lease_expires_at=NULL`)
	require.NoError(t, err)
	hosted, err := NewHostedRawIngestStore(f.runtime, f.tenant, "parser-1")
	require.NoError(t, err)
	leases, err := hosted.ClaimRawParseJobs(t.Context(), "before", 1, time.Minute, 5)
	require.NoError(t, err)
	require.Empty(t, leases)
	before := rawMaintenanceSnapshot(t, f)
	dry, err := f.sink.ReconcileRawSources(t.Context(), false)
	require.NoError(t, err)
	require.Equal(t, 1, dry.Found)
	require.Equal(t, before, rawMaintenanceSnapshot(t, f))
	repaired, err := f.sink.ReconcileRawSources(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, 1, repaired.Repaired)
	wrong, err := NewHostedRawIngestStore(f.runtime, f.tenant, "another-version")
	require.NoError(t, err)
	leases, err = wrong.ClaimRawParseJobs(t.Context(), "wrong", 1, time.Minute, 5)
	require.NoError(t, err)
	require.Empty(t, leases)
	leases, err = hosted.ClaimRawParseJobs(t.Context(), old.Owner, 1, time.Minute, 5)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Greater(t, leases[0].LeaseEpoch, old.LeaseEpoch)
	require.Equal(t, old.ID, leases[0].ID)
	require.Equal(t, old.Attempt, leases[0].Attempt)
	before = rawAuthoritativeSnapshot(t, f)
	require.ErrorIs(t, hosted.HeartbeatRawParseJob(t.Context(), old, time.Minute), rawderive.ErrLeaseLost)
	require.ErrorIs(t, hosted.CompleteRawParseJob(t.Context(), old), rawderive.ErrLeaseLost)
	require.Equal(t, before, rawAuthoritativeSnapshot(t, f))
}

// Snapshot complete authoritative rows, preserving dead-letter timestamps and
// corpus revisions. Only the separate maintenance table has volatile run times.
func rawAuthoritativeSnapshot(t *testing.T, f projectionFixture) string {
	t.Helper()
	tables := []string{"raw_ingest_jobs", "raw_source_projections", "raw_projection_generations", "raw_session_branches", "session_sources", "raw_source_contributions", "raw_corpus_state"}
	return rawTableSnapshot(t, f, tables)
}
func rawMaintenanceSnapshot(t *testing.T, f projectionFixture) string {
	t.Helper()
	return rawAuthoritativeSnapshot(t, f) + rawTableSnapshot(t, f, []string{"raw_maintenance_state"})
}
func rawTableSnapshot(t *testing.T, f projectionFixture, tables []string) string {
	t.Helper()
	var all []string
	for _, table := range tables {
		rows, err := f.runtime.QueryContext(t.Context(), `SELECT to_jsonb(r)::text FROM `+table+` r`)
		require.NoError(t, err)
		var records []string
		for rows.Next() {
			var row string
			require.NoError(t, rows.Scan(&row))
			records = append(records, row)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		slices.Sort(records)
		all = append(all, table, fmt.Sprint(records))
	}
	return fmt.Sprint(all)
}

func TestRawReconcileBoundedCursorCancellationAndLock(t *testing.T) {
	for _, count := range []int{1, rawReconcileHeadBatchSize * 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newProjectionFixture(t)
			var expected []string
			for i := 0; i < count; i++ {
				m, _ := f.accept(t, fmt.Sprintf("device-%04d", i), "batch", "")
				expected = append(expected, rawSourceID(m))
			}
			_, err := f.runtime.ExecContext(t.Context(), `DELETE FROM raw_projection_generations;DELETE FROM raw_source_projections`)
			require.NoError(t, err)
			tx, err := f.runtime.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('raw-reconcile:'||current_schema()||':'||$1,0))`, f.tenant)
			require.NoError(t, err)
			busy, err := f.sink.ReconcileRawSources(t.Context(), true)
			require.NoError(t, err)
			require.True(t, busy.Busy)
			require.NoError(t, tx.Rollback())
			var visited []string
			for {
				before := rawMaintenanceSnapshot(t, f)
				_, err = f.sink.ReconcileRawSources(t.Context(), false)
				require.NoError(t, err)
				require.Equal(t, before, rawMaintenanceSnapshot(t, f))
				got, err := f.sink.ReconcileRawSources(t.Context(), true)
				require.NoError(t, err)
				require.LessOrEqual(t, got.Scanned, rawReconcileHeadBatchSize)
				var encoded []byte
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT COALESCE(jsonb_agg(source_id ORDER BY source_id),'[]'::jsonb) FROM raw_source_projections`).Scan(&encoded))
				var ids []string
				require.NoError(t, json.Unmarshal(encoded, &ids))
				// Each damaged identity is repaired exactly once across the full sweep.
				var added []string
				for _, id := range ids {
					if !slices.Contains(visited, id) {
						added = append(added, id)
					}
				}
				require.Len(t, added, got.Scanned)
				require.Equal(t, got.Scanned, got.Repaired)
				visited = append(visited, added...)
				if got.Wrapped {
					break
				}
			}
			slices.Sort(expected)
			slices.Sort(visited)
			require.Equal(t, expected, visited)
		})
	}
}

func TestRawReconcileCancellationAfterMutation(t *testing.T) {
	f := newProjectionFixture(t)
	f.accept(t, "device", "cancel", "")
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET projection_selected=false;INSERT INTO raw_maintenance_state(check_name,found,repaired) VALUES('reconcile',7,9)`)
	require.NoError(t, err)
	before := rawMaintenanceSnapshot(t, f)
	// AFTER ledger INSERT is reached only after the job and selection writes.
	blocker, err := f.admin.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended(current_schema()||':cancel-fixture',0))`)
	require.NoError(t, err)
	_, err = f.admin.ExecContext(t.Context(), `CREATE FUNCTION cancel_repair_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(hashtextextended(current_schema()||':cancel-fixture',0)); RETURN NEW; END $$;CREATE TRIGGER cancel_repair_fixture AFTER INSERT ON raw_projection_generations FOR EACH ROW EXECUTE FUNCTION cancel_repair_fixture()`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.sink.ReconcileRawSources(ctx, true); done <- err }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := f.admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE a.usename=$1 AND l.locktype='advisory' AND NOT l.granted)`, f.role).Scan(&waiting)
		return err == nil && waiting
	}, time.Minute, 10*time.Millisecond)
	cancel()
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Minute):
		t.Fatal("canceled repair did not return")
	}
	require.Equal(t, before, rawMaintenanceSnapshot(t, f))
}

func TestRawMaintenanceUpgradeAndPrivileges(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.admin.ExecContext(t.Context(), `DROP TABLE raw_maintenance_state`)
	require.NoError(t, err)
	_, err = NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant})
	require.Error(t, err)
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	err = CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull)
	require.ErrorContains(t, err, "raw_maintenance_state")
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,UPDATE ON raw_maintenance_state TO "`+f.role+`"`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	_, err = NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant})
	require.NoError(t, err)
	var unsafe int
	require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname='raw_maintenance_state' AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity)`, f.schema).Scan(&unsafe))
	require.Zero(t, unsafe)
}
