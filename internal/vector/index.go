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

// vectorsPrefix names the vec0/bookkeeping tables kit's sqlitevec store
// manages for our documents table (message_vectors_generations,
// message_vectors_chunks, message_vectors_stamps, message_vectors_v<N>).
const vectorsPrefix = "message_vectors"

// vectorSchema binds kit's sqlitevec store to our vector_messages mirror
// table.
var vectorSchema = sqlitevec.Schema{
	DocsTable:      "vector_messages",
	IDColumn:       "doc_key",
	ContentColumn:  "content",
	EmbedGenColumn: "embed_gen",
	RevisionColumn: "content_hash",
	VectorsPrefix:  vectorsPrefix,
}

// generationsTable is the kit-managed table holding one row per embedding
// generation (ordinal, gen_key, fingerprint, dimension, state).
const generationsTable = vectorsPrefix + "_generations"

// stampsTable is the kit-managed table recording which documents are
// embedded under which generation ordinal.
const stampsTable = vectorsPrefix + "_stamps"

// mirrorDDL creates agentsview's mirror of embeddable message content plus
// a small key/value table for metadata kit's store does not track (the
// display model name for a generation). ordinal is the unit's first
// (start) ordinal; ordinal_end is its last member's ordinal, equal to
// ordinal for single-message (user) documents. subordinate and offsets
// default to the "no run grouping yet" shape so a row inserted without
// them (see mirror.go) is still valid.
const mirrorDDL = `
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

// mirrorSchemaVersion is the current vectors.db mirror schema generation,
// stamped into vector_meta under mirrorSchemaVersionKey. It covers both the
// mirror's DDL shape (mirrorDDL's column set) AND its document-identity
// scheme (what one vector_messages row means); bump it whenever either
// changes in a way old rows cannot simply be read as-is. Open resets
// vectors.db on the write path, and flags ErrMirrorVersionMismatch on the
// read path, whenever the stamped value differs (or is absent while any
// mirror state already exists).
//
// History: "2" added the ordinal_end/subordinate/offsets columns but still
// held one row per message; "3" switched document identity to run-grouped
// units (one row per user message or per run of contiguous assistant
// messages) with no DDL change.
const mirrorSchemaVersion = "3"

// mirrorSchemaVersionKey is the vector_meta key holding mirrorSchemaVersion.
const mirrorSchemaVersionKey = "mirror_schema_version"

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
// derived from vectorsPrefix.
type Index struct {
	db       *sql.DB
	store    *sqlitevec.Store[string, string]
	split    kitvec.SplitOptions
	readOnly bool

	// versionMismatch records a read-path Open's version-gate finding: the
	// mirror was written by a different mirrorSchemaVersion. Write-path
	// Opens always resolve the mismatch themselves (reset and restamp), so
	// this is only ever true on a read-only Index. Search checks it before
	// touching any table.
	versionMismatch bool
}

// GenerationInfo describes one embedding generation and its coverage of the
// current vector_messages mirror, for CLI and status display.
type GenerationInfo struct {
	ID          int64  `json:"id"` // generations-table ordinal, CLI-facing
	State       string `json:"state"`
	Model       string `json:"model"`
	Dimension   int    `json:"dimension"`
	Fingerprint string `json:"fingerprint"`
	Embedded    int64  `json:"embedded"` // stamped docs
	Missing     int64  `json:"missing"`  // mirror docs not stamped
}

// ChunkOverlap derives the SplitOptions.Overlap rune count from
// maxInputChars: 15% of the chunk size, so consecutive chunks share enough
// context for the anchor/window logic to bridge a run split mid-message.
// Open and vectorGeneration (cmd/agentsview/embeddings.go) both call this so
// the split behavior and its fingerprint can never drift apart.
func ChunkOverlap(maxInputChars int) int {
	return maxInputChars * 15 / 100
}

// Open opens (creating when rw) vectors.db and the kit sqlitevec store atop
// it. maxInputChars bounds the rune length of a single embedding request via
// SplitOptions{MaxRunes: maxInputChars, Overlap: ChunkOverlap(maxInputChars)}.
//
// When readOnly is true the file must already exist; Open never creates or
// migrates schema in that mode, matching internal/db.OpenReadOnly's
// cold-CLI-read contract.
func Open(ctx context.Context, path string, readOnly bool, maxInputChars int) (*Index, error) {
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

	versionMismatch, err := prepareMirrorSchema(ctx, db, readOnly)
	if err != nil {
		db.Close()
		return nil, err
	}

	store, err := sqlitevec.New[string, string](ctx, db, vectorSchema)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("opening vector store: %w", err)
	}

	overlap := ChunkOverlap(maxInputChars)
	return &Index{
		db:              db,
		store:           store,
		split:           kitvec.SplitOptions{MaxRunes: maxInputChars, Overlap: overlap},
		readOnly:        readOnly,
		versionMismatch: versionMismatch,
	}, nil
}

// prepareMirrorSchema checks vectors.db's stamped mirror_schema_version
// against mirrorSchemaVersion and, on the write path, brings the schema
// current. On a mismatch — including the version key being absent while any
// mirror-state table already exists — the write path drops every such table
// and recreates the current schema from scratch: vectors.db is disposable by
// design (unlike sessions.db, which is never reset this way), so a clean
// rebuild is simpler and safer than an in-place column migration. On the
// read path a mismatch is reported back as mismatch=true without touching
// any table, leaving stale rows exactly as they are; the caller (Search)
// then fails closed with ErrMirrorVersionMismatch instead of risking a
// misread of old-shaped rows.
func prepareMirrorSchema(ctx context.Context, db *sql.DB, readOnly bool) (mismatch bool, err error) {
	mismatch, tables, err := mirrorVersionMismatch(ctx, db)
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
	if _, err := db.ExecContext(ctx, mirrorDDL); err != nil {
		return false, fmt.Errorf("creating vectors.db schema: %w", err)
	}
	if err := stampMirrorSchemaVersion(ctx, db); err != nil {
		return false, err
	}
	return false, nil
}

// mirrorStateTableNames lists the sqlite_master table names considered part
// of the versioned mirror: the mirror's own tables, plus any kit-owned
// message_vectors* table (the generations and stamps bookkeeping tables and
// one vec0 table per embedding generation, including retired or abandoned
// ones left behind by a prior build).
func mirrorStateTableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT name FROM sqlite_master
 WHERE type = 'table'
   AND (name IN ('vector_messages', 'vector_meta') OR name LIKE ?)`,
		vectorsPrefix+"%")
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
// mirror_schema_version differs from mirrorSchemaVersion. An empty/fresh
// database (no mirror-state table at all) is never a mismatch — there is
// nothing yet to be incompatible with. Otherwise, a version key that is
// absent, or holds a different value, while any mirror-state table already
// exists is a mismatch: that table predates versioning entirely, or was
// stamped by a different scheme. tables is always returned so a write-path
// caller can drop exactly the set that was inspected.
func mirrorVersionMismatch(ctx context.Context, db *sql.DB) (mismatch bool, tables []string, err error) {
	tables, err = mirrorStateTableNames(ctx, db)
	if err != nil {
		return false, nil, err
	}
	if len(tables) == 0 {
		return false, tables, nil
	}
	if !slices.Contains(tables, "vector_meta") {
		return true, tables, nil
	}

	var stamped string
	err = db.QueryRowContext(ctx,
		`SELECT value FROM vector_meta WHERE key = ?`, mirrorSchemaVersionKey,
	).Scan(&stamped)
	if err == sql.ErrNoRows {
		return true, tables, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("reading mirror schema version: %w", err)
	}
	return stamped != mirrorSchemaVersion, tables, nil
}

// dropMirrorTables drops every named table, resetting vectors.db's mirror
// state ahead of a fresh mirrorDDL + stampMirrorSchemaVersion. Table names
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

// stampMirrorSchemaVersion records mirrorSchemaVersion into vector_meta,
// overwriting any prior value.
func stampMirrorSchemaVersion(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
INSERT INTO vector_meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		mirrorSchemaVersionKey, mirrorSchemaVersion,
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

// EnsureGeneration registers gen (a model + dimension configuration) with
// kit's store under its own fingerprint as the gen_key, creating its vec0
// table on first use, and records the model's display name in vector_meta
// so Generations can show it (kit's store persists only the fingerprint).
// Calling it again for the same fingerprint updates only the state.
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
	if _, err := ix.db.ExecContext(ctx, `
INSERT INTO vector_meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		"gen_model:"+fingerprint, gen.Model); err != nil {
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
		`UPDATE `+generationsTable+` SET state = ? WHERE ordinal = ?`, string(state), id)
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
		`SELECT gen_key FROM `+generationsTable+` WHERE state = ? ORDER BY ordinal LIMIT 1`,
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
// generation's coverage of the current vector_messages mirror: Embedded
// counts stamps for that generation ordinal whose revision still matches
// the mirror row's current content_hash, Missing counts mirror documents
// with no such matching-revision stamp. A stamp whose revision no longer
// matches (the mirror row's content changed since it was embedded) counts
// as Missing rather than Embedded, since kit's store treats it as pending
// re-embed.
const generationCoverageQuery = `
SELECT g.ordinal, g.gen_key, g.fingerprint, g.dimension, g.state,
       (SELECT COUNT(*) FROM ` + stampsTable + ` s WHERE s.ordinal = g.ordinal
          AND EXISTS (SELECT 1 FROM vector_messages d
                      WHERE s.doc_key = d.doc_key AND s.revision = d.content_hash)),
       (SELECT COUNT(*) FROM vector_messages d WHERE NOT EXISTS
          (SELECT 1 FROM ` + stampsTable + ` s
           WHERE s.ordinal = g.ordinal AND s.doc_key = d.doc_key AND s.revision = d.content_hash))
FROM ` + generationsTable + ` g`

// Generations returns every generation with its coverage counts against the
// current vector_messages mirror, ordered by ordinal. Like Search and
// StaleActive it fails closed with ErrMirrorVersionMismatch on a read-only
// Index over a mismatched mirror, rather than reporting coverage counts
// computed over stale-shape rows.
func (ix *Index) Generations(ctx context.Context) ([]GenerationInfo, error) {
	if ix.versionMismatch {
		return nil, ErrMirrorVersionMismatch
	}
	rows, err := ix.db.QueryContext(ctx, generationCoverageQuery+` ORDER BY g.ordinal`)
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
	row := ix.db.QueryRowContext(ctx, generationCoverageQuery+` WHERE g.ordinal = ?`, id)
	info, err := ix.scanGenerationInfo(ctx, row)
	if err == sql.ErrNoRows {
		return GenerationInfo{}, fmt.Errorf("generation %d: %w", id, ErrGenerationNotFound)
	}
	if err != nil {
		return GenerationInfo{}, err
	}
	return info, nil
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

	var model sql.NullString
	err := ix.db.QueryRowContext(ctx,
		`SELECT value FROM vector_meta WHERE key = ?`, "gen_model:"+info.Fingerprint).Scan(&model)
	if err != nil && err != sql.ErrNoRows {
		return GenerationInfo{}, fmt.Errorf("lookup generation model: %w", err)
	}
	info.Model = model.String
	return info, nil
}
