//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
	"go.kenn.io/agentsview/internal/ledger/segfile"
	"go.kenn.io/agentsview/internal/storage"
)

const ledgerPushSchema = "agentsview_ledger_push_test"

// ledgerPushEnv is one PostgreSQL hub schema plus helpers to build laptops.
type ledgerPushEnv struct {
	t  *testing.T
	pg *sql.DB
}

func newLedgerPushEnv(t *testing.T) *ledgerPushEnv {
	t.Helper()
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, ledgerPushSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { pg.Close() })
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + ledgerPushSchema + ` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(t.Context(), pg, ledgerPushSchema))
	return &ledgerPushEnv{t: t, pg: pg}
}

func (e *ledgerPushEnv) laptop(machine string, policy *storage.LedgerPushPolicy) (*db.DB, *Sync) {
	e.t.Helper()
	local, err := db.Open(e.t.Context(), filepath.Join(e.t.TempDir(), "sessions.db"))
	require.NoError(e.t, err)
	e.t.Cleanup(func() { local.Close() })
	return local, &Sync{
		local: local, pg: e.pg, machine: machine, schema: ledgerPushSchema,
		schemaDone: true, syncStateTarget: "hub", ledgerPolicy: policy,
	}
}

func (e *ledgerPushEnv) hub() *Store { return &Store{pg: e.pg} }

func appendLocal(t *testing.T, local *db.DB, zone, source string, tier ledger.PayloadTier, summary string) ledger.Segment {
	t.Helper()
	seg, err := ledger.NewWriter(local, zone, source, nil).Append(t.Context(), []ledger.Event{{
		EventClass: ledger.ClassHealth, PayloadTier: tier,
		Payload: map[string]any{"subsystem": "push-test", "summary": summary},
	}})
	require.NoError(t, err)
	return seg
}

func pushStatus(t *testing.T, local *db.DB) LedgerPushStatus {
	t.Helper()
	value, err := local.GetSyncState(t.Context(), LedgerPushStatusKeyPrefix+"hub")
	require.NoError(t, err)
	st, err := DecodeLedgerPushStatus(value)
	require.NoError(t, err)
	return st
}

var defaultPolicy = &storage.LedgerPushPolicy{Zones: []string{"default"}}

func TestLedgerPush(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, env *ledgerPushEnv)
	}{
		{"two_machines_converge", func(t *testing.T, env *ledgerPushEnv) {
			t.Helper()
			ctx := t.Context()
			// The fixtures include confidential-tier events; replicate them so
			// both laptops end up with every segment on the hub.
			all := &storage.LedgerPushPolicy{Zones: []string{"default"}, ReplicateConfidential: true}
			a, syncA := env.laptop("machine-a", all)
			b, syncB := env.laptop("machine-b", all)
			fixtures := filepath.Join("..", "ledger", "testdata", "segments")
			for _, local := range []*db.DB{a, b} {
				_, err := segfile.ImportDir(ctx, fixtures, "default", local)
				require.NoError(t, err)
			}
			segA := appendLocal(t, a, "default", "av-a", ledger.TierStructured, "from a")
			segB := appendLocal(t, b, "default", "av-b", ledger.TierStructured, "from b")

			require.NoError(t, syncA.syncLedgerSegments(ctx, false))
			require.NoError(t, syncB.syncLedgerSegments(ctx, false))
			assert.Equal(t, 6, pushStatus(t, a).Zones["default"].Pushed, "five fixtures plus av-a")
			assert.Equal(t, 1, pushStatus(t, b).Zones["default"].Pushed, "only av-b is new to the hub")
			assert.Equal(t, 5, pushStatus(t, b).Zones["default"].Identical, "the shared imports are identical")

			hub := env.hub()
			st, err := hub.LedgerStatus(ctx, "default")
			require.NoError(t, err)
			assert.Equal(t, uint64(1), st.Sources["av-a"])
			assert.Equal(t, uint64(1), st.Sources["av-b"])
			for _, local := range []*db.DB{a, b} {
				mine, err := local.LedgerStatus(ctx, "default")
				require.NoError(t, err)
				for source, seq := range mine.Sources {
					assert.Equal(t, seq, st.Sources[source], source)
				}
			}
			for source, want := range map[string]ledger.Segment{"av-a": segA, "av-b": segB} {
				got, err := hub.ListLedgerSegments(ctx, "default", source, 0, 0)
				require.NoError(t, err)
				require.Len(t, got, 1)
				assert.True(t, got[0].ContentMatches(want), source)
			}

			require.NoError(t, syncA.syncLedgerSegments(ctx, false))
			again := pushStatus(t, a).Zones["default"]
			assert.Equal(t, 0, again.Pushed, "a re-push inserts nothing")
		}},
		{"confidential_stays_local_by_default", func(t *testing.T, env *ledgerPushEnv) {
			t.Helper()
			ctx := t.Context()
			local, sync := env.laptop("machine-a", defaultPolicy)
			appendLocal(t, local, "default", "av-a", ledger.TierStructured, "public")
			appendLocal(t, local, "default", "av-a", ledger.TierConfidential, "secret")
			require.NoError(t, sync.syncLedgerSegments(ctx, false))
			counts := pushStatus(t, local).Zones["default"]
			assert.Equal(t, 1, counts.Pushed)
			assert.Equal(t, 1, counts.HeldBack)
			seqs, err := env.hub().LedgerSegmentSeqs(ctx, "default", "av-a")
			require.NoError(t, err)
			assert.Equal(t, []uint64{1}, seqs)

			sync.ledgerPolicy = &storage.LedgerPushPolicy{Zones: []string{"default"}, ReplicateConfidential: true}
			require.NoError(t, sync.syncLedgerSegments(ctx, true))
			seqs, err = env.hub().LedgerSegmentSeqs(ctx, "default", "av-a")
			require.NoError(t, err)
			assert.Equal(t, []uint64{1, 2}, seqs)
		}},
		{"replicate_false_zone_is_never_pushed", func(t *testing.T, env *ledgerPushEnv) {
			t.Helper()
			ctx := t.Context()
			local, sync := env.laptop("machine-a", defaultPolicy)
			appendLocal(t, local, "private", "av-a", ledger.TierStructured, "local only")
			require.NoError(t, sync.syncLedgerSegments(ctx, false))
			assert.Equal(t, 1, pushStatus(t, local).Zones["private"].HeldBack)
			st, err := env.hub().LedgerStatus(ctx, "private")
			require.NoError(t, err)
			assert.Equal(t, 0, st.Segments)
		}},
		{"tampered_local_segment_is_refused_and_reported", func(t *testing.T, env *ledgerPushEnv) {
			t.Helper()
			ctx := t.Context()
			local, sync := env.laptop("machine-a", defaultPolicy)
			appendLocal(t, local, "default", "av-a", ledger.TierStructured, "honest")
			appendLocal(t, local, "default", "av-a", ledger.TierStructured, "tampered")
			// Simulate damage behind the append-only guard with a separate
			// connection to the archive file.
			raw, err := sql.Open("sqlite3", local.Path())
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, `DROP TRIGGER trg_ledger_segments_no_update`)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx,
				`UPDATE ledger_segments SET events_json = replace(events_json, 'tampered', 'tempered') WHERE source_seq = 2`)
			require.NoError(t, err)
			require.NoError(t, raw.Close())

			for range 2 { // the refusal is sticky across pushes
				require.NoError(t, sync.syncLedgerSegments(ctx, false))
				st := pushStatus(t, local)
				require.Len(t, st.Failures, 1)
				assert.Equal(t, [3]string{"default", "av-a", "2"}, [3]string{st.Failures[0][0], st.Failures[0][1], st.Failures[0][2]})
				assert.Contains(t, st.Failures[0][3], "checksum mismatch")
			}
			seqs, err := env.hub().LedgerSegmentSeqs(ctx, "default", "av-a")
			require.NoError(t, err)
			assert.Equal(t, []uint64{1}, seqs)
		}},
		{"conflicting_identity_is_never_overwritten", func(t *testing.T, env *ledgerPushEnv) {
			t.Helper()
			ctx := t.Context()
			other, _ := env.laptop("machine-b", defaultPolicy)
			theirs := appendLocal(t, other, "default", "shared-name", ledger.TierStructured, "theirs")
			_, err := env.hub().AppendLedgerSegment(ctx, "default", theirs, ledger.OriginPush)
			require.NoError(t, err)

			local, sync := env.laptop("machine-a", defaultPolicy)
			appendLocal(t, local, "default", "shared-name", ledger.TierStructured, "mine")
			require.NoError(t, sync.syncLedgerSegments(ctx, false))
			st := pushStatus(t, local)
			require.Len(t, st.Failures, 1)
			assert.Contains(t, st.Failures[0][3], "DIFFERENT content")
			got, err := env.hub().ListLedgerSegments(ctx, "default", "shared-name", 0, 0)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.True(t, got[0].ContentMatches(theirs), "the hub copy is unchanged")
		}},
		{"wiped_watermark_causes_no_duplicates", func(t *testing.T, env *ledgerPushEnv) {
			t.Helper()
			ctx := t.Context()
			local, sync := env.laptop("machine-a", defaultPolicy)
			appendLocal(t, local, "default", "av-a", ledger.TierStructured, "one")
			appendLocal(t, local, "default", "av-a", ledger.TierStructured, "two")
			require.NoError(t, sync.syncLedgerSegments(ctx, false))
			require.NoError(t, sync.effectiveSyncState().SetSyncState(ctx, ledgerPushWatermarkKey, ""))
			require.NoError(t, sync.syncLedgerSegments(ctx, false))
			counts := pushStatus(t, local).Zones["default"]
			assert.Equal(t, 0, counts.Pushed)
			assert.Equal(t, 2, counts.Identical)
			assert.Equal(t, 2, pgTableCount(t, ctx, env.pg, "ledger_segments"))
			assert.Equal(t, 2, pgTableCount(t, ctx, env.pg, "ledger_events"))
		}},
		{"push_runs_the_phase_and_nil_policy_skips_it", func(t *testing.T, env *ledgerPushEnv) {
			t.Helper()
			ctx := t.Context()
			off, offSync := env.laptop("machine-off", nil)
			appendLocal(t, off, "default", "av-off", ledger.TierStructured, "ledger off")
			_, err := offSync.Push(ctx, false, nil)
			require.NoError(t, err)
			assert.Equal(t, 0, pgTableCount(t, ctx, env.pg, "ledger_segments"))

			on, onSync := env.laptop("machine-on", defaultPolicy)
			appendLocal(t, on, "default", "av-on", ledger.TierStructured, "ledger on")
			_, err = onSync.Push(ctx, false, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, pgTableCount(t, ctx, env.pg, "ledger_segments"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, newLedgerPushEnv(t)) })
	}
}

func TestPushSchemaCurrentRequiresLedgerTables(t *testing.T) {
	env := newLedgerPushEnv(t)
	ctx := context.Background()
	require.True(t, pushSchemaCurrent(ctx, env.pg))
	_, err := env.pg.Exec(`DROP TABLE ledger_events`)
	require.NoError(t, err)
	assert.False(t, pushSchemaCurrent(ctx, env.pg), "a hub from before the ledger must run EnsureSchema")
	sync := &Sync{pg: env.pg, schema: ledgerPushSchema}
	require.NoError(t, sync.EnsureSchema(ctx))
	assert.True(t, pushSchemaCurrent(ctx, env.pg))
}
