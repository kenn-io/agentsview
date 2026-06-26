package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/db"
)

type syncStateStore = SyncStateStore

type scopedSyncStateStore struct {
	base          syncStateStore
	scope         string
	migrateLegacy bool
	migrateOnce   sync.Once
	migrateErr    error
}

func newScopedSyncStateStore(
	base syncStateStore,
	scope string,
	migrateLegacy bool,
) *scopedSyncStateStore {
	return &scopedSyncStateStore{
		base:          base,
		scope:         scope,
		migrateLegacy: migrateLegacy,
	}
}

func (s *scopedSyncStateStore) scopedKey(key string) string {
	if s.scope == "" {
		return key
	}
	return key + ":" + s.scope
}

func (s *scopedSyncStateStore) ensureMigration() error {
	if s.scope == "" || !s.migrateLegacy {
		return nil
	}
	s.migrateOnce.Do(func() {
		for _, key := range []string{
			"last_push_at",
			lastPushBoundaryStateKey,
			lastPushTargetFingerprintKey,
		} {
			scopedKey := s.scopedKey(key)
			scopedValue, err := s.base.GetSyncState(scopedKey)
			if err != nil {
				s.migrateErr = fmt.Errorf(
					"reading %s during PG sync-state migration: %w",
					scopedKey, err,
				)
				return
			}
			legacyValue, err := s.base.GetSyncState(key)
			if err != nil {
				s.migrateErr = fmt.Errorf(
					"reading legacy %s during PG sync-state migration: %w",
					key, err,
				)
				return
			}
			if legacyValue == "" {
				continue
			}
			if scopedValue == "" {
				if err := s.base.SetSyncState(
					scopedKey, legacyValue,
				); err != nil {
					s.migrateErr = fmt.Errorf(
						"writing %s during PG sync-state migration: %w",
						scopedKey, err,
					)
					return
				}
			}
			if err := s.base.SetSyncState(key, ""); err != nil {
				s.migrateErr = fmt.Errorf(
					"clearing legacy %s during PG sync-state migration: %w",
					key, err,
				)
				return
			}
		}
	})
	return s.migrateErr
}

func (s *scopedSyncStateStore) GetSyncState(key string) (string, error) {
	if err := s.ensureMigration(); err != nil {
		return "", err
	}
	return s.base.GetSyncState(s.scopedKey(key))
}

func (s *scopedSyncStateStore) SetSyncState(
	key, value string,
) error {
	if err := s.ensureMigration(); err != nil {
		return err
	}
	return s.base.SetSyncState(s.scopedKey(key), value)
}

func (s *scopedSyncStateStore) GetOrCreateSyncState(
	key, defaultValue string,
) (string, error) {
	if err := s.ensureMigration(); err != nil {
		return "", err
	}
	return s.base.GetOrCreateSyncState(
		s.scopedKey(key), defaultValue,
	)
}

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

// isUndefinedColumn returns true when a query references a column
// that does not exist (PG SQLSTATE 42703).
func isUndefinedColumn(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "42703")
}

// Sync manages push-only sync from local SQLite to a remote
// PostgreSQL database.
type Sync struct {
	pg                     *sql.DB
	local                  *db.DB
	syncState              syncStateStore
	machine                string
	schema                 string
	targetFingerprint      string
	syncStateTarget        string
	migrateLegacySyncState bool

	// Project filtering for push scope.
	projects        []string
	excludeProjects []string

	closeOnce sync.Once
	closeErr  error

	schemaMu   sync.Mutex
	schemaDone bool
}

func (s *Sync) effectiveSyncState() syncStateStore {
	if s.syncState != nil {
		return s.syncState
	}
	return s.local
}

// SyncOptions holds optional configuration for a Sync instance.
type SyncOptions struct {
	// Projects limits push scope to these project names.
	// Mutually exclusive with ExcludeProjects.
	Projects []string
	// ExcludeProjects excludes these project names from push.
	// Mutually exclusive with Projects.
	ExcludeProjects []string
	// SyncStateTarget scopes per-target push watermarks and fingerprints.
	SyncStateTarget string
	// MigrateLegacySyncState moves unsuffixed legacy sync-state keys into the
	// named default target the first time that target runs.
	MigrateLegacySyncState bool
}

// New creates a Sync instance and verifies the PG connection.
// The machine name must not be "local", which is reserved as the
// SQLite sentinel for sessions that originated on this machine.
// When allowInsecure is true, non-loopback connections without TLS
// produce a warning instead of failing.
func New(
	pgURL, schema string, local *db.DB,
	machine string, allowInsecure bool,
	opts SyncOptions,
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
	targetFingerprint, err := pgTargetFingerprint(pgURL, schema)
	if err != nil {
		pg.Close()
		return nil, fmt.Errorf(
			"computing pg target fingerprint: %w", err,
		)
	}

	return &Sync{
		pg:    pg,
		local: local,
		syncState: newScopedSyncStateStore(
			local,
			opts.SyncStateTarget,
			opts.MigrateLegacySyncState,
		),
		machine:                machine,
		schema:                 schema,
		targetFingerprint:      targetFingerprint,
		syncStateTarget:        opts.SyncStateTarget,
		migrateLegacySyncState: opts.MigrateLegacySyncState,
		projects:               opts.Projects,
		excludeProjects:        opts.ExcludeProjects,
	}, nil
}

// isFiltered reports whether push scope is restricted by
// project include/exclude filters.
func (s *Sync) isFiltered() bool {
	return len(s.projects) > 0 || len(s.excludeProjects) > 0
}

// DB returns the underlying PostgreSQL connection pool.
func (s *Sync) DB() *sql.DB { return s.pg }

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
	lastPush, err := ReadLastPushAt(
		s.local, s.syncStateTarget, s.migrateLegacySyncState,
	)
	if err != nil {
		log.Printf(
			"warning: reading last_push_at: %v", err,
		)
		lastPush = ""
	}

	return readStatus(ctx, s.pg, s.machine, lastPush)
}

// ReadStatus reads PostgreSQL status without requiring a local SQLite sync
// handle. Callers pass any local last-push watermark they want displayed.
func ReadStatus(
	ctx context.Context,
	pgURL, schema, machine string,
	allowInsecure bool,
	lastPush string,
) (SyncStatus, error) {
	if machine == "" {
		return SyncStatus{}, fmt.Errorf(
			"machine name must not be empty",
		)
	}
	if machine == "local" {
		return SyncStatus{}, fmt.Errorf(
			"machine name %q is reserved; "+
				"choose a different pg.machine_name",
			machine,
		)
	}
	pg, err := Open(pgURL, schema, allowInsecure)
	if err != nil {
		return SyncStatus{}, err
	}
	defer pg.Close()
	return readStatus(ctx, pg, machine, lastPush)
}

func readStatus(
	ctx context.Context,
	pg *sql.DB,
	machine string,
	lastPush string,
) (SyncStatus, error) {
	var pgSessions int
	err := pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions",
	).Scan(&pgSessions)
	if err != nil {
		if isUndefinedTable(err) {
			return SyncStatus{
				Machine:    machine,
				LastPushAt: lastPush,
			}, nil
		}
		return SyncStatus{}, fmt.Errorf(
			"counting pg sessions: %w", err,
		)
	}

	var pgMessages int
	err = pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages",
	).Scan(&pgMessages)
	if err != nil {
		if isUndefinedTable(err) {
			return SyncStatus{
				Machine:    machine,
				LastPushAt: lastPush,
				PGSessions: pgSessions,
			}, nil
		}
		return SyncStatus{}, fmt.Errorf(
			"counting pg messages: %w", err,
		)
	}

	return SyncStatus{
		Machine:    machine,
		LastPushAt: lastPush,
		PGSessions: pgSessions,
		PGMessages: pgMessages,
	}, nil
}

func ReadLastPushAt(
	local SyncStateStore,
	target string,
	migrateLegacy bool,
) (string, error) {
	if local == nil {
		return "", fmt.Errorf("local sync state is required")
	}
	if target == "" {
		return local.GetSyncState("last_push_at")
	}
	store := newScopedSyncStateStore(
		local,
		target,
		false,
	)
	lastPush, err := store.GetSyncState("last_push_at")
	if err != nil {
		return "", err
	}
	if lastPush != "" || !migrateLegacy {
		return lastPush, nil
	}
	return local.GetSyncState("last_push_at")
}

// SyncStatus holds summary information about the sync state.
type SyncStatus struct {
	Machine    string `json:"machine"`
	LastPushAt string `json:"last_push_at"`
	PGSessions int    `json:"pg_sessions"`
	PGMessages int    `json:"pg_messages"`
}
