//go:build pgtest

package postgres

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"hegel.dev/go/hegel"
)

func TestRawOutcomeFailureSurvivesReplay(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "failure", "")
	lease := f.lease(t, m)
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), lease, "parse", "private diagnostic"))
	// Model the replay transition independently of PR 1's command.
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET state='ready',attempt_count=0 WHERE id=$1`, lease.ID)
	require.NoError(t, err)
	var count int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count FROM raw_job_outcome_buckets WHERE stage='parse' AND outcome='failed'`).Scan(&count))
	assert.Equal(t, int64(1), count)
	require.Error(t, f.jobs.FailRawParseJob(t.Context(), lease, "parse", "stale"))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count FROM raw_job_outcome_buckets WHERE outcome='failed'`).Scan(&count))
	assert.Equal(t, int64(1), count)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_job_outcome_buckets(bucket_start,stage,outcome,count) VALUES(now()-interval '24 hours','parse','complete',99)`)
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "new-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.jobs.CompleteRawParseJob(t.Context(), leases[0]))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT sum(count) FROM raw_job_outcome_buckets`).Scan(&count))
	assert.Equal(t, int64(2), count)
}

// State changes count once, while replay, claims and repeated writes cannot
// erase or duplicate historical outcomes. Each generated sequence rolls back.
func TestRawOutcomeBucketsProperty(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "property", "")
	lease := f.lease(t, m)
	hegel.Test(t, func(ht *hegel.T) {
		stage := hegel.Draw(ht, hegel.SampledFrom([]string{"parse", "derive"}))
		states := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{"ready", "leased", "retrying", "complete", "failed", "superseded"})))
		tx, err := f.runtime.BeginTx(t.Context(), nil)
		require.NoError(ht, err)
		defer func() { require.NoError(ht, tx.Rollback()) }()
		_, err = tx.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET stage=$1 WHERE id=$2`, stage, lease.ID)
		require.NoError(ht, err)
		previous := "leased"
		want := map[string]int64{}
		for _, state := range states {
			_, err = tx.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET state=$1 WHERE id=$2`, state, lease.ID)
			require.NoError(ht, err)
			if state != previous && (state == "retrying" || state == "complete" || state == "failed" || state == "superseded") {
				want[state]++
			}
			previous = state
		}
		rows, err := tx.QueryContext(t.Context(), `SELECT outcome,sum(count) FROM raw_job_outcome_buckets WHERE stage=$1 GROUP BY outcome`, stage)
		require.NoError(ht, err)
		got := map[string]int64{}
		for rows.Next() {
			var state string
			var count int64
			require.NoError(ht, rows.Scan(&state, &count))
			got[state] = count
		}
		require.NoError(ht, rows.Err())
		require.NoError(ht, rows.Close())
		assert.Equal(ht, want, got)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

// Retention is measured in UTC hours even when the runtime uses another zone.
func TestRawOutcomeRetentionProperty(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "retention", "")
	lease := f.lease(t, m)
	hegel.Test(t, func(ht *hegel.T) {
		stage := hegel.Draw(ht, hegel.SampledFrom([]string{"parse", "derive"}))
		transitionStage := hegel.Draw(ht, hegel.SampledFrom([]string{"parse", "derive"}))
		zone := hegel.Draw(ht, hegel.SampledFrom([]string{"UTC", "Asia/Kathmandu", "America/New_York"}))
		// Bound timestamp materialization to one year; draw the full age domain.
		age := min(hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64)), 8760)
		tx, err := f.runtime.BeginTx(t.Context(), nil)
		require.NoError(ht, err)
		defer func() { require.NoError(ht, tx.Rollback()) }()
		_, err = tx.ExecContext(t.Context(), `SELECT set_config('TimeZone',$1,true)`, zone)
		require.NoError(ht, err)
		var now time.Time
		require.NoError(ht, tx.QueryRowContext(t.Context(), `SELECT CURRENT_TIMESTAMP`).Scan(&now))
		hour := now.UTC().Truncate(time.Hour)
		ages := []int64{0, 23, 24, 25, age}
		for _, hours := range ages {
			_, err = tx.ExecContext(t.Context(), `INSERT INTO raw_job_outcome_buckets(bucket_start,stage,outcome,count) VALUES($1,$2,'failed',1) ON CONFLICT DO NOTHING`, hour.Add(-time.Duration(hours)*time.Hour), stage)
			require.NoError(ht, err)
		}
		_, err = tx.ExecContext(t.Context(), `INSERT INTO raw_job_outcome_buckets(bucket_start,stage,outcome,count) VALUES($1,$2,'complete',4)`, hour, transitionStage)
		require.NoError(ht, err)
		_, err = tx.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET stage=$1,state='complete' WHERE id=$2`, transitionStage, lease.ID)
		require.NoError(ht, err)
		var bucket time.Time
		require.NoError(ht, tx.QueryRowContext(t.Context(), `SELECT bucket_start FROM raw_job_outcome_buckets WHERE outcome='complete' AND count<>4 AND stage=$1`, transitionStage).Scan(&bucket))
		assert.Equal(ht, bucket.UTC().Truncate(time.Hour), bucket.UTC())
		for _, hours := range ages {
			seeded := hour.Add(-time.Duration(hours) * time.Hour)
			var exists bool
			require.NoError(ht, tx.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM raw_job_outcome_buckets WHERE bucket_start=$1 AND stage=$2 AND outcome='failed')`, seeded, stage).Scan(&exists))
			assert.Equal(ht, !seeded.Before(bucket.Add(-23*time.Hour)), exists)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestRawOutcomeTriggerRequiredAtStartup(t *testing.T) {
	f := newProjectionFixture(t)
	_, err := f.admin.ExecContext(t.Context(), `ALTER TABLE raw_ingest_jobs DISABLE TRIGGER raw_job_outcome`)
	require.NoError(t, err)
	_, err = NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant})
	require.ErrorContains(t, err, "outcome protection is missing or altered")
}

func TestRawOutcomePrivilegesRequiredAtStartup(t *testing.T) {
	f := newProjectionFixture(t)
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		_, err := f.admin.ExecContext(t.Context(), `REVOKE `+privilege+` ON raw_job_outcome_buckets FROM "`+f.role+`"`)
		require.NoError(t, err)
		require.ErrorContains(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull), "raw_job_outcome_buckets", privilege)
		_, err = f.admin.ExecContext(t.Context(), `GRANT `+privilege+` ON raw_job_outcome_buckets TO "`+f.role+`"`)
		require.NoError(t, err)
		require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	}
}
