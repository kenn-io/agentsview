package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/wesm/agentsview/internal/db"
)

// isUndefinedTable returns true when the error indicates the
// queried relation does not exist (PG SQLSTATE 42P01). We match
// only the SQLSTATE code to avoid false positives from other
// "does not exist" errors (missing columns, functions, etc.).
func isUndefinedTable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "42P01")
}

// Sync manages push-only sync from local SQLite to a remote
// PostgreSQL database.
type Sync struct {
	pg     *sql.DB
	local  *db.DB
	machine string
	schema  string

	closeOnce sync.Once
	closeErr  error

	schemaMu   sync.Mutex
	schemaDone bool
}

// New creates a Sync instance and verifies the PG connection.
// The machine name must not be "local", which is reserved as the
// SQLite sentinel for sessions that originated on this machine.
// When allowInsecure is true, non-loopback connections without TLS
// produce a warning instead of failing.
func New(
	pgURL, schema string, local *db.DB,
	machine string, allowInsecure bool,
) (*Sync, error) {
	if pgURL == "" {
		return nil, fmt.Errorf("postgres URL is required")
	}
	if machine == "" {
		return nil, fmt.Errorf(
			"machine name must not be empty",
		)
	}
	if machine == "local" {
		return nil, fmt.Errorf(
			"machine name %q is reserved; "+
				"choose a different pg.machine_name",
			machine,
		)
	}
	if local == nil {
		return nil, fmt.Errorf("local db is required")
	}

	pg, err := Open(pgURL, schema, allowInsecure)
	if err != nil {
		return nil, err
	}

	return &Sync{
		pg:      pg,
		local:   local,
		machine: machine,
		schema:  schema,
	}, nil
}

// Close closes the PostgreSQL connection pool.
// Callers must ensure no Push operations are in-flight
// before calling Close; otherwise those operations will fail
// with connection errors.
func (s *Sync) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.pg.Close()
	})
	return s.closeErr
}

// EnsureSchema creates the schema and tables in PG if they
// don't already exist. It also marks the schema as initialized
// so subsequent Push calls skip redundant checks.
func (s *Sync) EnsureSchema(ctx context.Context) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if s.schemaDone {
		return nil
	}
	if err := EnsureSchema(ctx, s.pg, s.schema); err != nil {
		return err
	}
	s.schemaDone = true
	return nil
}

// Status returns sync status information.
// Sync state reads (last_push_at) are non-fatal because these
// are informational watermarks stored in SQLite. PG query
// failures are fatal because they indicate a connectivity
// problem that the caller needs to know about.
func (s *Sync) Status(
	ctx context.Context,
) (SyncStatus, error) {
	lastPush, err := s.local.GetSyncState("last_push_at")
	if err != nil {
		log.Printf(
			"warning: reading last_push_at: %v", err,
		)
		lastPush = ""
	}

	var pgSessions int
	err = s.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions",
	).Scan(&pgSessions)
	if err != nil {
		if isUndefinedTable(err) {
			return SyncStatus{
				Machine:    s.machine,
				LastPushAt: lastPush,
			}, nil
		}
		return SyncStatus{}, fmt.Errorf(
			"counting pg sessions: %w", err,
		)
	}

	var pgMessages int
	err = s.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages",
	).Scan(&pgMessages)
	if err != nil {
		if isUndefinedTable(err) {
			return SyncStatus{
				Machine:    s.machine,
				LastPushAt: lastPush,
				PGSessions: pgSessions,
			}, nil
		}
		return SyncStatus{}, fmt.Errorf(
			"counting pg messages: %w", err,
		)
	}

	return SyncStatus{
		Machine:    s.machine,
		LastPushAt: lastPush,
		PGSessions: pgSessions,
		PGMessages: pgMessages,
	}, nil
}

// SyncStatus holds summary information about the sync state.
type SyncStatus struct {
	Machine    string `json:"machine"`
	LastPushAt string `json:"last_push_at"`
	PGSessions int    `json:"pg_sessions"`
	PGMessages int    `json:"pg_messages"`
}
