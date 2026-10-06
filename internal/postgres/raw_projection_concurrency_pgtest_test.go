//go:build pgtest

package postgres

import (
	"context"
	"errors"
	"fmt"
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

// Manifest acceptance and every other projection update the corpus revision
// row, so a projection must not hold it while it writes one group's rows.
func TestRawProjectionRowWritesDoNotHoldCorpusRevisionAgainstOtherSources(t *testing.T) {
	f := newProjectionFixture(t)
	slow, _ := f.accept(t, "device-a", "slow-a", "")
	lease := f.lease(t, slow)
	other, _ := f.accept(t, "device-b", "other-b", "")
	_, err := f.admin.ExecContext(t.Context(), `CREATE FUNCTION hold_projection_message() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(hashtext(TG_TABLE_SCHEMA)); RETURN NEW; END $$; CREATE TRIGGER hold_projection_message BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION hold_projection_message()`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, f.schema)
	require.NoError(t, err)
	projected := make(chan error, 1)
	go func() { projected <- f.sink.Project(ctx, lease, slow, projectionOutcome("slow")) }()
	require.Eventually(t, func() bool {
		var writing int
		err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND wait_event='advisory'`, f.role).Scan(&writing)
		return err == nil && writing == 1
	}, 3*time.Second, 10*time.Millisecond)

	selectCtx, cancelSelect := context.WithTimeout(ctx, 3*time.Second)
	defer cancelSelect()
	_, err = f.sink.SelectSourceGeneration(selectCtx, other, "parser-1")

	require.NoError(t, err, "selecting another source waited for a projection that was still writing rows")
	require.NoError(t, gate.Commit())
	require.NoError(t, <-projected)
	var selection, corpus int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT selection_revision,corpus_revision FROM raw_corpus_state WHERE singleton=1`).Scan(&selection, &corpus))
	assert.Equal(t, int64(2), selection)
	assert.Equal(t, int64(1), corpus)
	var queuedSelection, queuedCorpus int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT selection_revision,corpus_revision FROM raw_embedding_outbox`).Scan(&queuedSelection, &queuedCorpus))
	assert.Equal(t, selection, queuedSelection, "embedding work must carry the selection revision its projection committed with")
	assert.Equal(t, corpus, queuedCorpus)
}

// Excluding a trashed session removes its physical rows; other sources must
// stay selectable while those rows are being removed.
func TestRawExclusionRowRemovalDoesNotHoldCorpusRevisionAgainstOtherSources(t *testing.T) {
	tests := []struct {
		name    string
		exclude func(context.Context, *RawProjectionStore) error
	}{
		{name: "one trashed session", exclude: func(ctx context.Context, s *RawProjectionStore) error {
			removed, err := s.ExcludeTrashedSession(ctx, "codex:portable")
			if err == nil && !removed {
				return errors.New("trashed session was not excluded")
			}
			return err
		}},
		{name: "empty trash", exclude: func(ctx context.Context, s *RawProjectionStore) error {
			removed, err := s.EmptyTrash(ctx)
			if err == nil && removed != 1 {
				return fmt.Errorf("emptied %d sessions, want 1", removed)
			}
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newProjectionFixture(t)
			trashed, _ := f.accept(t, "device-a", "trashed-a", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, trashed), trashed, projectionOutcome("trashed")))
			require.NoError(t, f.sink.SetCuration(t.Context(), "codex:portable", "trashed", true))
			other, _ := f.accept(t, "device-b", "other-b", "")
			_, err := f.admin.ExecContext(t.Context(), `CREATE FUNCTION hold_session_removal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(hashtext(TG_TABLE_SCHEMA)); RETURN OLD; END $$; CREATE TRIGGER hold_session_removal BEFORE DELETE ON sessions FOR EACH ROW EXECUTE FUNCTION hold_session_removal()`)
			require.NoError(t, err)
			var before struct{ identity, corpus int64 }
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT identity_revision,corpus_revision FROM raw_corpus_state WHERE singleton=1`).Scan(&before.identity, &before.corpus))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			gate, err := f.admin.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer gate.Rollback()
			_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, f.schema)
			require.NoError(t, err)
			excluded := make(chan error, 1)
			go func() { excluded <- tt.exclude(ctx, f.sink) }()
			require.Eventually(t, func() bool {
				var removing int
				err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND wait_event='advisory'`, f.role).Scan(&removing)
				return err == nil && removing == 1
			}, 3*time.Second, 10*time.Millisecond)

			selectCtx, cancelSelect := context.WithTimeout(ctx, 3*time.Second)
			defer cancelSelect()
			_, err = f.sink.SelectSourceGeneration(selectCtx, other, "parser-1")

			require.NoError(t, err, "selecting another source waited for an exclusion that was still removing rows")
			require.NoError(t, gate.Commit())
			require.NoError(t, <-excluded)
			var identity, corpus int64
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT identity_revision,corpus_revision FROM raw_corpus_state WHERE singleton=1`).Scan(&identity, &corpus))
			assert.Equal(t, before.identity+1, identity)
			assert.Equal(t, before.corpus+1, corpus)
			var sessions int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&sessions))
			assert.Zero(t, sessions)
		})
	}
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
