package duckdb

import "go.kenn.io/agentsview/internal/db"

func (s *Store) UpsertSession(_ db.Session) error                      { return db.ErrReadOnly }
func (s *Store) ReplaceSessionMessages(_ string, _ []db.Message) error { return db.ErrReadOnly }
func (s *Store) WriteSessionBatchAtomic(
	_ []db.SessionBatchWrite, _ ...func() error,
) (db.SessionBatchResult, error) {
	return db.SessionBatchResult{}, db.ErrReadOnly
}
