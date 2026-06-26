package duckdb

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) InsertInsight(_ db.Insight) (int64, error) { return 0, db.ErrReadOnly }
func (s *Store) DeleteInsight(_ int64) error               { return db.ErrReadOnly }
func (s *Store) ListInsights(_ context.Context, _ db.InsightFilter) ([]db.Insight, error) {
	return []db.Insight{}, nil
}
func (s *Store) GetInsight(_ context.Context, _ int64) (*db.Insight, error) { return nil, nil }
func (s *Store) GetCachedInsight(_ context.Context, _ string) (*db.Insight, error) {
	return nil, nil
}
func (s *Store) RenameSession(_ string, _ *string) error        { return db.ErrReadOnly }
func (s *Store) SoftDeleteSession(_ string) error               { return db.ErrReadOnly }
func (s *Store) SoftDeleteSessions(_ []string) (int, error)     { return 0, db.ErrReadOnly }
func (s *Store) RestoreSession(_ string) (int64, error)         { return 0, db.ErrReadOnly }
func (s *Store) DeleteSessionIfTrashed(_ string) (int64, error) { return 0, db.ErrReadOnly }
func (s *Store) ListTrashedSessions(_ context.Context) ([]db.Session, error) {
	return []db.Session{}, nil
}
func (s *Store) EmptyTrash() (int, error)                              { return 0, db.ErrReadOnly }
func (s *Store) UpsertSession(_ db.Session) error                      { return db.ErrReadOnly }
func (s *Store) ReplaceSessionMessages(_ string, _ []db.Message) error { return db.ErrReadOnly }
func (s *Store) WriteSessionBatchAtomic(
	_ []db.SessionBatchWrite, _ ...func() error,
) (db.SessionBatchResult, error) {
	return db.SessionBatchResult{}, db.ErrReadOnly
}
