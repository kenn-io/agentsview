//go:build pgtest

package postgres

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
	"testing"
	"time"
)

func TestRawProjectionCurationResolvesMembershipAfterConcurrentSplit(t *testing.T) {
	f := newProjectionFixture(t)
	a, ar := f.accept(t, "device-a", "race-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, projectionOutcome("equal")))
	b, _ := f.accept(t, "device-b", "race-b", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, projectionOutcome("equal")))
	aa := f.alias(t, a)
	ba := f.alias(t, b)
	resolved, err := f.sink.Resolve(t.Context(), aa)
	require.NoError(t, err)
	next, _ := f.accept(t, "device-a", "split-a", ar.Receipt)
	lease := f.lease(t, next)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate, err := f.runtime.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT group_id FROM raw_session_groups WHERE group_id=$1 FOR UPDATE`, resolved.GroupID)
	require.NoError(t, err)
	projected := make(chan error, 1)
	go func() { projected <- f.sink.Project(ctx, lease, next, projectionOutcome("split")) }()
	waiting := func(count int) func() bool {
		return func() bool {
			var got int
			err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock'`, f.role).Scan(&got)
			return err == nil && got >= count
		}
	}
	require.Eventually(t, waiting(1), 3*time.Second, 10*time.Millisecond)
	curated := make(chan error, 1)
	go func() { curated <- f.sink.SetCuration(ctx, aa, "starred", true) }()
	require.Eventually(t, waiting(2), 3*time.Second, 10*time.Millisecond)
	require.NoError(t, gate.Commit())
	require.NoError(t, <-projected)
	require.NoError(t, <-curated)
	ra, err := f.sink.Resolve(t.Context(), aa)
	require.NoError(t, err)
	rb, err := f.sink.Resolve(t.Context(), ba)
	require.NoError(t, err)
	assert.NotEqual(t, ra.SessionID, rb.SessionID)
	stars, err := (&Store{pg: f.runtime}).ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{ra.SessionID}, stars)
}
func TestRawProjectionCurationSQLFailureRollsBackOverlayAndMaterialization(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "curation-failure", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("hello")))
	before, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	_, err = f.admin.ExecContext(t.Context(), `CREATE FUNCTION reject_star_materialization() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic star dependency unavailable'; END $$; CREATE TRIGGER reject_star_materialization BEFORE INSERT ON starred_sessions FOR EACH ROW EXECUTE FUNCTION reject_star_materialization()`)
	require.NoError(t, err)
	err = f.sink.SetCuration(t.Context(), "codex:portable", "starred", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "synthetic star dependency unavailable")
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_curation`).Scan(&count))
	assert.Zero(t, count)
	after, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, before.CorpusRevision, after.CorpusRevision)
	stars, err := (&Store{pg: f.runtime}).ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, stars)
}

func TestRawProjectionRollsBackWhenLeaseExpiresDuringWrites(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "expires-during-write", "")
	_, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-1")
	require.NoError(t, err)
	_, err = f.admin.ExecContext(t.Context(), `CREATE FUNCTION delay_projection_message() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.ordinal=0 THEN IF NOT EXISTS(SELECT 1 FROM raw_ingest_jobs WHERE state='leased' AND lease_expires_at>clock_timestamp()) THEN RAISE EXCEPTION 'fixture lease expired before writes'; END IF; PERFORM pg_sleep(0.4); END IF; RETURN NEW; END $$; CREATE TRIGGER delay_projection_message BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION delay_projection_message()`)
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "expiry-worker", 1, 250*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.ErrorIs(t, f.sink.Project(t.Context(), leases[0], m, projectionOutcome("expired write")), rawderive.ErrLeaseLost)
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_source_contributions`).Scan(&count))
	assert.Zero(t, count)
}
