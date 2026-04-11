package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

// Compile-time check: *Store satisfies db.Store.
var _ db.Store = (*Store)(nil)

// NewStore opens a PostgreSQL connection using the shared Open()
// helper and returns a read-only Store.
// When allowInsecure is true, non-loopback connections without
// TLS produce a warning instead of failing.
func NewStore(
	pgURL, schema string, allowInsecure bool,
) (*Store, error) {
	pg, err := Open(pgURL, schema, allowInsecure)
	if err != nil {
		return nil, err
	}
	return &Store{pg: pg}, nil
}

// DB returns the underlying *sql.DB for operations that need
// direct access (e.g. schema compatibility checks).
func (s *Store) DB() *sql.DB { return s.pg }

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.pg.Close()
}

// SetCursorSecret sets the HMAC key used for cursor signing.
func (s *Store) SetCursorSecret(secret []byte) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	s.cursorSecret = append([]byte(nil), secret...)
}

// ReadOnly returns true; this is a read-only data source.
func (s *Store) ReadOnly() bool { return true }

// GetSessionVersion returns the message count and a hash of
// updated_at for SSE change detection.
func (s *Store) GetSessionVersion(
	id string,
) (int, int64, bool) {
	var count int
	var updatedAt time.Time
	err := s.pg.QueryRow(
		`SELECT message_count, COALESCE(updated_at, created_at)
		 FROM sessions WHERE id = $1`,
		id,
	).Scan(&count, &updatedAt)
	if err != nil {
		return 0, 0, false
	}
	formatted := FormatISO8601(updatedAt)
	var h int64
	for _, c := range formatted {
		h = h*31 + int64(c)
	}
	return count, h, true
}

// ------------------------------------------------------------
// Write stubs (all return db.ErrReadOnly)
// ------------------------------------------------------------

// StarSession is not supported in read-only mode.
func (s *Store) StarSession(_ string) (bool, error) {
	return false, db.ErrReadOnly
}

// UnstarSession is not supported in read-only mode.
func (s *Store) UnstarSession(_ string) error {
	return db.ErrReadOnly
}

// ListStarredSessionIDs returns an empty slice.
func (s *Store) ListStarredSessionIDs(
	_ context.Context,
) ([]string, error) {
	return []string{}, nil
}

// BulkStarSessions is not supported in read-only mode.
func (s *Store) BulkStarSessions(_ []string) error {
	return db.ErrReadOnly
}

// PinMessage is not supported in read-only mode.
func (s *Store) PinMessage(
	_ string, _ int64, _ *string,
) (int64, error) {
	return 0, db.ErrReadOnly
}

// UnpinMessage is not supported in read-only mode.
func (s *Store) UnpinMessage(_ string, _ int64) error {
	return db.ErrReadOnly
}

// ListPinnedMessages returns an empty slice.
func (s *Store) ListPinnedMessages(
	_ context.Context, _ string, _ string,
) ([]db.PinnedMessage, error) {
	return []db.PinnedMessage{}, nil
}

// InsertInsight is not supported in read-only mode.
func (s *Store) InsertInsight(
	_ db.Insight,
) (int64, error) {
	return 0, db.ErrReadOnly
}

// DeleteInsight is not supported in read-only mode.
func (s *Store) DeleteInsight(_ int64) error {
	return db.ErrReadOnly
}

// ListInsights returns an empty slice.
func (s *Store) ListInsights(
	_ context.Context, _ db.InsightFilter,
) ([]db.Insight, error) {
	return []db.Insight{}, nil
}

// GetInsight returns nil.
func (s *Store) GetInsight(
	_ context.Context, _ int64,
) (*db.Insight, error) {
	return nil, nil
}

// RenameSession is not supported in read-only mode.
func (s *Store) RenameSession(
	_ string, _ *string,
) error {
	return db.ErrReadOnly
}

// SoftDeleteSession is not supported in read-only mode.
func (s *Store) SoftDeleteSession(_ string) error {
	return db.ErrReadOnly
}

// RestoreSession is not supported in read-only mode.
func (s *Store) RestoreSession(_ string) (int64, error) {
	return 0, db.ErrReadOnly
}

// DeleteSessionIfTrashed is not supported in read-only mode.
func (s *Store) DeleteSessionIfTrashed(
	_ string,
) (int64, error) {
	return 0, db.ErrReadOnly
}

// ListTrashedSessions returns an empty slice.
func (s *Store) ListTrashedSessions(
	_ context.Context,
) ([]db.Session, error) {
	return []db.Session{}, nil
}

// EmptyTrash is not supported in read-only mode.
func (s *Store) EmptyTrash() (int, error) {
	return 0, db.ErrReadOnly
}

// UpsertSession is not supported in read-only mode.
func (s *Store) UpsertSession(_ db.Session) error {
	return db.ErrReadOnly
}

// ReplaceSessionMessages is not supported in read-only mode.
func (s *Store) ReplaceSessionMessages(
	_ string, _ []db.Message,
) error {
	return db.ErrReadOnly
}
