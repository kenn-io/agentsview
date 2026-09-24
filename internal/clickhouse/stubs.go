package clickhouse

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ledger"
)

func (s *Store) InsertInsight(_ context.Context, _ db.Insight) (int64, error) {
	return 0, db.ErrReadOnly
}
func (s *Store) DeleteInsight(_ context.Context, _ int64) error { return db.ErrReadOnly }
func (s *Store) ListInsights(_ context.Context, _ db.InsightFilter) ([]db.Insight, error) {
	return []db.Insight{}, nil
}
func (s *Store) GetInsight(_ context.Context, _ int64) (*db.Insight, error) { return nil, nil }
func (s *Store) GetCachedInsight(_ context.Context, _ string) (*db.Insight, error) {
	return nil, nil
}

func (s *Store) RenameSession(_ context.Context, _ string, _ *string) error { return db.ErrReadOnly }

func (s *Store) SoftDeleteSession(_ context.Context, _ string) error { return db.ErrReadOnly }

func (s *Store) SoftDeleteSessions(_ context.Context, _ []string) (int, error) {
	return 0, db.ErrReadOnly
}

func (s *Store) RestoreSession(_ context.Context, _ string) (int64, error) { return 0, db.ErrReadOnly }

func (s *Store) DeleteSessionIfTrashed(_ context.Context, _ string) (int64, error) {
	return 0, db.ErrReadOnly
}
func (s *Store) EmptyTrash(_ context.Context) (int, error)           { return 0, db.ErrReadOnly }
func (s *Store) UpsertSession(_ context.Context, _ db.Session) error { return db.ErrReadOnly }
func (s *Store) ReplaceSessionMessages(_ context.Context, _ string, _ []db.Message) error {
	return db.ErrReadOnly
}

func (s *Store) WriteSessionBatchAtomic(_ context.Context,
	_ []db.SessionBatchWrite, _ ...func() error,
) (db.SessionBatchResult, error) {
	return db.SessionBatchResult{}, db.ErrReadOnly
}

func (s *Store) AppendLedgerSegment(_ context.Context, _ string, _ ledger.Segment, _ string) (ledger.PublishOutcome, error) {
	return ledger.Published, db.ErrReadOnly
}
func (s *Store) LatestLedgerSeq(_ context.Context, _, _ string) (uint64, error) { return 0, nil }
func (s *Store) ListLedgerSegments(_ context.Context, _, _ string, _ uint64, _ int) ([]ledger.Segment, error) {
	return []ledger.Segment{}, nil
}

func (s *Store) LedgerSegmentSeqs(_ context.Context, _, _ string) ([]uint64, error) { return nil, nil }

func (s *Store) LedgerStatus(_ context.Context, zone string) (ledger.ZoneStatus, error) {
	return ledger.ZoneStatus{Zone: zone, Sources: map[string]uint64{}}, nil
}

func (s *Store) GetLedgerVerifyState(_ context.Context, _, _ string) (*ledger.VerifyCheckpoint, error) {
	return nil, nil
}

func (s *Store) SaveLedgerVerifyState(_ context.Context, _, _ string, _ ledger.VerifyCheckpoint) error {
	return db.ErrReadOnly
}

func (s *Store) QueryLedger(_ context.Context, _ ledger.Query) ([]ledger.ZoneEvents, error) {
	return []ledger.ZoneEvents{}, nil
}

func (s *Store) LedgerZones(_ context.Context) ([]string, error) { return []string{}, nil }
func (s *Store) FrictionSubjectsForDate(_ context.Context, _ string, _ *time.Location, _ bool) ([]db.FrictionSubject, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) FrictionFindingsForSubjects(_ context.Context, _ []string) ([]db.FrictionFinding, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) FrictionUsageForSessions(_ context.Context, _ []string) (map[string]friction.SessionUsage, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) FrictionArchiveSpend(_ context.Context, _, _ string, _ *time.Location) (*friction.ArchiveSpend, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) SaveFrictionDigest(_ context.Context, _ db.FrictionDigest, _ []db.FrictionDigestSubject, _ []db.FrictionPatternUpdate) error {
	return db.ErrReadOnly
}

func (s *Store) GetFrictionDigest(_ context.Context, _ string) (*db.FrictionDigest, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) LatestFrictionDigestDate(_ context.Context) (string, error) {
	return "", db.ErrReadOnly
}

func (s *Store) EarliestSessionDate(_ context.Context, _ *time.Location) (string, error) {
	return "", db.ErrReadOnly
}

func (s *Store) UpdateFrictionDigestRender(_ context.Context, _ string, _, _ []byte, _ int) error {
	return db.ErrReadOnly
}
