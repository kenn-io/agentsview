package duckdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/uptrace/bun"
	commondb "go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/duckdb/bundialect"
)

// SchemaVersion is the version of the DuckDB mirror schema created by
// createSchema. The mirror schema is create-only: there are no in-place
// migrations between versions. A version mismatch means the mirror file
// must be rebuilt with 'agentsview duckdb push --full'. v10 adds the usage
// accounting rebuild boundary and moves common table creation to the canonical
// Bun registry on top of schema v9's session launch and prompt provenance
// columns. v11 adds an opaque mirror generation token for coherent multi-query
// Quack reads. v12 rebuilds pricing timestamps written before canonical
// PostgreSQL-compatible microsecond normalization.
const SchemaVersion = 12

const schemaVersionMetadataKey = "agentsview_schema_version"

// Mirror metadata keys recorded by writeMirrorMetadata and read back by
// readMirrorMetadata / ProbeMirror.
const (
	dataVersionMetadataKey      = "agentsview_source_data_version"
	sourceDatabaseIDMetadataKey = "agentsview_source_database_id"
	sourceArchiveIDMetadataKey  = "agentsview_source_archive_id"
	pushScopeMetadataKey        = "agentsview_push_scope"
	lastPushAtMetadataKey       = "agentsview_last_push_at"
	lastPushMachineMetadataKey  = "agentsview_last_push_machine"
	lastPushCutoffMetadataKey   = "agentsview_last_push_cutoff"
	deletionRevisionMetadataKey = "agentsview_session_deletion_revision"
	identityRevisionMetadataKey = "agentsview_project_identity_revision"
	mappingRevisionMetadataKey  = "agentsview_worktree_mapping_revision"
	mirrorGenerationMetadataKey = "agentsview_mirror_generation"
)

// curationFingerprintMetadataKey stores a hash of the local in-scope
// curation state (starred session ids, pinned message ids) as of the last
// push that actually refreshed starred_sessions/pinned_messages. It is read
// and written directly via readMetadataKey/recordMetadataKey rather than
// through mirrorMetadata/writeMirrorMetadata: unlike the fields in that
// struct, it is not part of the rebuild-vs-incremental decision (see
// rebuildReason in probe.go), only of the incremental curation-refresh
// skip (see refreshCurationIfChanged in push.go).
const curationFingerprintMetadataKey = "agentsview_curation_fingerprint"

// cursorUsageMaxIDMetadataKey stores the largest local cursor_usage_events
// id the mirror has consumed. The local table is append-only with a
// monotonic integer primary key, so this high-water mark lets every push
// load only appended rows instead of the full history (see
// syncCursorUsageEvents in push.go). Like the curation fingerprint, it is
// read and written directly and plays no part in the rebuild-vs-incremental
// decision; a fresh rebuild file has no metadata, so its first sync loads
// the full history.
const cursorUsageMaxIDMetadataKey = "agentsview_cursor_usage_max_id"

// DuckDB schema notes:
//
//   - DuckDB stores timestamps as TIMESTAMP for mirror tables; read queries
//     should cast/format them to text when scanning into db.Session/db.Message.
//   - DuckDB BOOLEAN columns scan into Go bools directly, unlike SQLite's
//     integer booleans.
//   - SQLite INTEGER PRIMARY KEY rowids are mirrored as BIGINT values because
//     DuckDB does not provide SQLite rowid/autoincrement semantics.
//   - DuckDB does not support SQLite FTS5 or PostgreSQL GIN indexes here; text
//     search optimization is handled separately from this compatibility schema.
//   - DuckDB Quack currently rejects catalogs with TIMESTAMP DEFAULT
//     current_timestamp columns, so mirror timestamp columns avoid dynamic
//     defaults and writers supply current_timestamp explicitly where needed.

const syncMetadataDDL = `CREATE TABLE IF NOT EXISTS sync_metadata (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
)`

func expectedMirrorColumns() map[string][]string {
	expected := map[string][]string{
		"sync_metadata": {"key", "value"},
	}
	for _, table := range bunmodel.CommonTables() {
		expected[table.Name] = bunmodel.ModelColumns(table.Model)
	}
	expected["sessions"] = append(
		expected["sessions"], "agentsview_push_fingerprint",
	)
	return expected
}

// EnsureSchema creates the DuckDB mirror schema. It has no production
// callers: Sync.Push always goes through ProbeMirror to pick rebuildMirror
// (create-only, via createSchema) or incrementalPush against an
// already-valid mirror, and 'duckdb serve'/'duckdb quack serve' probe
// instead of migrating (see ProbeMirror, WatchMirrorReplacement). It is
// kept exported as a convenient fixture builder for tests that need a
// fresh, schema-compatible, empty mirror file to seed with raw INSERTs.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	return createSchema(ctx, db)
}

// createSchema creates the DuckDB mirror schema on a fresh file. Mirror
// schema v10 has no in-place migrations: an existing file whose shape or
// version does not match is rejected by CheckSchemaCompat and must be
// rebuilt with 'agentsview duckdb push --full' rather than patched here.
func createSchema(ctx context.Context, db *sql.DB) error {
	store := bun.NewDB(db, bundialect.New())
	if _, err := store.NewRaw(syncMetadataDDL).Exec(ctx); err != nil {
		return fmt.Errorf("creating duckdb table sync_metadata: %w", err)
	}
	if err := commondb.CreateCommonSchema(ctx, store); err != nil {
		return err
	}
	if _, err := store.NewRaw(
		`ALTER TABLE sessions ADD COLUMN IF NOT EXISTS agentsview_push_fingerprint TEXT`,
	).Exec(ctx); err != nil {
		return fmt.Errorf("creating DuckDB session fingerprint extension: %w", err)
	}
	for _, stmt := range []string{
		"CREATE INDEX IF NOT EXISTS idx_messages_session_ordinal ON messages(session_id, ordinal)",
		"CREATE INDEX IF NOT EXISTS idx_tool_calls_session_category ON tool_calls(session_id, category)",
		"CREATE INDEX IF NOT EXISTS idx_tool_calls_message ON tool_calls(message_id)",
		"CREATE INDEX IF NOT EXISTS idx_pinned_message ON pinned_messages(message_id)",
	} {
		if _, err := store.NewRaw(stmt).Exec(ctx); err != nil {
			return fmt.Errorf("creating DuckDB serving extension index: %w", err)
		}
	}
	if err := recordMetadataKey(
		ctx, store, schemaVersionMetadataKey, strconv.Itoa(SchemaVersion),
	); err != nil {
		return fmt.Errorf("recording duckdb schema version: %w", err)
	}
	generation, err := readMetadataKey(ctx, store, mirrorGenerationMetadataKey)
	if err != nil {
		return err
	}
	if generation == "" {
		generation, err = newMirrorGenerationToken()
		if err != nil {
			return err
		}
		if err := recordMetadataKey(
			ctx, store, mirrorGenerationMetadataKey, generation,
		); err != nil {
			return fmt.Errorf("recording initial duckdb mirror generation: %w", err)
		}
	}
	return nil
}

type metadataStore interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func recordMetadataKey(
	ctx context.Context, db metadataStore, key string, value string,
) error {
	var existing string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		key,
	).Scan(&existing)
	if err == nil && existing == value {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking duckdb metadata key %s: %w", key, err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sync_metadata (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	); err != nil {
		return fmt.Errorf("recording duckdb metadata key %s: %w", key, err)
	}
	return nil
}

// mirrorMetadata captures the push-scope bookkeeping written to
// sync_metadata by writeMirrorMetadata and read back by readMirrorMetadata /
// ProbeMirror. It records what a mirror file contains (schema/data version,
// push scope) and how it got that way (cutoff, last push time/machine) plus
// the source revisions needed to detect deletions and identity changes that
// happened after the mirror was built.
type mirrorMetadata struct {
	SchemaVersion int
	DataVersion   int
	// SourceDatabaseID is the archive_metadata database_id of the SQLite
	// archive the mirror was built from. It identifies the archive
	// GENERATION, not just the path: a resync builds a fresh archive with a
	// new database_id (see internal/db/orphaned.go, which deliberately does
	// not copy the old id), so a recorded id that no longer matches the
	// local archive means the mirror's cutoff and journal cursors describe a
	// different archive's history and only a full rebuild is sound (see
	// rebuildReason).
	SourceDatabaseID string
	// SourceArchiveID is the stable provenance identity stamped onto mirrored
	// sessions and governance metadata. It may change independently when an
	// archive identity is repaired.
	SourceArchiveID  string
	Scope            string
	LastPushCutoff   string
	LastPushAt       string
	LastPushMachine  string
	DeletionRevision int64
	IdentityRevision int64
	MappingRevision  int64
	MirrorGeneration string
}

// writeMirrorMetadata upserts every mirrorMetadata field into sync_metadata.
func writeMirrorMetadata(ctx context.Context, db *sql.DB, meta mirrorMetadata) error {
	generation, err := newMirrorGenerationToken()
	if err != nil {
		return err
	}
	fields := []struct {
		key   string
		value string
	}{
		{schemaVersionMetadataKey, strconv.Itoa(meta.SchemaVersion)},
		{dataVersionMetadataKey, strconv.Itoa(meta.DataVersion)},
		{sourceDatabaseIDMetadataKey, meta.SourceDatabaseID},
		{sourceArchiveIDMetadataKey, meta.SourceArchiveID},
		{pushScopeMetadataKey, meta.Scope},
		{lastPushCutoffMetadataKey, meta.LastPushCutoff},
		{lastPushAtMetadataKey, meta.LastPushAt},
		{lastPushMachineMetadataKey, meta.LastPushMachine},
		{deletionRevisionMetadataKey, strconv.FormatInt(meta.DeletionRevision, 10)},
		{identityRevisionMetadataKey, strconv.FormatInt(meta.IdentityRevision, 10)},
		{mappingRevisionMetadataKey, strconv.FormatInt(meta.MappingRevision, 10)},
		// Publish the opaque generation last: readers retain the previous token
		// until every descriptive metadata field has been finalized.
		{mirrorGenerationMetadataKey, generation},
	}
	store := bun.NewDB(db, bundialect.New())
	tx, err := store.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb mirror metadata update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, field := range fields {
		if err := recordMetadataKey(ctx, tx, field.key, field.value); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb mirror metadata update: %w", err)
	}
	return nil
}

func newMirrorGenerationToken() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generating duckdb mirror generation: %w", err)
	}
	return hex.EncodeToString(nonce[:]), nil
}

func validateMirrorGeneration(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf(
			"missing or empty required duckdb metadata key %s",
			mirrorGenerationMetadataKey,
		)
	}
	return nil
}

// readMirrorMetadata reads mirrorMetadata back from sync_metadata. Missing
// keys read back as zero values; malformed integer fields are reported as
// errors so callers (ProbeMirror) can surface them as shape issues rather
// than silently treating a corrupt mirror as version 0.
func readMirrorMetadata(ctx context.Context, db *sql.DB) (mirrorMetadata, error) {
	store := bun.NewDB(db, bundialect.New())
	raw := make(map[string]string, 12)
	for _, key := range []string{
		schemaVersionMetadataKey, dataVersionMetadataKey,
		sourceDatabaseIDMetadataKey, sourceArchiveIDMetadataKey,
		pushScopeMetadataKey,
		lastPushCutoffMetadataKey, lastPushAtMetadataKey, lastPushMachineMetadataKey,
		deletionRevisionMetadataKey, identityRevisionMetadataKey,
		mappingRevisionMetadataKey, mirrorGenerationMetadataKey,
	} {
		value, err := readMetadataKey(ctx, store, key)
		if err != nil {
			return mirrorMetadata{}, err
		}
		raw[key] = value
	}
	meta := mirrorMetadata{
		SourceDatabaseID: raw[sourceDatabaseIDMetadataKey],
		SourceArchiveID:  raw[sourceArchiveIDMetadataKey],
		Scope:            raw[pushScopeMetadataKey],
		LastPushCutoff:   raw[lastPushCutoffMetadataKey],
		LastPushAt:       raw[lastPushAtMetadataKey],
		LastPushMachine:  raw[lastPushMachineMetadataKey],
		MirrorGeneration: raw[mirrorGenerationMetadataKey],
	}
	if err := validateMirrorGeneration(meta.MirrorGeneration); err != nil {
		return mirrorMetadata{}, err
	}
	var err error
	if meta.SchemaVersion, err = parseMirrorMetadataInt(
		schemaVersionMetadataKey, raw[schemaVersionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	if meta.DataVersion, err = parseMirrorMetadataInt(
		dataVersionMetadataKey, raw[dataVersionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	if meta.DeletionRevision, err = parseMirrorMetadataInt64(
		deletionRevisionMetadataKey, raw[deletionRevisionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	if meta.IdentityRevision, err = parseMirrorMetadataInt64(
		identityRevisionMetadataKey, raw[identityRevisionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	if meta.MappingRevision, err = parseMirrorMetadataInt64(
		mappingRevisionMetadataKey, raw[mappingRevisionMetadataKey],
	); err != nil {
		return mirrorMetadata{}, err
	}
	return meta, nil
}

func readMetadataKey(ctx context.Context, db metadataStore, key string) (string, error) {
	var value string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`, key,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading duckdb metadata key %s: %w", key, err)
	}
	return value, nil
}

func parseMirrorMetadataInt(key, value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parsing duckdb metadata key %s value %q: %w", key, value, err)
	}
	return parsed, nil
}

func parseMirrorMetadataInt64(key, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing duckdb metadata key %s value %q: %w", key, value, err)
	}
	return parsed, nil
}

// CheckSchemaCompat verifies that the local DuckDB mirror file has the
// required tables, columns, and schema version. It does not mutate the
// database. The mirror schema is create-only, so a mismatch of any kind
// means the mirror must be rebuilt rather than migrated in place.
func CheckSchemaCompat(ctx context.Context, db *sql.DB) error {
	return checkSchemaShapeCompat(ctx, db, localSchema)
}

// CheckSchemaCompatViaQuack verifies schema compatibility of a remote Quack
// server's underlying mirror file.
func CheckSchemaCompatViaQuack(ctx context.Context, db *sql.DB) error {
	return checkSchemaShapeCompat(ctx, db, remoteSchema)
}

// schemaLocation says whether a compat failure is against the local mirror
// file or a remote Quack server, which changes only the missing-table/column
// hint: a remote server's shape is fixed by upgrading and restarting it, but
// its schema *version* is a property of the mirror file it serves, which
// only 'agentsview duckdb push --full' on the owning machine can fix.
type schemaLocation bool

const (
	localSchema  schemaLocation = false
	remoteSchema schemaLocation = true
)

func checkSchemaShapeCompat(
	ctx context.Context, db *sql.DB, location schemaLocation,
) error {
	existing, err := loadColumns(ctx, db)
	if err != nil {
		return err
	}
	var missing []string
	expected := expectedMirrorColumns()
	tables := make([]string, 0, len(expected))
	for table := range expected {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		have, ok := existing[table]
		if !ok || len(have) == 0 {
			missing = append(missing, "missing table "+table)
			continue
		}
		for _, column := range expected[table] {
			if !have[column] {
				missing = append(missing, table+"."+column)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		if location == remoteSchema {
			return fmt.Errorf(
				"duckdb schema incompatible; the DuckDB server is on an "+
					"older AgentsView build; upgrade and restart the DuckDB "+
					"server so it migrates its schema at startup; missing: %s",
				strings.Join(missing, ", "),
			)
		}
		return fmt.Errorf(
			"duckdb schema incompatible; rebuild with 'agentsview duckdb push --full'; missing: %s",
			strings.Join(missing, ", "),
		)
	}

	var version string
	err = db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if location == remoteSchema {
			return fmt.Errorf(
				"duckdb schema incompatible; missing %s in sync_metadata; "+
					"upgrade and restart the DuckDB server so it migrates "+
					"its schema at startup",
				schemaVersionMetadataKey,
			)
		}
		return fmt.Errorf(
			"duckdb schema incompatible; missing %s in sync_metadata",
			schemaVersionMetadataKey,
		)
	}
	if err != nil {
		return fmt.Errorf("checking duckdb schema version: %w", err)
	}
	got, err := strconv.Atoi(version)
	if err != nil {
		return fmt.Errorf(
			"duckdb schema incompatible; invalid schema version %q",
			version,
		)
	}
	if got != SchemaVersion {
		return fmt.Errorf(
			"mirror schema version %d does not match this build's %d; "+
				"rebuild with 'agentsview duckdb push --full'",
			got, SchemaVersion,
		)
	}
	generation, err := readMetadataKey(ctx, db, mirrorGenerationMetadataKey)
	if err != nil {
		return fmt.Errorf("checking duckdb mirror generation: %w", err)
	}
	if err := validateMirrorGeneration(generation); err != nil {
		return fmt.Errorf(
			"duckdb schema incompatible: %w", err,
		)
	}

	return nil
}

func loadColumns(ctx context.Context, db *sql.DB) (map[string]map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT lower(table_name), lower(column_name)
		FROM information_schema.columns
		WHERE table_schema = current_schema()`)
	if err != nil {
		return nil, fmt.Errorf("loading duckdb schema columns: %w", err)
	}
	defer rows.Close()

	columns := make(map[string]map[string]bool)
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, fmt.Errorf("scanning duckdb schema columns: %w", err)
		}
		if columns[table] == nil {
			columns[table] = make(map[string]bool)
		}
		columns[table][column] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb schema columns: %w", err)
	}
	return columns, nil
}
