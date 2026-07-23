package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// registerVecOnce guards the process-wide sqlite-vec extension registration
// that sqlitevec.Register performs; calling it more than once would attempt
// to register the same SQL functions twice.
var registerVecOnce sync.Once

// messageMirrorDDL creates agentsview's mirror of embeddable message content
// plus a small key/value table for metadata kit's store does not track (the
// display model name for a generation). ordinal is the unit's first
// (start) ordinal; ordinal_end is its last member's ordinal, equal to
// ordinal for single-message (user) documents. subordinate and offsets
// default to the "no run grouping yet" shape so a row inserted without
// them (see mirror.go) is still valid.
const messageMirrorDDL = `
CREATE TABLE IF NOT EXISTS vector_messages (
    doc_key      TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    source_uuid  TEXT NOT NULL DEFAULT '',
    ordinal      INTEGER NOT NULL,
    ordinal_end  INTEGER NOT NULL,
    subordinate  INTEGER NOT NULL DEFAULT 0,
    offsets      TEXT NOT NULL DEFAULT '[]',
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    embed_gen    TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vector_messages_session_ordinal
    ON vector_messages(session_id, ordinal);
CREATE TABLE IF NOT EXISTS vector_meta (
    key TEXT PRIMARY KEY, value TEXT NOT NULL
);
`

const recallMirrorDDL = `
CREATE TABLE IF NOT EXISTS vector_recall_entries (
    doc_key      TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    source_uuid  TEXT NOT NULL DEFAULT '',
    ordinal      INTEGER NOT NULL,
    ordinal_end  INTEGER NOT NULL,
    subordinate  INTEGER NOT NULL DEFAULT 0,
    offsets      TEXT NOT NULL DEFAULT '[]',
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    embed_gen    TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vector_recall_entries_identity
    ON vector_recall_entries(session_id, ordinal);
CREATE TABLE IF NOT EXISTS vector_recall_meta (
    key TEXT PRIMARY KEY, value TEXT NOT NULL
);
`

// mirrorSchemaVersionKey is the metadata-table key holding a spec's
// MirrorSchemaVersion.
const mirrorSchemaVersionKey = "mirror_schema_version"

// IndexSpec binds an Index to one embedding store inside vectors.db. Every
// table the Index touches derives from the spec — DocsTable and MetaTable
// are the store's own tables, and every kit-managed table starts with
// VectorsPrefix — so two stores can share one database file without reading
// or resetting each other's state. Table names and prefixes of distinct
// specs must not overlap: no spec's VectorsPrefix may be a prefix of
// another spec's table names, since mirrorStateTableNames matches by these
// names when a schema-version mismatch resets the store.
type IndexSpec struct {
	// Name identifies the store for CLI selection and display ("messages").
	Name string
	// SupportsAutomatedScope reports whether IncludeAutomated changes this
	// store's source corpus. It defaults to false so new stores cannot inherit
	// message-specific scope behavior accidentally.
	SupportsAutomatedScope bool
	// DocsTable is the store's mirror documents table.
	DocsTable string
	// MetaTable is the store's own key/value metadata table (schema
	// version, refresh watermark, scope, generation model names). Each
	// store has its own so a reset of one store never clears another's
	// metadata.
	MetaTable string
	// VectorsPrefix names the kit-managed bookkeeping tables
	// (<prefix>_generations, _stamps, _chunks, _v<N>).
	VectorsPrefix string
	// MirrorDDL creates DocsTable, its indexes, and MetaTable
	// (CREATE ... IF NOT EXISTS).
	MirrorDDL string
	// MirrorSchemaVersion versions the mirror's DDL shape and its
	// document-identity scheme; see prepareMirrorSchema.
	MirrorSchemaVersion string
}

// NormalizeIncludeAutomated applies the store's scope capability to a build
// request. Stores whose sources are not session-filtered always use false so
// alternating build entry points cannot churn their stored scope metadata.
func (s IndexSpec) NormalizeIncludeAutomated(value bool) bool {
	return s.SupportsAutomatedScope && value
}

func (s IndexSpec) schema() sqlitevec.Schema {
	return sqlitevec.Schema{
		DocsTable:      s.DocsTable,
		IDColumn:       "doc_key",
		ContentColumn:  "content",
		EmbedGenColumn: "embed_gen",
		RevisionColumn: "content_hash",
		VectorsPrefix:  s.VectorsPrefix,
	}
}

func (s IndexSpec) generationsTable() string { return s.VectorsPrefix + "_generations" }
func (s IndexSpec) stampsTable() string      { return s.VectorsPrefix + "_stamps" }
func (s IndexSpec) chunksTable() string      { return s.VectorsPrefix + "_chunks" }
func (s IndexSpec) repairQueueTable() string { return s.VectorsPrefix + "_repair_queue" }

// MessageIndexSpec is the conversation-message embedding store. Its table
// names, metadata keys, and schema version predate IndexSpec and must stay
// byte-identical so existing vectors.db files open without a reset.
//
// History: "2" added the ordinal_end/subordinate/offsets columns but still
// held one row per message; "3" switched document identity to run-grouped
// units (one row per user message or per run of contiguous assistant
// messages) with no DDL change.
func MessageIndexSpec() IndexSpec {
	return IndexSpec{
		Name:                   "messages",
		SupportsAutomatedScope: true,
		DocsTable:              "vector_messages",
		MetaTable:              "vector_meta",
		VectorsPrefix:          "message_vectors",
		MirrorDDL:              messageMirrorDDL,
		MirrorSchemaVersion:    "3",
	}
}

// RecallIndexSpec is the distilled recall-entry embedding store. It shares
// vectors.db with the message store but owns disjoint mirror, metadata, and
// kit-managed vector tables.
func RecallIndexSpec() IndexSpec {
	return IndexSpec{
		Name:                "recall",
		DocsTable:           "vector_recall_entries",
		MetaTable:           "vector_recall_meta",
		VectorsPrefix:       "recall_vectors",
		MirrorDDL:           recallMirrorDDL,
		MirrorSchemaVersion: "1",
	}
}

// ErrMirrorVersionMismatch reports a vectors.db written by a different
// mirror schema version than this binary expects. Write-path Open calls
// reset the mirror instead of returning this error (see prepareMirrorSchema);
// read-path Open calls (CLI reads, direct-install search) succeed regardless,
// but every subsequent Search call on that Index fails with this sentinel
// until a build recreates the mirror.
var ErrMirrorVersionMismatch = errors.New(
	"vector index was built by an incompatible version: run `agentsview embeddings build`")

// Index wraps vectors.db: agentsview's mirror of embeddable message content
// plus kit's sqlitevec store, which owns the generation and vec0 tables
// derived from spec's VectorsPrefix.
type Index struct {
	db       *sql.DB
	store    *sqlitevec.Store[string, string]
	spec     IndexSpec
	split    kitvec.SplitOptions
	readOnly bool

	// versionMismatch records a read-path Open's version-gate finding: the
	// mirror was written by a different spec.MirrorSchemaVersion. Write-path
	// Opens always resolve the mismatch themselves (reset and restamp), so
	// this is only ever true on a read-only Index. Search checks it before
	// touching any table.
	versionMismatch bool
}

// GenerationInfo describes one embedding generation and its coverage of the
// current mirror, for CLI and status display.
type GenerationInfo struct {
	ID          int64  `json:"id"` // generations-table ordinal, CLI-facing
	State       string `json:"state"`
	Model       string `json:"model"`
	Dimension   int    `json:"dimension"`
	Fingerprint string `json:"fingerprint"`
	Embedded    int64  `json:"embedded"` // stamped docs
	Missing     int64  `json:"missing"`  // mirror docs not stamped
	// Store names the embedding store this generation belongs to (e.g.
	// "messages"). Populated by the CLI after fetching, not by the daemon
	// API, since generation IDs are only unique within a store.
	Store string `json:"store,omitempty"`
}

// ChunkOverlap derives the SplitOptions.Overlap rune count from
// maxInputChars: 15% of the chunk size, so consecutive chunks share enough
// context for the anchor/window logic to bridge a run split mid-message.
// Open and vectorGeneration (cmd/agentsview/embeddings.go) both call this so
// the split behavior and its fingerprint can never drift apart.
func ChunkOverlap(maxInputChars int) int {
	return maxInputChars * 15 / 100
}

// Open opens the message-store index; see OpenSpec.
func Open(ctx context.Context, path string, readOnly bool, maxInputChars int) (*Index, error) {
	return OpenSpec(ctx, path, MessageIndexSpec(), readOnly, maxInputChars)
}

// StoreExists reports whether vectors.db contains any tables owned by spec.
// It probes sqlite_master without constructing sqlitevec's store, because
// sqlitevec initializes missing bookkeeping tables and cannot do that through
// a read-only connection. A partially present store counts as existing so its
// normal open path can surface corruption or schema-version errors.
func StoreExists(
	ctx context.Context, path string, spec IndexSpec,
) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	database, err := sql.Open(vectorDriverName, vectorDSN(path, true))
	if err != nil {
		return false, fmt.Errorf("opening vectors.db store probe: %w", err)
	}
	defer database.Close()
	var exists bool
	err = database.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sqlite_master
			WHERE type = 'table'
			  AND (name IN (?, ?) OR substr(name, 1, ?) = ?)
		)`, spec.DocsTable, spec.MetaTable,
		len(spec.VectorsPrefix), spec.VectorsPrefix,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("probing vectors.db store %s: %w", spec.Name, err)
	}
	return exists, nil
}

// OpenSpec opens (creating when rw) vectors.db and the kit sqlitevec store
// atop it, bound to spec's store. maxInputChars bounds the rune length of a
// single embedding request via SplitOptions{MaxRunes: maxInputChars,
// Overlap: ChunkOverlap(maxInputChars)}.
//
// When readOnly is true the file must already exist; OpenSpec never creates
// or migrates schema in that mode, matching internal/db.OpenReadOnly's
// cold-CLI-read contract.
func OpenSpec(
	ctx context.Context, path string, spec IndexSpec, readOnly bool, maxInputChars int,
) (*Index, error) {
	registerVecOnce.Do(sqlitevec.Register)

	if readOnly {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("opening read-only vectors.db: %w", err)
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating vectors.db directory: %w", err)
	}

	db, err := sql.Open(vectorDriverName, vectorDSN(path, readOnly))
	if err != nil {
		return nil, fmt.Errorf("opening vectors.db: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening vectors.db: %w", err)
	}

	versionMismatch, err := prepareMirrorSchema(ctx, db, spec, readOnly)
	if err != nil {
		db.Close()
		return nil, err
	}

	store, err := sqlitevec.New[string, string](ctx, db, spec.schema())
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("opening vector store: %w", err)
	}
	if !readOnly {
		if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS `+spec.repairQueueTable()+` (
    ordinal INTEGER NOT NULL,
    doc_key TEXT NOT NULL,
    PRIMARY KEY (ordinal, doc_key)
)`); err != nil {
			db.Close()
			return nil, fmt.Errorf("creating invalid vector repair queue: %w", err)
		}
		if _, err := db.ExecContext(ctx,
			`CREATE INDEX IF NOT EXISTS `+spec.repairQueueTable()+`_doc_key ON `+
				spec.repairQueueTable()+` (doc_key)`); err != nil {
			db.Close()
			return nil, fmt.Errorf("indexing invalid vector repair queue by document: %w", err)
		}
	}

	overlap := ChunkOverlap(maxInputChars)
	return &Index{
		db:              db,
		store:           store,
		spec:            spec,
		split:           kitvec.SplitOptions{MaxRunes: maxInputChars, Overlap: overlap},
		readOnly:        readOnly,
		versionMismatch: versionMismatch,
	}, nil
}

// prepareMirrorSchema checks vectors.db's stamped mirror_schema_version
// against spec.MirrorSchemaVersion and, on the write path, brings the schema
// current. On a mismatch — including the version key being absent while any
// mirror-state table already exists — the write path drops every such table
// and recreates the current schema from scratch: vectors.db is disposable by
// design (unlike sessions.db, which is never reset this way), so a clean
// rebuild is simpler and safer than an in-place column migration. On the
// read path a mismatch is reported back as mismatch=true without touching
// any table, leaving stale rows exactly as they are; the caller (Search)
// then fails closed with ErrMirrorVersionMismatch instead of risking a
// misread of old-shaped rows.
func prepareMirrorSchema(ctx context.Context, db *sql.DB, spec IndexSpec, readOnly bool) (mismatch bool, err error) {
	mismatch, tables, err := mirrorVersionMismatch(ctx, db, spec)
	if err != nil {
		return false, err
	}
	if readOnly {
		return mismatch, nil
	}

	if mismatch {
		if err := dropMirrorTables(ctx, db, tables); err != nil {
			return false, err
		}
	}
	if _, err := db.ExecContext(ctx, spec.MirrorDDL); err != nil {
		return false, fmt.Errorf("creating vectors.db schema: %w", err)
	}
	if err := stampMirrorSchemaVersion(ctx, db, spec); err != nil {
		return false, err
	}
	return false, nil
}

// mirrorStateTableNames lists the sqlite_master table names considered part
// of the versioned mirror: the mirror's own tables, plus any kit-owned
// spec.VectorsPrefix* table (the generations and stamps bookkeeping tables
// and one vec0 table per embedding generation, including retired or
// abandoned ones left behind by a prior build). The prefix is matched
// literally via substr, not LIKE: LIKE would treat "_" in a prefix such as
// "message_vectors" as a single-character wildcard, letting one store's
// schema-version reset match — and drop — another store's tables whose
// names differ only at wildcard positions.
func mirrorStateTableNames(ctx context.Context, db *sql.DB, spec IndexSpec) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT name FROM sqlite_master
 WHERE type = 'table'
   AND (name IN (?, ?) OR substr(name, 1, ?) = ?)`,
		spec.DocsTable, spec.MetaTable, len(spec.VectorsPrefix), spec.VectorsPrefix)
	if err != nil {
		return nil, fmt.Errorf("listing mirror tables: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning mirror table name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing mirror tables: %w", err)
	}
	return names, nil
}

// mirrorVersionMismatch reports whether vectors.db's stamped
// mirror_schema_version differs from spec.MirrorSchemaVersion. An
// empty/fresh database (no mirror-state table at all) is never a mismatch —
// there is nothing yet to be incompatible with. Otherwise, a version key
// that is absent, or holds a different value, while any mirror-state table
// already exists is a mismatch: that table predates versioning entirely, or
// was stamped by a different scheme. tables is always returned so a
// write-path caller can drop exactly the set that was inspected.
func mirrorVersionMismatch(ctx context.Context, db *sql.DB, spec IndexSpec) (mismatch bool, tables []string, err error) {
	tables, err = mirrorStateTableNames(ctx, db, spec)
	if err != nil {
		return false, nil, err
	}
	if len(tables) == 0 {
		return false, tables, nil
	}
	if !slices.Contains(tables, spec.MetaTable) {
		return true, tables, nil
	}

	var stamped string
	err = db.QueryRowContext(ctx,
		`SELECT value FROM `+spec.MetaTable+` WHERE key = ?`, mirrorSchemaVersionKey,
	).Scan(&stamped)
	if err == sql.ErrNoRows {
		return true, tables, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("reading mirror schema version: %w", err)
	}
	return stamped != spec.MirrorSchemaVersion, tables, nil
}

// dropMirrorTables drops every named table, resetting vectors.db's mirror
// state ahead of a fresh MirrorDDL + stampMirrorSchemaVersion. Table names
// come from sqlite_master (mirrorStateTableNames), not caller input, so
// building the DROP statement by concatenation is safe; SQL does not allow
// binding identifiers as query parameters.
func dropMirrorTables(ctx context.Context, db *sql.DB, tables []string) error {
	for _, name := range tables {
		if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS "`+name+`"`); err != nil {
			return fmt.Errorf("dropping stale mirror table %s: %w", name, err)
		}
	}
	return nil
}

// stampMirrorSchemaVersion records spec.MirrorSchemaVersion into
// spec.MetaTable, overwriting any prior value.
func stampMirrorSchemaVersion(ctx context.Context, db *sql.DB, spec IndexSpec) error {
	if _, err := db.ExecContext(ctx, `
INSERT INTO `+spec.MetaTable+` (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		mirrorSchemaVersionKey, spec.MirrorSchemaVersion,
	); err != nil {
		return fmt.Errorf("stamping mirror schema version: %w", err)
	}
	return nil
}

// Close closes the underlying vectors.db connection.
func (ix *Index) Close() error {
	return ix.db.Close()
}

// requireWritable rejects generation-mutating calls on an Index opened
// read-only, matching internal/db's read-only guard pattern.
func (ix *Index) requireWritable() error {
	if ix.readOnly {
		return fmt.Errorf("vectors.db is opened read-only")
	}
	return nil
}

// metaGet reads one key from the spec's metadata table; ok is false when
// the key is absent.
func (ix *Index) metaGet(ctx context.Context, key string) (value string, ok bool, err error) {
	err = ix.db.QueryRowContext(ctx,
		`SELECT value FROM `+ix.spec.MetaTable+` WHERE key = ?`, key,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s key %s: %w", ix.spec.MetaTable, key, err)
	}
	return value, true, nil
}

// metaSet upserts one key into the spec's metadata table.
func (ix *Index) metaSet(ctx context.Context, key, value string) error {
	if _, err := ix.db.ExecContext(ctx, `
INSERT INTO `+ix.spec.MetaTable+` (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	); err != nil {
		return fmt.Errorf("writing %s key %s: %w", ix.spec.MetaTable, key, err)
	}
	return nil
}

// metaDelete deletes one key from the spec's metadata table.
func (ix *Index) metaDelete(ctx context.Context, key string) error {
	if _, err := ix.db.ExecContext(ctx,
		`DELETE FROM `+ix.spec.MetaTable+` WHERE key = ?`, key,
	); err != nil {
		return fmt.Errorf("deleting %s key %s: %w", ix.spec.MetaTable, key, err)
	}
	return nil
}

// EnsureGeneration registers gen (a model + dimension configuration) with
// kit's store under its own fingerprint as the gen_key, creating its vec0
// table on first use, and records the model's display name in the spec's
// metadata table so Generations can show it (kit's store persists only the
// fingerprint). Calling it again for the same fingerprint updates only the
// state.
func (ix *Index) EnsureGeneration(
	ctx context.Context, gen kitvec.Generation, state sqlitevec.State,
) (string, error) {
	if err := ix.requireWritable(); err != nil {
		return "", err
	}
	fingerprint := gen.Fingerprint()
	if err := ix.store.EnsureGeneration(ctx, fingerprint, gen, state); err != nil {
		return "", fmt.Errorf("ensure generation: %w", err)
	}
	if err := ix.metaSet(ctx, "gen_model:"+fingerprint, gen.Model); err != nil {
		return "", fmt.Errorf("record generation model: %w", err)
	}
	return fingerprint, nil
}

// SetStateByID transitions the generation identified by its generations-table
// ordinal (the CLI-facing ID) to state.
func (ix *Index) SetStateByID(ctx context.Context, id int64, state sqlitevec.State) error {
	if err := ix.requireWritable(); err != nil {
		return err
	}
	res, err := ix.db.ExecContext(ctx,
		`UPDATE `+ix.spec.generationsTable()+` SET state = ? WHERE ordinal = ?`, string(state), id)
	if err != nil {
		return fmt.Errorf("set generation state: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set generation state rows: %w", err)
	} else if n == 0 {
		return fmt.Errorf("generation %d: %w", id, ErrGenerationNotFound)
	}
	return nil
}

// ActiveFingerprint returns the fingerprint of the generation currently in
// the active state, if any.
func (ix *Index) ActiveFingerprint(ctx context.Context) (string, bool, error) {
	return ix.fingerprintByState(ctx, string(sqlitevec.StateActive))
}

// BuildingFingerprint returns the fingerprint of the generation currently in
// the building state, if any.
func (ix *Index) BuildingFingerprint(ctx context.Context) (string, bool, error) {
	return ix.fingerprintByState(ctx, string(sqlitevec.StateBuilding))
}

func (ix *Index) fingerprintByState(ctx context.Context, state string) (string, bool, error) {
	var fingerprint string
	err := ix.db.QueryRowContext(ctx,
		`SELECT gen_key FROM `+ix.spec.generationsTable()+` WHERE state = ? ORDER BY ordinal LIMIT 1`,
		state).Scan(&fingerprint)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lookup %s generation: %w", state, err)
	}
	return fingerprint, true, nil
}

// generationCoverageQuery is the exact join agentsview uses to report each
// generation's coverage of the current mirror: Embedded counts stamps for
// that generation ordinal whose revision still matches the mirror row's
// current content_hash, Missing counts mirror documents with no such
// matching-revision stamp. A stamp whose revision no longer matches (the
// mirror row's content changed since it was embedded) counts as Missing
// rather than Embedded, since kit's store treats it as pending re-embed.
func (ix *Index) generationCoverageQuery() string {
	return `
SELECT g.ordinal, g.gen_key, g.fingerprint, g.dimension, g.state,
       (SELECT COUNT(*) FROM ` + ix.spec.stampsTable() + ` s WHERE s.ordinal = g.ordinal
          AND EXISTS (SELECT 1 FROM ` + ix.spec.DocsTable + ` d
                      WHERE s.doc_key = d.doc_key AND s.revision = d.content_hash)),
       (SELECT COUNT(*) FROM ` + ix.spec.DocsTable + ` d WHERE NOT EXISTS
          (SELECT 1 FROM ` + ix.spec.stampsTable() + ` s
           WHERE s.ordinal = g.ordinal AND s.doc_key = d.doc_key AND s.revision = d.content_hash))
FROM ` + ix.spec.generationsTable() + ` g`
}

// Generations returns every generation with its coverage counts against the
// current mirror, ordered by ordinal. Like Search and StaleActive it fails
// closed with ErrMirrorVersionMismatch on a read-only Index over a
// mismatched mirror, rather than reporting coverage counts computed over
// stale-shape rows.
func (ix *Index) Generations(ctx context.Context) ([]GenerationInfo, error) {
	if ix.versionMismatch {
		return nil, ErrMirrorVersionMismatch
	}
	rows, err := ix.db.QueryContext(ctx, ix.generationCoverageQuery()+` ORDER BY g.ordinal`)
	if err != nil {
		return nil, fmt.Errorf("list generations: %w", err)
	}
	defer rows.Close()

	var infos []GenerationInfo
	for rows.Next() {
		info, err := ix.scanGenerationInfo(ctx, rows)
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list generations: %w", err)
	}
	return infos, nil
}

// ErrGenerationNotFound is returned by GenerationByID (and propagated by
// Manager.Activate/Retire) when id does not match any row in the
// generations table. Match it with errors.Is; the wrapping error's message
// still carries the specific id for logs and direct display.
var ErrGenerationNotFound = errors.New("generation not found")

// GenerationByID returns the single generation identified by its
// generations-table ordinal.
func (ix *Index) GenerationByID(ctx context.Context, id int64) (GenerationInfo, error) {
	row := ix.db.QueryRowContext(ctx, ix.generationCoverageQuery()+` WHERE g.ordinal = ?`, id)
	info, err := ix.scanGenerationInfo(ctx, row)
	if err == sql.ErrNoRows {
		return GenerationInfo{}, fmt.Errorf("generation %d: %w", id, ErrGenerationNotFound)
	}
	if err != nil {
		return GenerationInfo{}, err
	}
	return info, nil
}

// MissingEmbeddedDocs returns how many current mirror docs are still missing
// from genOrdinal's embedded set. A nil sessionIDs slice counts the whole
// mirror; a non-nil slice limits the count to those sessions, which lets
// change-scoped PG pushes bound the readiness read to their candidate set.
func (ix *Index) MissingEmbeddedDocs(
	ctx context.Context, genOrdinal int64, sessionIDs []string,
) (int64, error) {
	if ix.versionMismatch {
		return 0, ErrMirrorVersionMismatch
	}
	if sessionIDs != nil && len(sessionIDs) == 0 {
		return 0, nil
	}
	query := missingEmbeddedDocsQuery(ix.spec)
	if sessionIDs == nil {
		var missing int64
		if err := ix.db.QueryRowContext(ctx, query, genOrdinal).Scan(&missing); err != nil {
			return 0, fmt.Errorf("count generation missing docs: %w", err)
		}
		return missing, nil
	}
	var total int64
	if err := chunkKeys(sessionIDs, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		var missing int64
		if err := ix.db.QueryRowContext(ctx,
			query+` AND d.session_id IN `+placeholders,
			append([]any{genOrdinal}, args...)...,
		).Scan(&missing); err != nil {
			return fmt.Errorf("count scoped generation missing docs: %w", err)
		}
		total += missing
		return nil
	}); err != nil {
		return 0, err
	}
	return total, nil
}

// genInfoScanner is the subset of *sql.Row / *sql.Rows Scan needs, letting
// scanGenerationInfo serve both Generations (rows) and GenerationByID (row).
type genInfoScanner interface {
	Scan(dest ...any) error
}

func (ix *Index) scanGenerationInfo(ctx context.Context, src genInfoScanner) (GenerationInfo, error) {
	var (
		info   GenerationInfo
		genKey string
	)
	if err := src.Scan(
		&info.ID, &genKey, &info.Fingerprint, &info.Dimension, &info.State,
		&info.Embedded, &info.Missing,
	); err != nil {
		if err == sql.ErrNoRows {
			return GenerationInfo{}, err
		}
		return GenerationInfo{}, fmt.Errorf("scan generation: %w", err)
	}

	model, _, err := ix.metaGet(ctx, "gen_model:"+info.Fingerprint)
	if err != nil {
		return GenerationInfo{}, fmt.Errorf("lookup generation model: %w", err)
	}
	info.Model = model
	return info, nil
}
