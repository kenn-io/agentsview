package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
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

// isInsufficientPrivilege returns true when the role lacks a required
// privilege (PG SQLSTATE 42501) — e.g. a restricted push role that cannot
// create vector tables in a schema provisioned by a privileged role.
func isInsufficientPrivilege(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "42501")
}

// Sync manages push-only sync from local SQLite to a remote
// PostgreSQL database.
type Sync struct {
	pg                     *sql.DB
	local                  *db.DB
	syncState              syncStateStore
	aliasBackfillState     syncStateStore
	machine                string
	schema                 string
	targetFingerprint      string
	syncStateTarget        string
	migrateLegacySyncState bool

	// Project filtering for push scope.
	projects        []string
	excludeProjects []string

	// vectorSource, when set, supplies the local vectors.db active generation
	// pushed as a phase at the end of Push. Nil disables the phase.
	vectorSource VectorPushSource
	// afterVectorGenerationLookup is a scoped-promotion test hook.
	afterVectorGenerationLookup func()
	// afterScopedVectorApply is a scoped-retry test hook.
	afterScopedVectorApply func()

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

func (s *Sync) aliasBackfillSyncStateOrDefault() syncStateStore {
	if s.aliasBackfillState != nil {
		return s.aliasBackfillState
	}
	return s.effectiveSyncState()
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
	// VectorSource, when non-nil, enables the vector push phase, replicating
	// the local vectors.db active generation into PG. Nil skips the phase.
	VectorSource VectorPushSource
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
	if err := ValidateProjectFilters(
		opts.Projects,
		opts.ExcludeProjects,
	); err != nil {
		return nil, err
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
	syncStateScope := pushSyncStateScope(
		opts.SyncStateTarget,
		opts.Projects,
		opts.ExcludeProjects,
	)
	aliasBackfillStateScope := opts.SyncStateTarget
	migrateLegacySyncState := opts.MigrateLegacySyncState &&
		!hasProjectFilter(opts.Projects, opts.ExcludeProjects)

	return &Sync{
		pg:    pg,
		local: local,
		syncState: newScopedSyncStateStore(
			local,
			syncStateScope,
			migrateLegacySyncState,
		),
		aliasBackfillState: newScopedSyncStateStore(
			local,
			aliasBackfillStateScope,
			false,
		),
		machine:                machine,
		schema:                 schema,
		targetFingerprint:      targetFingerprint,
		syncStateTarget:        syncStateScope,
		migrateLegacySyncState: migrateLegacySyncState,
		projects:               opts.Projects,
		excludeProjects:        opts.ExcludeProjects,
		vectorSource:           opts.VectorSource,
	}, nil
}

func hasProjectFilter(projects, excludeProjects []string) bool {
	return len(projects) > 0 || len(excludeProjects) > 0
}

// ValidateProjectFilters rejects ambiguous include/exclude project filters.
func ValidateProjectFilters(projects, excludeProjects []string) error {
	if len(projects) > 0 && len(excludeProjects) > 0 {
		return fmt.Errorf(
			"projects and exclude_projects are mutually exclusive",
		)
	}
	return nil
}

func pushSyncStateScope(
	target string,
	projects, excludeProjects []string,
) string {
	if !hasProjectFilter(projects, excludeProjects) {
		return target
	}

	includeValues := normalizeProjectFilterValues(projects)
	excludeValues := normalizeProjectFilterValues(excludeProjects)

	sum := sha256.New()
	writeSyncScopeField(sum, "target")
	writeSyncScopeField(sum, target)
	writeSyncScopeField(sum, "include")
	writeSyncScopeField(sum, fmt.Sprintf("%d", len(includeValues)))
	for _, value := range includeValues {
		writeSyncScopeField(sum, value)
	}
	writeSyncScopeField(sum, "exclude")
	writeSyncScopeField(sum, fmt.Sprintf("%d", len(excludeValues)))
	for _, value := range excludeValues {
		writeSyncScopeField(sum, value)
	}
	fingerprint := hex.EncodeToString(sum.Sum(nil)[:8])
	if target == "" {
		return "project-filter:" + fingerprint
	}
	return target + ":project-filter:" + fingerprint
}

func normalizeProjectFilterValues(values []string) []string {
	out := append([]string{}, values...)
	sort.Strings(out)
	return slicesCompact(out)
}

func slicesCompact(values []string) []string {
	if len(values) == 0 {
		return values
	}
	write := 1
	for _, value := range values[1:] {
		if value == values[write-1] {
			continue
		}
		values[write] = value
		write++
	}
	return values[:write]
}

func writeSyncScopeField(sum interface{ Write([]byte) (int, error) }, value string) {
	_, _ = fmt.Fprintf(sum, "%d:", len(value))
	_, _ = sum.Write([]byte(value))
}

// isFiltered reports whether push scope is restricted by
// project include/exclude filters.
func (s *Sync) isFiltered() bool {
	return hasProjectFilter(s.projects, s.excludeProjects)
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
	return s.ensureSchemaLocked(ctx)
}

func (s *Sync) ensureSchemaLocked(ctx context.Context) error {
	if s.schemaDone {
		return nil
	}
	if err := CheckDataVersionCompat(ctx, s.pg); err != nil {
		return err
	}
	if pushSchemaCurrent(ctx, s.pg) {
		// Schema DDL is current, so skip the index and column
		// maintenance that can lock against concurrent pg serve
		// reads (issue #887). Still run the row-level data repairs
		// so is_automated and token-coverage flags stay correct on
		// existing rows.
		if err := runSchemaDataRepairsPG(ctx, s.pg); err != nil {
			return err
		}
		// pushSchemaCurrent predates the vector tables, so a schema
		// created before this feature is "current" yet lacks them; a
		// plain post-upgrade pg push would never create them. Run the
		// same best-effort vector setup the full EnsureSchema path does
		// (schema.go), once per Sync via the schemaDone memo. Failure
		// leaves semantic search unavailable but must not fail the push.
		if _, err := ensureVectorBaseSchemaPG(ctx, s.pg); err != nil {
			log.Printf("pg schema: vector schema setup failed: %v", err)
		}
		s.schemaDone = true
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
		s.local, s.syncStateTarget, nil, nil,
		s.migrateLegacySyncState,
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
	projects, excludeProjects []string,
	migrateLegacy bool,
) (string, error) {
	if local == nil {
		return "", fmt.Errorf("local sync state is required")
	}
	scope := pushSyncStateScope(target, projects, excludeProjects)
	if scope == "" {
		return local.GetSyncState("last_push_at")
	}
	store := newScopedSyncStateStore(
		local,
		scope,
		false,
	)
	lastPush, err := store.GetSyncState("last_push_at")
	if err != nil {
		return "", err
	}
	if lastPush != "" ||
		!migrateLegacy ||
		hasProjectFilter(projects, excludeProjects) {
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
