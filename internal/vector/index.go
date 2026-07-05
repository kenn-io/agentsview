package vector

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "github.com/mattn/go-sqlite3"
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
// display model name for a generation).
const mirrorDDL = `
CREATE TABLE IF NOT EXISTS vector_messages (
    doc_key      TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    source_uuid  TEXT NOT NULL DEFAULT '',
    ordinal      INTEGER NOT NULL,
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

// Index wraps vectors.db: agentsview's mirror of embeddable message content
// plus kit's sqlitevec store, which owns the generation and vec0 tables
// derived from vectorsPrefix.
type Index struct {
	db       *sql.DB
	store    *sqlitevec.Store[string, string]
	split    kitvec.SplitOptions
	readOnly bool
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

// vectorDSN builds the sqlite3 DSN for vectors.db. The rw path mirrors
// internal/db.Open's pragmas (WAL, busy timeout, NORMAL synchronous); the
// ro path opens with mode=ro and no immutable hint, since vectors.db can be
// concurrently rewritten by another agentsview process.
func vectorDSN(path string, readOnly bool) string {
	params := url.Values{}
	if readOnly {
		params.Set("mode", "ro")
	} else {
		params.Set("_journal_mode", "WAL")
		params.Set("_busy_timeout", "5000")
		params.Set("_synchronous", "NORMAL")
	}
	return path + "?" + params.Encode()
}

// Open opens (creating when rw) vectors.db and the kit sqlitevec store atop
// it. maxInputChars bounds the rune length of a single embedding request via
// SplitOptions{MaxRunes: maxInputChars, Overlap: maxInputChars / 30}.
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

	db, err := sql.Open("sqlite3", vectorDSN(path, readOnly))
	if err != nil {
		return nil, fmt.Errorf("opening vectors.db: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening vectors.db: %w", err)
	}

	if !readOnly {
		if _, err := db.ExecContext(ctx, mirrorDDL); err != nil {
			db.Close()
			return nil, fmt.Errorf("creating vectors.db schema: %w", err)
		}
	}

	store, err := sqlitevec.New[string, string](ctx, db, vectorSchema)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("opening vector store: %w", err)
	}

	overlap := maxInputChars / 30
	return &Index{
		db:       db,
		store:    store,
		split:    kitvec.SplitOptions{MaxRunes: maxInputChars, Overlap: overlap},
		readOnly: readOnly,
	}, nil
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
		return fmt.Errorf("generation %d not found", id)
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
// counts documents stamped for that generation ordinal, Missing counts
// mirror documents that are not.
const generationCoverageQuery = `
SELECT g.ordinal, g.gen_key, g.fingerprint, g.dimension, g.state,
       (SELECT COUNT(*) FROM ` + stampsTable + ` s WHERE s.ordinal = g.ordinal),
       (SELECT COUNT(*) FROM vector_messages d WHERE NOT EXISTS
          (SELECT 1 FROM ` + stampsTable + ` s
           WHERE s.ordinal = g.ordinal AND s.doc_key = d.doc_key))
FROM ` + generationsTable + ` g`

// Generations returns every generation with its coverage counts against the
// current vector_messages mirror, ordered by ordinal.
func (ix *Index) Generations(ctx context.Context) ([]GenerationInfo, error) {
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

// GenerationByID returns the single generation identified by its
// generations-table ordinal.
func (ix *Index) GenerationByID(ctx context.Context, id int64) (GenerationInfo, error) {
	row := ix.db.QueryRowContext(ctx, generationCoverageQuery+` WHERE g.ordinal = ?`, id)
	info, err := ix.scanGenerationInfo(ctx, row)
	if err == sql.ErrNoRows {
		return GenerationInfo{}, fmt.Errorf("generation %d not found", id)
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
