package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/ledger"
)

func TestQueryLedgerUntil(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	seg := ledger.NewSegment("host-a", 1, base)
	for i, ts := range []time.Time{base.Add(-time.Hour), base, base.Add(90 * time.Minute)} {
		seg.Append(ledger.Event{
			EventID: ledger.NewEventID(), Zone: "default", Source: "host-a", SourceSeq: uint64(i),
			Timestamp: ts, EventClass: ledger.ClassHealth, PayloadTier: ledger.TierStructured,
		})
	}
	require.NoError(t, seg.Seal())
	_, err := d.AppendLedgerSegment(t.Context(), "default", seg, "local")
	require.NoError(t, err)

	for _, tc := range []struct {
		name  string
		q     ledger.Query
		wantN int
	}{
		{"unbounded", ledger.Query{Since: base.Add(-2 * time.Hour), Zone: "default", Limit: 10}, 3},
		{"until_excludes_boundary", ledger.Query{Since: base.Add(-2 * time.Hour), Until: base, Zone: "default", Limit: 10}, 1},
		{"limit_after_until", ledger.Query{Since: base.Add(-2 * time.Hour), Until: base, Zone: "default", Limit: 1}, 1},
		{"window", ledger.Query{Since: base, Until: base.Add(2 * time.Hour), Zone: "default", Limit: 10}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := d.QueryLedger(t.Context(), tc.q)
			require.NoError(t, err)
			n := 0
			for _, z := range res {
				n += len(z.Events)
			}
			assert.Equal(t, tc.wantN, n)
		})
	}
}
