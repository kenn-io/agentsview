package duckdb

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) InsertInsight(ctx context.Context, _ db.Insight) (int64, error) {
	return 0, db.ErrReadOnly
}
func (s *Store) DeleteInsight(ctx context.Context, _ int64) error { return db.ErrReadOnly }
func (s *Store) ListInsights(_ context.Context, _ db.InsightFilter) ([]db.Insight, error) {
	return []db.Insight{}, nil
}
func (s *Store) GetInsight(_ context.Context, _ int64) (*db.Insight, error) { return nil, nil }
func (s *Store) GetCachedInsight(_ context.Context, _ string) (*db.Insight, error) {
	return nil, nil
}

func (s *Store) RenameSession(ctx context.Context, _ string, _ *string) error { return db.ErrReadOnly }

func (s *Store) SoftDeleteSession(ctx context.Context, _ string) error { return db.ErrReadOnly }

func (s *Store) SoftDeleteSessions(ctx context.Context, _ []string) (int, error) {
	return 0, db.ErrReadOnly
}

func (s *Store) RestoreSession(ctx context.Context, _ string) (int64, error) {
	return 0, db.ErrReadOnly
}

func (s *Store) DeleteSessionIfTrashed(ctx context.Context, _ string) (int64, error) {
	return 0, db.ErrReadOnly
}
func (s *Store) EmptyTrash(ctx context.Context) (int, error)           { return 0, db.ErrReadOnly }
func (s *Store) UpsertSession(ctx context.Context, _ db.Session) error { return db.ErrReadOnly }
func (s *Store) ReplaceSessionMessages(ctx context.Context, _ string, _ []db.Message) error {
	return db.ErrReadOnly
}

func (s *Store) WriteSessionBatchAtomic(ctx context.Context,
	_ []db.SessionBatchWrite, _ ...func() error,
) (db.SessionBatchResult, error) {
	return db.SessionBatchResult{}, db.ErrReadOnly
}
