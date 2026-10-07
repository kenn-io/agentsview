//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
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

// parkOn makes every matching row write wait on the fixture's advisory gate.
// condition is a PL/pgSQL boolean over NEW or OLD.
func (f projectionFixture) parkOn(t *testing.T, timing, table, condition string) {
	t.Helper()
	row := "NEW"
	if timing == "DELETE" {
		row = "OLD"
	}
	_, err := f.admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION park_%[2]s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %[3]s THEN PERFORM pg_advisory_xact_lock(hashtext(TG_TABLE_SCHEMA)); END IF; RETURN %[4]s; END $$; CREATE TRIGGER park_%[2]s BEFORE %[1]s ON %[2]s FOR EACH ROW EXECUTE FUNCTION park_%[2]s()`, timing, table, condition, row))
	require.NoError(t, err)
}

// whileParked runs write until a parkOn trigger holds it mid-materialization,
// runs during, and then releases write and requires both to succeed.
func (f projectionFixture) whileParked(t *testing.T, write, during func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, f.schema)
	require.NoError(t, err)
	written := make(chan error, 1)
	go func() { written <- write(ctx) }()
	require.Eventually(t, func() bool {
		var parked int
		err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND wait_event='advisory'`, f.role).Scan(&parked)
		return err == nil && parked == 1
	}, 3*time.Second, 10*time.Millisecond)

	duringCtx, cancelDuring := context.WithTimeout(ctx, 3*time.Second)
	defer cancelDuring()
	require.NoError(t, during(duringCtx), "another source waited for a writer that was still materializing rows")

	require.NoError(t, gate.Commit())
	require.NoError(t, <-written)
}

// requireSelectableWhileParked requires that another source can be selected
// while write is parked. Manifest acceptance and every publication update the
// corpus revision row, so no writer may hold it across row writes.
func (f projectionFixture) requireSelectableWhileParked(t *testing.T, other rawsync.CanonicalManifest, write func(context.Context) error) {
	t.Helper()
	f.whileParked(t, write, func(ctx context.Context) error {
		_, err := f.sink.SelectSourceGeneration(ctx, other, "parser-1")
		return err
	})
}

type corpusRevisions struct{ identity, selection, corpus int64 }

func (f projectionFixture) revisions(t *testing.T) corpusRevisions {
	t.Helper()
	var r corpusRevisions
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT identity_revision,selection_revision,corpus_revision FROM raw_corpus_state WHERE singleton=1`).Scan(&r.identity, &r.selection, &r.corpus))
	return r
}

type embeddingEvent struct {
	session, content, action string
	selection, corpus        int64
}

func (f projectionFixture) embeddingEvents(t *testing.T, action string) []embeddingEvent {
	t.Helper()
	rows, err := f.runtime.QueryContext(t.Context(), `SELECT session_id,content_revision,action,selection_revision,corpus_revision FROM raw_embedding_outbox WHERE action=$1 ORDER BY corpus_revision`, action)
	require.NoError(t, err)
	defer rows.Close()
	var events []embeddingEvent
	for rows.Next() {
		var e embeddingEvent
		require.NoError(t, rows.Scan(&e.session, &e.content, &e.action, &e.selection, &e.corpus))
		events = append(events, e)
	}
	require.NoError(t, rows.Err())
	return events
}

// projectSession publishes one single-session source under its own group.
func (f projectionFixture) projectSession(t *testing.T, device, session string) RawIdentity {
	t.Helper()
	m, _ := f.accept(t, device, "capture-"+session, "")
	outcome := projectionOutcome("content " + session)
	outcome.Outcome.Results[0].Result.Session.ID = "codex:" + session
	outcome.Outcome.Results[0].Result.Session.SourceSessionID = session
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	resolved, err := f.sink.Resolve(t.Context(), "codex:"+session)
	require.NoError(t, err)
	require.Equal(t, RawIdentityUnique, resolved.State)
	return resolved
}

func TestRawProjectionRowWritesDoNotHoldCorpusRevisionAgainstOtherSources(t *testing.T) {
	f := newProjectionFixture(t)
	slow, _ := f.accept(t, "device-a", "slow-a", "")
	lease := f.lease(t, slow)
	other, _ := f.accept(t, "device-b", "other-b", "")
	f.parkOn(t, "INSERT", "messages", "true")

	f.requireSelectableWhileParked(t, other, func(ctx context.Context) error {
		return f.sink.Project(ctx, lease, slow, projectionOutcome("slow"))
	})

	now := f.revisions(t)
	assert.Equal(t, int64(2), now.selection)
	assert.Equal(t, int64(1), now.corpus)
	events := f.embeddingEvents(t, "reconcile")
	require.Len(t, events, 1)
	assert.Equal(t, now.selection, events[0].selection, "embedding work must carry the selection revision its projection committed with")
	assert.Equal(t, now.corpus, events[0].corpus)
}

// Projections of different groups share no rows, so one can publish while
// another is still writing.
func TestRawProjectionsOfDifferentGroupsPublishIndependently(t *testing.T) {
	f := newProjectionFixture(t)
	slow, _ := f.accept(t, "device-a", "slow-a", "")
	slowLease := f.lease(t, slow)
	slowOutcome := projectionOutcome("parked")
	fast, _ := f.accept(t, "device-b", "fast-b", "")
	fastOutcome := projectionOutcome("fast")
	fastOutcome.Outcome.Results[0].Result.Session.ID = "codex:fast"
	fastOutcome.Outcome.Results[0].Result.Session.SourceSessionID = "fast"
	f.parkOn(t, "INSERT", "messages", "NEW.content = 'parked'")

	f.whileParked(t, func(ctx context.Context) error {
		return f.sink.Project(ctx, slowLease, slow, slowOutcome)
	}, func(ctx context.Context) error {
		_, err := f.sink.SelectSourceGeneration(ctx, fast, "parser-1")
		if err != nil {
			return err
		}
		leases, err := f.jobs.ClaimRawParseJobs(ctx, "second-worker", 1, time.Minute)
		if err != nil {
			return err
		}
		if len(leases) != 1 {
			return fmt.Errorf("claimed %d jobs, want 1", len(leases))
		}
		return f.sink.Project(ctx, leases[0], fast, fastOutcome)
	})

	slowIdentity, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	fastIdentity, err := f.sink.Resolve(t.Context(), "codex:fast")
	require.NoError(t, err)
	require.Equal(t, RawIdentityUnique, slowIdentity.State)
	require.Equal(t, RawIdentityUnique, fastIdentity.State)
	now := f.revisions(t)
	assert.Equal(t, int64(2), now.corpus)
	assert.Equal(t, []embeddingEvent{
		{session: fastIdentity.SessionID, content: fastIdentity.ContentRevision, action: "reconcile", selection: 2, corpus: 1},
		{session: slowIdentity.SessionID, content: slowIdentity.ContentRevision, action: "reconcile", selection: 2, corpus: 2},
	}, f.embeddingEvents(t, "reconcile"))
	var messages int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM messages`).Scan(&messages))
	assert.Equal(t, 4, messages)
}

func TestRawCurationRowWritesDoNotHoldCorpusRevisionAgainstOtherSources(t *testing.T) {
	f := newProjectionFixture(t)
	f.projectSession(t, "device-a", "starred")
	other, _ := f.accept(t, "device-b", "other-b", "")
	before := f.revisions(t)
	f.parkOn(t, "INSERT", "starred_sessions", "true")

	f.requireSelectableWhileParked(t, other, func(ctx context.Context) error {
		return f.sink.SetCuration(ctx, "codex:starred", "starred", true)
	})

	now := f.revisions(t)
	assert.Equal(t, before.corpus+1, now.corpus)
	assert.Equal(t, before.identity, now.identity)
	stars, err := (&Store{pg: f.runtime}).ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Len(t, stars, 1)
}

// Excluding trashed sessions removes their physical rows. Emptying the trash
// removes every selected group's rows before it publishes any of them.
func TestRawExclusionRowRemovalDoesNotHoldCorpusRevisionAgainstOtherSources(t *testing.T) {
	tests := []struct {
		name    string
		exclude func(context.Context, *RawProjectionStore) (int, error)
		// removed picks, in publication order, the sessions the operation
		// removes from the two trashed sessions ordered by group.
		removed func(first, second RawIdentity) []RawIdentity
	}{
		{
			name: "one trashed session",
			exclude: func(ctx context.Context, s *RawProjectionStore) (int, error) {
				removed, err := s.ExcludeTrashedSession(ctx, "codex:trashed-a")
				if removed {
					return 1, err
				}
				return 0, err
			},
			removed: func(first, second RawIdentity) []RawIdentity {
				if first.PublicID == "codex:trashed-a" {
					return []RawIdentity{first}
				}
				return []RawIdentity{second}
			},
		},
		{
			name: "empty trash across two groups",
			exclude: func(ctx context.Context, s *RawProjectionStore) (int, error) {
				return s.EmptyTrash(ctx)
			},
			removed: func(first, second RawIdentity) []RawIdentity {
				return []RawIdentity{first, second}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newProjectionFixture(t)
			first := f.projectSession(t, "device-a", "trashed-a")
			second := f.projectSession(t, "device-b", "trashed-b")
			first.PublicID, second.PublicID = "codex:trashed-a", "codex:trashed-b"
			require.NotEqual(t, first.GroupID, second.GroupID)
			if second.GroupID < first.GroupID {
				first, second = second, first
			}
			for _, alias := range []string{"codex:trashed-a", "codex:trashed-b"} {
				require.NoError(t, f.sink.SetCuration(t.Context(), alias, "trashed", true))
			}
			other, _ := f.accept(t, "device-c", "other-c", "")
			want := tt.removed(first, second)
			before := f.revisions(t)
			// Park the last removal, after every earlier group is materialized.
			f.parkOn(t, "DELETE", "sessions", "OLD.id = '"+want[len(want)-1].SessionID+"'")

			var removed int
			f.requireSelectableWhileParked(t, other, func(ctx context.Context) error {
				var err error
				removed, err = tt.exclude(ctx, f.sink)
				return err
			})

			assert.Equal(t, len(want), removed)
			now := f.revisions(t)
			assert.Equal(t, before.identity+int64(len(want)), now.identity)
			assert.Equal(t, before.corpus+int64(len(want)), now.corpus)
			var wantEvents []embeddingEvent
			for i, identity := range want {
				wantEvents = append(wantEvents, embeddingEvent{
					session: identity.SessionID, content: identity.ContentRevision, action: "remove",
					selection: now.selection, corpus: before.corpus + int64(i) + 1,
				})
			}
			assert.Equal(t, wantEvents, f.embeddingEvents(t, "remove"))
			var sessions int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&sessions))
			assert.Equal(t, 2-len(want), sessions)
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

func TestRawProjectionRollsBackWhenLeaseExpiresWaitingForCorpusRevision(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "expires-waiting", "")
	_, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-1")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "expiry-worker", 1, time.Second)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gate, err := f.admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer gate.Rollback()
	_, err = gate.ExecContext(ctx, `UPDATE raw_corpus_state SET corpus_revision=corpus_revision WHERE singleton=1`)
	require.NoError(t, err)

	projected := make(chan error, 1)
	go func() { projected <- f.sink.Project(ctx, leases[0], m, projectionOutcome("expires waiting")) }()
	require.Eventually(t, func() bool {
		var waiting int
		err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename=$1 AND wait_event_type='Lock' AND wait_event='transactionid'`, f.role).Scan(&waiting)
		return err == nil && waiting == 1
	}, 3*time.Second, 10*time.Millisecond)
	time.Sleep(time.Until(leases[0].ExpiresAt) + 100*time.Millisecond)
	require.NoError(t, gate.Commit())

	require.ErrorIs(t, <-projected, rawderive.ErrLeaseLost)
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&count))
	assert.Zero(t, count)
}
