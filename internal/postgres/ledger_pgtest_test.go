//go:build pgtest

package postgres

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
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
	return map[string]ledgerParityStore{"sqlite": local, "postgres": newLedgerTestStore(t)}
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
	store := newLedgerTestStore(t)
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

func TestLedgerPGRustFixturesRoundTrip(t *testing.T) {
	store := newLedgerTestStore(t)
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
			_, err = store.AppendLedgerSegment(t.Context(), "default", seg, ledger.OriginImport)
			if e.Name() == "fixture-bigseq-000001.json" {
				require.Error(t, err, "an event seq above i64::MAX must be refused")
				return
			}
			require.NoError(t, err)
			gotSegs, err := store.ListLedgerSegments(t.Context(), "default", seg.Source, seg.SourceSeq-1, 1)
			require.NoError(t, err)
			require.Len(t, gotSegs, 1)
			got, err := ledger.MarshalSegmentFile(gotSegs[0])
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got))
		})
	}
}
