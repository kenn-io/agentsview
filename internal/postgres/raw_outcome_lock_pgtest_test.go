//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Immediate outcome pruning lets selection hold an expired bucket while waiting
// for projection's corpus row, then makes projection wait for that bucket.
func TestRawOutcomeSelectionAndProjectionCommitWithoutLockInversion(t *testing.T) {
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "project", "")
	_, err := f.sink.SelectSourceGeneration(t.Context(), a, "parser-1")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "outcome-worker", 1, 5*time.Minute, 5)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, a.ManifestID, leases[0].ManifestID)
	b, _ := f.accept(t, "device-b", "select", "")
	_, err = f.sink.SelectSourceGeneration(t.Context(), b, "parser-1")
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_job_outcome_buckets(bucket_start,stage,outcome,count) VALUES(date_trunc('hour',clock_timestamp(),'UTC')-interval '24 hours','parse','retrying',99)`)
	require.NoError(t, err)
	// AFTER UPDATE parks publication with the corpus row already locked.
	_, err = f.admin.ExecContext(t.Context(), `CREATE FUNCTION park_outcome_corpus() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.corpus_revision>OLD.corpus_revision THEN PERFORM pg_advisory_xact_lock(hashtext(TG_TABLE_SCHEMA)); END IF; RETURN NEW; END $$;
 CREATE TRIGGER park_outcome_corpus AFTER UPDATE ON raw_corpus_state FOR EACH ROW EXECUTE FUNCTION park_outcome_corpus()`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	gate, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = gate.Rollback() }()
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, f.schema)
	require.NoError(t, err)
	projected := make(chan error, 1)
	go func() { projected <- f.sink.Project(ctx, leases[0], a, projectionOutcome("published")) }()
	var publisher int
	require.Eventually(t, func() bool {
		err := f.admin.QueryRowContext(ctx, `SELECT coalesce(max(pid),0) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND wait_event='advisory'`, f.role).Scan(&publisher)
		return err == nil && publisher != 0
	}, 30*time.Second, time.Second)
	selected := make(chan error, 1)
	go func() {
		_, err := f.sink.SelectSourceGeneration(ctx, b, "parser-2")
		selected <- err
	}()
	// On both implementations selection now waits for this publication's corpus
	// row. The immediate trigger has additionally locked the expired bucket.
	require.Eventually(t, func() bool {
		var waiting bool
		err := f.admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename=$1 AND pid<>$2 AND $2=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock')`, f.role, publisher).Scan(&waiting)
		return err == nil && waiting
	}, 30*time.Second, time.Second)
	require.NoError(t, gate.Commit())
	assert.NoError(t, <-projected, "projection and its outcome must commit")
	assert.NoError(t, <-selected, "selection and its outcome must commit")
	var completed, superseded, expired int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT coalesce(sum(count) FILTER(WHERE outcome='complete'),0),coalesce(sum(count) FILTER(WHERE outcome='superseded'),0),count(*) FILTER(WHERE bucket_start<date_trunc('hour',clock_timestamp(),'UTC')-interval '23 hours') FROM raw_job_outcome_buckets`).Scan(&completed, &superseded, &expired))
	assert.Equal(t, int64(1), completed)
	assert.Equal(t, int64(1), superseded)
	assert.Zero(t, expired)
	var state, version string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state FROM raw_ingest_jobs WHERE id=$1`, leases[0].ID).Scan(&state))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT processing_version FROM raw_source_projections WHERE source_id=$1`, rawSourceID(b)).Scan(&version))
	assert.Equal(t, "complete", state)
	assert.Equal(t, "parser-2", version)
}
