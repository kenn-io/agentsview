package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
)

func TestLedgerStubs(t *testing.T) {
	s := &Store{}
	ctx := t.Context()
	_, err := s.AppendLedgerSegment(ctx, "default", ledger.Segment{}, ledger.OriginLocal)
	require.ErrorIs(t, err, db.ErrReadOnly)
	require.ErrorIs(t, s.SaveLedgerVerifyState(ctx, "default", "host-a", ledger.VerifyCheckpoint{}), db.ErrReadOnly)
	seq, err := s.LatestLedgerSeq(ctx, "default", "host-a")
	require.NoError(t, err)
	assert.Zero(t, seq)
	segs, err := s.ListLedgerSegments(ctx, "default", "host-a", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, segs)
	st, err := s.LedgerStatus(ctx, "default")
	require.NoError(t, err)
	assert.Equal(t, "default", st.Zone)
	assert.NotNil(t, st.Sources)
	ckpt, err := s.GetLedgerVerifyState(ctx, "default", "host-a")
	require.NoError(t, err)
	assert.Nil(t, ckpt)
	results, err := s.QueryLedger(ctx, ledger.Query{})
	require.NoError(t, err)
	assert.Empty(t, results)
	zones, err := s.LedgerZones(ctx)
	require.NoError(t, err)
	assert.Empty(t, zones)
}
