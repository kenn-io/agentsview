package clickhouse

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
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

func (s *Store) ListFrictionFindings(
	_ context.Context, _ db.FrictionFindingFilter,
) ([]db.FrictionFinding, string, error) {
	return nil, "", db.ErrReadOnly
}

func (s *Store) ListFrictionDigests(_ context.Context, _, _ string) ([]db.FrictionDigest, error) {
	return nil, db.ErrReadOnly
}

func (s *Store) ListFrictionPatterns(
	_ context.Context, _ db.FrictionPatternFilter,
) ([]db.FrictionPattern, string, error) {
	return nil, "", db.ErrReadOnly
}

func (s *Store) GetFrictionIssueLinks(_ context.Context, _ []string) (map[string]db.FrictionIssueLink, error) {
	return map[string]db.FrictionIssueLink{}, nil
}
func (s *Store) UpsertFrictionIssueLink(_ context.Context, _ db.FrictionIssueLink) error {
	return db.ErrReadOnly
}
func (s *Store) DeleteFrictionIssueLink(_ context.Context, _ string) error { return db.ErrReadOnly }
func (s *Store) DueFrictionFilings(_ context.Context, _ time.Time, _ int) ([]db.FrictionIssueLink, error) {
	return []db.FrictionIssueLink{}, nil
}
func (s *Store) DigestDatesForFingerprints(_ context.Context, _ []string) ([]string, error) {
	return []string{}, nil
}
