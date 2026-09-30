package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
)

func TestHostedStoreLedgerReadsAreEmpty(t *testing.T) {
	h := &HostedStore{}
	ctx := t.Context()

	query, err := h.QueryLedger(ctx, ledger.Query{})
	require.NoError(t, err)
	assert.Empty(t, query)

	zones, err := h.LedgerZones(ctx)
	require.NoError(t, err)
	assert.Empty(t, zones)

	latest, err := h.LatestLedgerSeq(ctx, "zone-a", "host-a")
	require.NoError(t, err)
	assert.Zero(t, latest)

	segments, err := h.ListLedgerSegments(ctx, "zone-a", "host-a", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, segments)

	seqs, err := h.LedgerSegmentSeqs(ctx, "zone-a", "host-a")
	require.NoError(t, err)
	assert.Empty(t, seqs)

	status, err := h.LedgerStatus(ctx, "zone-a")
	require.NoError(t, err)
	assert.Equal(t, ledger.ZoneStatus{Zone: "zone-a", Sources: map[string]uint64{}}, status)

	checkpoint, err := h.GetLedgerVerifyState(ctx, "zone-a", "host-a")
	require.NoError(t, err)
	assert.Nil(t, checkpoint)
}

func TestHostedStoreLedgerWritesAreReadOnly(t *testing.T) {
	h := &HostedStore{}

	segment := ledger.NewSegment("host-a", 1, time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	segment.Append(ledger.Event{
		EventID:     ledger.DeterministicEventID("host-a", "1/0"),
		Zone:        "zone-a",
		Source:      "host-a",
		SourceSeq:   0,
		Timestamp:   time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		EventClass:  ledger.ClassHealth,
		PayloadTier: ledger.TierMetadataOnly,
		Payload:     map[string]any{"action": "test"},
	})
	require.NoError(t, segment.Seal())

	outcome, err := h.AppendLedgerSegment(t.Context(), "zone-a", segment, ledger.OriginLocal)
	assert.Equal(t, ledger.Published, outcome)
	require.ErrorIs(t, err, db.ErrReadOnly)

	err = h.SaveLedgerVerifyState(t.Context(), "zone-a", "host-a", ledger.VerifyCheckpoint{})
	require.ErrorIs(t, err, db.ErrReadOnly)
}

func TestStoreLedgerWritesAreReadOnly(t *testing.T) {
	s := &Store{}
	outcome, err := s.AppendLedgerSegment(
		t.Context(), "zone-a", ledger.Segment{}, ledger.OriginLocal,
	)
	assert.Equal(t, ledger.Published, outcome)
	require.ErrorIs(t, err, db.ErrReadOnly)

	err = s.SaveLedgerVerifyState(t.Context(), "zone-a", "host-a", ledger.VerifyCheckpoint{})
	require.ErrorIs(t, err, db.ErrReadOnly)
}
