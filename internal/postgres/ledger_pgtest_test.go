//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/storage"
)

var ledgerPGT0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func pgLedgerSegment(t *testing.T, source string, seq uint64, n int) ledger.Segment {
	t.Helper()
	seg := ledger.NewSegment(source, seq, ledgerPGT0)
	for i := range n {
		seg.Append(ledger.Event{
			EventID:     ledger.DeterministicEventID(source, fmt.Sprintf("%d/%d", seq, i)),
			Zone:        "zone-a",
			Source:      source,
			SourceSeq:   uint64(i),
			Timestamp:   ledgerPGT0.Add(time.Duration(seq)*time.Hour + time.Duration(i)*time.Second),
			EventClass:  ledger.ClassHealth,
			PayloadTier: ledger.TierMetadataOnly,
		})
	}
	require.NoError(t, seg.Seal())
	return seg
}

func newLedgerTestStore(t *testing.T) *Store {
	t.Helper()
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)
	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store
}

// writablePGTestStore bypasses Store's read-only db.Store contract for tests
// that exercise the underlying PostgreSQL ledger writer and reader together.
type writablePGTestStore struct {
	*Store
}

func newWritablePGTestStore(t *testing.T) *writablePGTestStore {
	t.Helper()
	return &writablePGTestStore{Store: newLedgerTestStore(t)}
}

func (s *writablePGTestStore) AppendLedgerSegment(
	ctx context.Context, zone string, seg ledger.Segment, origin string,
) (ledger.PublishOutcome, error) {
	return appendPGTestSegment(ctx, s.Store, zone, seg, origin)
}

func (s *writablePGTestStore) SaveLedgerVerifyState(
	ctx context.Context, zone, source string, checkpoint ledger.VerifyCheckpoint,
) error {
	return savePGTestVerifyState(ctx, s.Store, zone, source, checkpoint)
}

func appendPGTestSegment(
	ctx context.Context, store *Store, zone string, seg ledger.Segment, origin string,
) (ledger.PublishOutcome, error) {
	prepared, err := ledger.PrepareAppend(zone, seg, origin)
	if err != nil {
		return ledger.Published, err
	}
	tx, err := store.pg.BeginTx(ctx, nil)
	if err != nil {
		return ledger.Published, fmt.Errorf("beginning test ledger append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	outcome, err := appendPreparedLedgerSegmentTx(ctx, tx, prepared)
	if err != nil || outcome == ledger.AlreadyIdentical {
		return outcome, err
	}
	if err := tx.Commit(); err != nil {
		return ledger.Published, mapLedgerPGError("committing test ledger append", err)
	}
	return outcome, nil
}

func savePGTestVerifyState(
	ctx context.Context, store *Store, zone, source string, checkpoint ledger.VerifyCheckpoint,
) error {
	failures, missing, err := ledger.EncodeCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	if checkpoint.VerifiedSeq > uint64(1<<63-1) {
		return fmt.Errorf("ledger verify watermark %d exceeds i64::MAX", checkpoint.VerifiedSeq)
	}
	_, err = store.pg.ExecContext(ctx, `
		INSERT INTO ledger_verify_state (
			zone, source, verified_seq, failures_json, missing_json, updated_at
		) VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (zone, source) DO UPDATE SET
			verified_seq = excluded.verified_seq,
			failures_json = excluded.failures_json,
			missing_json = excluded.missing_json,
			updated_at = excluded.updated_at`,
		zone, source, int64(checkpoint.VerifiedSeq), failures, missing,
	)
	return err
}

// ledgerParityStores returns the SQLite archive and the PG store so every
// assertion below runs against both backends.
type ledgerParityStore interface {
	ledger.WriterStore
	ledger.VerifyStore
}

func ledgerParityStores(t *testing.T) map[string]ledgerParityStore {
	t.Helper()
	local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { local.Close() })
	return map[string]ledgerParityStore{"sqlite": local, "postgres": newWritablePGTestStore(t)}
}

func TestLedgerStoreParity(t *testing.T) {
	for name, store := range ledgerParityStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			a1 := pgLedgerSegment(t, "host-a", 1, 2)
			out, err := store.AppendLedgerSegment(ctx, "zone-a", a1, ledger.OriginLocal)
			require.NoError(t, err)
			assert.Equal(t, ledger.Published, out)
			out, err = store.AppendLedgerSegment(ctx, "zone-a", a1, ledger.OriginPush)
			require.NoError(t, err)
			assert.Equal(t, ledger.AlreadyIdentical, out)
			_, err = store.AppendLedgerSegment(ctx, "zone-a", pgLedgerSegment(t, "host-a", 1, 3), ledger.OriginPush)
			require.ErrorIs(t, err, ledger.ErrIntegrity)
			_, err = store.AppendLedgerSegment(ctx, "zone-a", pgLedgerSegment(t, "host-a", 3, 1), ledger.OriginLocal)
			require.NoError(t, err)

			latest, err := store.LatestLedgerSeq(ctx, "zone-a", "host-a")
			require.NoError(t, err)
			assert.Equal(t, uint64(3), latest)
			segs, err := store.ListLedgerSegments(ctx, "zone-a", "host-a", 0, 0)
			require.NoError(t, err)
			require.Len(t, segs, 2)
			assert.True(t, segs[0].ContentMatches(a1))
			segs, err = store.ListLedgerSegments(ctx, "zone-a", "host-a", 0, -1)
			require.NoError(t, err)
			require.Len(t, segs, 2, "negative limits also mean all segments")
			segs, err = store.ListLedgerSegments(ctx, "zone-a", "host-a", 0, 1)
			require.NoError(t, err)
			require.Len(t, segs, 1, "positive limits still cap the result")
			assert.True(t, segs[0].ContentMatches(a1))
			seqs, err := store.LedgerSegmentSeqs(ctx, "zone-a", "host-a")
			require.NoError(t, err)
			assert.Equal(t, []uint64{1, 3}, seqs)

			st, err := store.LedgerStatus(ctx, "zone-a")
			require.NoError(t, err)
			assert.Equal(t, 2, st.Segments)
			assert.Equal(t, 3, st.Events)
			assert.Equal(t, map[string]uint64{"host-a": 3}, st.Sources)
			assert.Equal(t, [][2]string{{"host-a", "2"}}, st.Gaps)

			rep, err := ledger.VerifyZone(ctx, store, "zone-a", false)
			require.NoError(t, err)
			assert.Equal(t, 2, rep.NewlyVerified)
			ckpt, err := store.GetLedgerVerifyState(ctx, "zone-a", "host-a")
			require.NoError(t, err)
			require.NotNil(t, ckpt)
			assert.Equal(t, uint64(3), ckpt.VerifiedSeq)
			assert.Equal(t, [][2]string{{"host-a", "2"}}, ckpt.Missing)

			w := ledger.NewWriter(store, "zone-a", "av-local", nil)
			seg, err := w.Append(ctx, []ledger.Event{{EventClass: ledger.ClassDecision, PayloadTier: ledger.TierStructured,
				Payload: map[string]any{"subsystem": "parity", "summary": "written by the writer"}}})
			require.NoError(t, err)
			assert.Equal(t, uint64(1), seg.SourceSeq)
		})
	}
}

func TestLedgerPGAppendOnly(t *testing.T) {
	store := newWritablePGTestStore(t)
	ctx := t.Context()
	_, err := store.AppendLedgerSegment(ctx, "zone-a", pgLedgerSegment(t, "host-a", 1, 1), ledger.OriginLocal)
	require.NoError(t, err)
	for _, stmt := range []string{
		`UPDATE ledger_segments SET origin = 'push'`,
		`DELETE FROM ledger_segments`,
		`UPDATE ledger_events SET summary = 'x'`,
		`DELETE FROM ledger_events`,
	} {
		t.Run(stmt, func(t *testing.T) {
			_, err := store.DB().ExecContext(ctx, stmt)
			require.Error(t, err)
			assert.ErrorIs(t, mapLedgerPGError("mutating", err), ledger.ErrAppendOnly)
		})
	}
	assert.Equal(t, 1, pgTableCount(t, ctx, store.DB(), "ledger_segments"))
	assert.Equal(t, 1, pgTableCount(t, ctx, store.DB(), "ledger_events"))
}

func TestEnsureLedgerSchemaFastPathCreatesLedgerTables(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	ctx := t.Context()
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema))

	_, err = pg.ExecContext(ctx, `
		DROP TABLE ledger_import_state, ledger_verify_state,
			ledger_events, ledger_segments CASCADE;
		DROP FUNCTION ledger_reject_mutation()`)
	require.NoError(t, err)
	require.True(t, pushSchemaCurrent(ctx, pg),
		"the legacy push schema probe must still select the fast path")

	syncer := &Sync{pg: pg, schema: schemaTestSchema}
	require.NoError(t, syncer.EnsureSchema(ctx))
	for _, table := range []string{
		"ledger_segments", "ledger_events", "ledger_verify_state", "ledger_import_state",
	} {
		assert.True(t, pgHasTable(ctx, pg, table),
			"fast path should create %s", table)
	}
	var guardsCurrent bool
	require.NoError(t, pg.QueryRowContext(ctx, ledgerAppendOnlyGuardsCurrentSQL,
		ledgerAppendOnlyFunctionBody).Scan(&guardsCurrent))
	assert.True(t, guardsCurrent, "fast path should install ledger append-only guards")
}

func TestEnsureLedgerSchemaFastPathRequiresExistingTablesForRestrictedRole(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_ledger_privilege_test"
	const role = "agentsview_ledger_restricted"
	const rolePassword = "agentsview_ledger_restricted_pw"

	admin, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open admin")
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(t.Context(), admin, schema))
	require.True(t, pushSchemaCurrent(t.Context(), admin),
		"fixture must exercise the schema-current sync fast path")

	_, _ = admin.Exec(`DROP OWNED BY ` + role)
	_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
	_, err = admin.Exec(`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + rolePassword + `'`)
	require.NoError(t, err, "create restricted role")
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		_, _ = admin.Exec(`DROP OWNED BY ` + role)
		_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role,
	} {
		_, err = admin.Exec(grant)
		require.NoError(t, err, grant)
	}

	restrictedURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	restrictedURL.User = url.UserPassword(role, rolePassword)
	restricted, err := Open(restrictedURL.String(), schema, true)
	require.NoError(t, err, "Open restricted")
	t.Cleanup(func() { _ = restricted.Close() })

	preprovisioned := &Sync{pg: restricted, schema: schema}
	require.NoError(t, preprovisioned.EnsureSchema(t.Context()),
		"a restricted push role may use a fully provisioned ledger schema")
	require.True(t, preprovisioned.schemaDone)
	policy := &storage.LedgerPushPolicy{Zones: []string{"default"}}
	assertMissingGuardsRejected := func() {
		t.Helper()
		syncer := &Sync{pg: restricted, schema: schema, ledgerPolicy: policy}
		err := syncer.EnsureSchema(t.Context())
		require.Error(t, err,
			"a restricted role must not mark a ledger schema without append-only guards ready")
		assert.Contains(t, err.Error(), "append-only guards")
		assert.Contains(t, err.Error(), "CREATE privilege")
		assert.False(t, syncer.schemaDone,
			"a ledger schema without append-only guards must remain retryable")
	}
	restoreGuards := func() {
		t.Helper()
		require.NoError(t, ensureLedgerSchemaPG(t.Context(), admin),
			"restore ledger append-only guards")
		guardsCurrent, err := ledgerAppendOnlyGuardsCurrentPG(t.Context(), admin)
		require.NoError(t, err, "check restored ledger append-only guards")
		assert.True(t, guardsCurrent,
			"schema setup should repair invalid ledger append-only guards")
	}

	_, err = admin.Exec(`DROP FUNCTION ledger_reject_mutation() CASCADE`)
	require.NoError(t, err, "remove the ledger guard function and triggers")
	assertMissingGuardsRejected()
	restoreGuards()

	_, err = admin.Exec(`DROP TRIGGER ledger_events_append_only ON ledger_events`)
	require.NoError(t, err, "remove one ledger guard trigger")
	assertMissingGuardsRejected()
	restoreGuards()

	_, err = admin.Exec(`ALTER TABLE ledger_events
		DISABLE TRIGGER ledger_events_append_only`)
	require.NoError(t, err, "disable one ledger guard trigger")
	assertMissingGuardsRejected()
	restoreGuards()

	_, err = admin.Exec(`
		DROP TRIGGER ledger_events_append_only ON ledger_events;
		CREATE TRIGGER ledger_events_append_only BEFORE UPDATE ON ledger_events
		FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation()`)
	require.NoError(t, err, "replace a ledger guard with an incomplete event mask")
	assertMissingGuardsRejected()
	restoreGuards()

	_, err = admin.Exec(`
		CREATE FUNCTION ledger_test_noop() RETURNS trigger
		LANGUAGE plpgsql AS $ledger_test_noop$
		BEGIN RETURN OLD; END;
		$ledger_test_noop$;
		DROP TRIGGER ledger_events_append_only ON ledger_events;
		CREATE TRIGGER ledger_events_append_only
		BEFORE UPDATE OR DELETE ON ledger_events
		FOR EACH ROW EXECUTE FUNCTION ledger_test_noop()`)
	require.NoError(t, err, "replace a ledger guard with another function")
	assertMissingGuardsRejected()
	restoreGuards()

	_, err = admin.Exec(`DROP TABLE ledger_segments CASCADE`)
	require.NoError(t, err, "simulate a partially provisioned ledger schema")

	disabled := &Sync{pg: restricted, schema: schema}
	require.NoError(t, disabled.EnsureSchema(t.Context()),
		"missing optional ledger tables must not block a session-only push")
	require.True(t, disabled.schemaDone)

	syncer := &Sync{
		pg: restricted, schema: schema,
		ledgerPolicy: policy,
	}
	err = syncer.EnsureSchema(t.Context())
	require.Error(t, err,
		"a restricted role must not mark an incomplete ledger schema ready")
	assert.Contains(t, err.Error(), "ledger")
	assert.Contains(t, err.Error(), "CREATE privilege")
	assert.Contains(t, err.Error(), "ledger_segments")
	assert.False(t, syncer.schemaDone,
		"an incomplete ledger schema must remain retryable")
}

func TestEnsureLedgerSchemaPGSerializesConcurrentGuardInstallers(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	_, err = pg.ExecContext(t.Context(), `
		DROP TRIGGER ledger_segments_append_only ON ledger_segments;
		DROP TRIGGER ledger_events_append_only ON ledger_events`)
	require.NoError(t, err)

	blocker, err := pg.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(t.Context(),
		`LOCK TABLE ledger_segments IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	dsnA, err := appendConnParams(pgURL, map[string]string{
		"application_name": "ledger-guard-a",
	})
	require.NoError(t, err)
	dsnB, err := appendConnParams(pgURL, map[string]string{
		"application_name": "ledger-guard-b",
	})
	require.NoError(t, err)
	installerA, err := Open(dsnA, schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, installerA.Close()) })
	installerB, err := Open(dsnB, schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, installerB.Close()) })

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, installer := range []*sql.DB{installerA, installerB} {
		go func(conn *sql.DB) {
			<-start
			results <- ensureLedgerSchemaPG(t.Context(), conn)
		}(installer)
	}
	close(start)
	require.Eventually(t, func() bool {
		var waiting int
		err := pg.QueryRowContext(t.Context(), `
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE application_name IN ('ledger-guard-a', 'ledger-guard-b')
				AND wait_event_type = 'Lock'
		`).Scan(&waiting)
		return err == nil && waiting == 2
	}, 5*time.Second, 10*time.Millisecond,
		"both installers should reach the locked guard DDL")

	require.NoError(t, blocker.Commit(), "release ledger_segments")
	require.NoError(t, <-results, "first concurrent installer")
	require.NoError(t, <-results, "second concurrent installer")

	var guards int
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT COUNT(*)
		FROM pg_trigger
		WHERE (tgname = 'ledger_segments_append_only'
				AND tgrelid = 'ledger_segments'::regclass)
			OR (tgname = 'ledger_events_append_only'
				AND tgrelid = 'ledger_events'::regclass)
	`).Scan(&guards))
	assert.Equal(t, 2, guards)
}

func TestLedgerPGRustFixturesRoundTrip(t *testing.T) {
	store := newWritablePGTestStore(t)
	src := filepath.Join("..", "ledger", "testdata", "segments")
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(src, e.Name()))
			require.NoError(t, err)
			seg, err := ledger.ParseSegmentFile(want)
			require.NoError(t, err)
			zone := "default"
			if len(seg.Events) > 0 {
				zone = seg.Events[0].Zone
			}
			_, err = store.AppendLedgerSegment(t.Context(), zone, seg, ledger.OriginImport)
			if e.Name() == "fixture-bigseq-000001.json" {
				require.Error(t, err, "an event seq above i64::MAX must be refused")
				return
			}
			require.NoError(t, err)
			gotSegs, err := store.ListLedgerSegments(t.Context(), zone, seg.Source, seg.SourceSeq-1, 1)
			require.NoError(t, err)
			require.Len(t, gotSegs, 1)
			got, err := ledger.MarshalSegmentFile(gotSegs[0])
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got))
		})
	}
}
