package postgres

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ledger"
)

func (*HostedStore) QueryLedger(
	_ context.Context, _ ledger.Query,
) ([]ledger.ZoneEvents, error) {
	return []ledger.ZoneEvents{}, nil
}

func (*HostedStore) LedgerZones(_ context.Context) ([]string, error) {
	return []string{}, nil
}

// HostedStore has no tenant mapping for archive-wide ledger data. Keep the
// public identity boundary by exposing no ledger rows and rejecting writes.
func (*HostedStore) AppendLedgerSegment(
	_ context.Context, _ string, _ ledger.Segment, _ string,
) (ledger.PublishOutcome, error) {
	return ledger.Published, db.ErrReadOnly
}

func (*HostedStore) LatestLedgerSeq(_ context.Context, _, _ string) (uint64, error) {
	return 0, nil
}

func (*HostedStore) ListLedgerSegments(
	_ context.Context, _, _ string, _ uint64, _ int,
) ([]ledger.Segment, error) {
	return []ledger.Segment{}, nil
}

func (*HostedStore) LedgerSegmentSeqs(_ context.Context, _, _ string) ([]uint64, error) {
	return nil, nil
}

func (*HostedStore) LedgerStatus(_ context.Context, zone string) (ledger.ZoneStatus, error) {
	return ledger.ZoneStatus{Zone: zone, Sources: map[string]uint64{}}, nil
}

func (*HostedStore) GetLedgerVerifyState(
	_ context.Context, _, _ string,
) (*ledger.VerifyCheckpoint, error) {
	return nil, nil
}

func (*HostedStore) SaveLedgerVerifyState(
	_ context.Context, _, _ string, _ ledger.VerifyCheckpoint,
) error {
	return db.ErrReadOnly
}
