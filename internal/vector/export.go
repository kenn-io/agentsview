package vector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"math"
	"slices"
	"strconv"
	"strings"
)

// ExportChunk is one embedded chunk of a document, decoded from the
// sqlite-vec blob back into float32s for replication to another backend.
type ExportChunk struct {
	ChunkIndex int
	Embedding  []float32
}

// ExportDoc is one embedded mirror document plus its chunk vectors, the
// unit pg push replicates. OffsetsJSON is the mirror's raw offsets column.
type ExportDoc struct {
	DocKey, SessionID, SourceUUID     string
	Ordinal, OrdinalEnd               int
	Subordinate                       bool
	OffsetsJSON, Content, ContentHash string
	Chunks                            []ExportChunk
}

// ActiveExport identifies the active generation for replication.
type ActiveExport struct {
	Fingerprint, Model string
	Ordinal            int64
	Dimension          int
}

// ActiveExport returns the active generation's identity, or ok=false when
// no generation is active (nothing to push). Like Search, it fails closed
// with ErrMirrorVersionMismatch before touching any table when ix was
// opened read-only against a mirror whose schema version does not match
// this binary's, rather than exporting rows shaped by a different schema.
func (ix *Index) ActiveExport(ctx context.Context) (ActiveExport, bool, error) {
	if ix.versionMismatch {
		return ActiveExport{}, false, ErrMirrorVersionMismatch
	}
	var exp ActiveExport
	err := ix.db.QueryRowContext(ctx,
		`SELECT ordinal, gen_key, dimension FROM `+ix.spec.generationsTable()+
			` WHERE state = 'active' ORDER BY ordinal LIMIT 1`,
	).Scan(&exp.Ordinal, &exp.Fingerprint, &exp.Dimension)
	if err == sql.ErrNoRows {
		return ActiveExport{}, false, nil
	}
	if err != nil {
		return ActiveExport{}, false, fmt.Errorf("lookup active generation: %w", err)
	}
	model, _, err := ix.metaGet(ctx, "gen_model:"+exp.Fingerprint)
	if err != nil {
		return ActiveExport{}, false, fmt.Errorf("lookup generation model: %w", err)
	}
	exp.Model = model
	return exp, true, nil
}

// SessionEmbeddedDocHashes returns, per session, a sha256 aggregate over the
// full exported row identity of each doc embedded at its current revision in
// genOrdinal, ordered by (doc_key). The aggregate covers doc_key, source_uuid,
// ordinal, ordinal_end, subordinate, offsets, and content_hash so that a
// metadata-only change (an ordinal shift on unchanged content, the
// compaction/resync case) still moves the aggregate; hashing only
// (doc_key, content_hash) would leave PG anchors stale. pg push compares these
// against the aggregates stored in PG to skip unchanged sessions.
//
// Like Search, it fails closed with ErrMirrorVersionMismatch before touching
// any table when ix was opened read-only against a mirror whose schema version
// does not match this binary's.
//
// A nil sessionIDs covers every embedded session; non-nil limits the scan to
// those sessions (empty returns an empty map without a scan), so change-scoped
// pushes read hashes proportional to their changed set.
func (ix *Index) SessionEmbeddedDocHashes(
	ctx context.Context, genOrdinal int64, sessionIDs []string,
) (map[string]string, error) {
	if ix.versionMismatch {
		return nil, ErrMirrorVersionMismatch
	}
	out := make(map[string]string)
	if sessionIDs != nil && len(sessionIDs) == 0 {
		return out, nil
	}
	scan := func(where string, args []any) error {
		rows, err := ix.db.QueryContext(ctx, `
SELECT d.session_id, d.doc_key, d.source_uuid, d.ordinal, d.ordinal_end,
       d.subordinate, d.offsets, d.content_hash
  FROM `+ix.spec.DocsTable+` d
  JOIN `+ix.spec.stampsTable()+` st ON st.doc_key = d.doc_key
 WHERE st.ordinal = ? AND st.revision = d.content_hash AND d.ordinal >= 0`+
			where+`
 ORDER BY d.session_id, d.doc_key`, args...)
		if err != nil {
			return fmt.Errorf("scan embedded doc hashes: %w", err)
		}
		defer rows.Close()

		var cur string
		h := sha256.New()
		flush := func() {
			if cur != "" {
				out[cur] = hex.EncodeToString(h.Sum(nil))
				h.Reset()
			}
		}
		for rows.Next() {
			var sessionID string
			var d ExportDoc
			if err := rows.Scan(&sessionID, &d.DocKey, &d.SourceUUID,
				&d.Ordinal, &d.OrdinalEnd, &d.Subordinate, &d.OffsetsJSON,
				&d.ContentHash); err != nil {
				return fmt.Errorf("scan embedded doc hash row: %w", err)
			}
			if sessionID != cur {
				flush()
				cur = sessionID
			}
			writeEmbeddedDocIdentity(h, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		flush()
		return nil
	}
	if sessionIDs == nil {
		if err := scan("", []any{genOrdinal}); err != nil {
			return nil, err
		}
		return out, nil
	}
	// Chunk the ID filter to stay under SQLite's bind-variable limit;
	// sessions never span chunks because each chunk filters whole IDs.
	if err := chunkKeys(sessionIDs, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		return scan(
			" AND d.session_id IN "+placeholders,
			append([]any{genOrdinal}, args...),
		)
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// writeEmbeddedDocIdentity appends one doc's exported row identity to h using
// the framing SessionEmbeddedDocHashes established: NUL-terminated fields, a
// newline per doc. Content and chunk vectors are deliberately excluded — the
// aggregate detects doc-set and metadata drift, and content_hash already
// covers the body. Shared by the full-index aggregate scan and the
// single-session export hash so the two can never drift.
func writeEmbeddedDocIdentity(h hash.Hash, d ExportDoc) {
	writeField := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	writeField(d.DocKey)
	writeField(d.SourceUUID)
	writeField(strconv.Itoa(d.Ordinal))
	writeField(strconv.Itoa(d.OrdinalEnd))
	writeField(strconv.FormatBool(d.Subordinate))
	writeField(d.OffsetsJSON)
	writeField(d.ContentHash)
	h.Write([]byte{'\n'})
}

// aggregateEmbeddedDocHash computes the SessionEmbeddedDocHashes value for one
// exported doc set: the docs hashed in doc_key order. An empty set yields "",
// matching the session's absence from the SessionEmbeddedDocHashes map.
func aggregateEmbeddedDocHash(docs []ExportDoc) string {
	if len(docs) == 0 {
		return ""
	}
	ordered := slices.Clone(docs)
	slices.SortFunc(ordered, func(a, b ExportDoc) int {
		return strings.Compare(a.DocKey, b.DocKey)
	})
	h := sha256.New()
	for _, d := range ordered {
		writeEmbeddedDocIdentity(h, d)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ExportSessionDocs returns sessionID's embedded docs in genOrdinal with
// their chunk vectors decoded to float32, plus the aggregate hash of exactly
// the returned doc set (the SessionEmbeddedDocHashes formula; "" when no docs
// are embedded). Docs, chunks, and hash are read inside one SQLite read
// transaction, so an embeddings build rewriting the mirror concurrently
// cannot yield a doc set whose hash claims coverage the chunk reads no
// longer see — pg push compares the returned hash against its delta-scan
// hash and defers the session on any divergence. Like Search, it fails
// closed with ErrMirrorVersionMismatch before touching any table when ix was
// opened read-only against a mirror whose schema version does not match this
// binary's.
func (ix *Index) ExportSessionDocs(
	ctx context.Context, genOrdinal int64, sessionID string,
) ([]ExportDoc, string, error) {
	if ix.versionMismatch {
		return nil, "", ErrMirrorVersionMismatch
	}
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("begin export snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
SELECT d.doc_key, d.session_id, d.source_uuid, d.ordinal, d.ordinal_end,
       d.subordinate, d.offsets, d.content, d.content_hash
  FROM `+ix.spec.DocsTable+` d
  JOIN `+ix.spec.stampsTable()+` st ON st.doc_key = d.doc_key
 WHERE st.ordinal = ? AND st.revision = d.content_hash
   AND d.session_id = ? AND d.ordinal >= 0
 ORDER BY d.ordinal`, genOrdinal, sessionID)
	if err != nil {
		return nil, "", fmt.Errorf("export session docs: %w", err)
	}
	defer rows.Close()

	var docs []ExportDoc
	for rows.Next() {
		var d ExportDoc
		if err := rows.Scan(&d.DocKey, &d.SessionID, &d.SourceUUID,
			&d.Ordinal, &d.OrdinalEnd, &d.Subordinate,
			&d.OffsetsJSON, &d.Content, &d.ContentHash); err != nil {
			return nil, "", fmt.Errorf("scan export doc: %w", err)
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}

	vecTable := fmt.Sprintf("%s_v%d", ix.spec.VectorsPrefix, genOrdinal)
	for i := range docs {
		chunks, err := exportDocChunks(
			ctx, tx, ix.spec, vecTable, genOrdinal, docs[i].DocKey,
		)
		if err != nil {
			return nil, "", err
		}
		docs[i].Chunks = chunks
	}
	return docs, aggregateEmbeddedDocHash(docs), nil
}

func exportDocChunks(
	ctx context.Context, tx *sql.Tx, spec IndexSpec,
	vecTable string, genOrdinal int64, docKey string,
) ([]ExportChunk, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT c.chunk_index, v.embedding
  FROM `+spec.chunksTable()+` c
  JOIN `+vecTable+` v ON v.rowid = c.vec_rowid
 WHERE c.ordinal = ? AND c.doc_key = ?
 ORDER BY c.chunk_index`, genOrdinal, docKey)
	if err != nil {
		return nil, fmt.Errorf("export chunks for %s: %w", docKey, err)
	}
	defer rows.Close()

	var chunks []ExportChunk
	for rows.Next() {
		var idx int
		var blob []byte
		if err := rows.Scan(&idx, &blob); err != nil {
			return nil, fmt.Errorf("scan chunk for %s: %w", docKey, err)
		}
		vec, err := decodeFloat32Blob(blob)
		if err != nil {
			return nil, fmt.Errorf("decode chunk %d of %s: %w", idx, docKey, err)
		}
		chunks = append(chunks, ExportChunk{ChunkIndex: idx, Embedding: vec})
	}
	return chunks, rows.Err()
}

// decodeFloat32Blob decodes sqlite-vec's raw little-endian float32 blob. A
// nil or empty blob decodes to an empty (non-nil) slice; the guard also proves
// b is non-nil to NilAway before the slice expression below.
func decodeFloat32Blob(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("embedding blob length %d not a multiple of 4", len(b))
	}
	if len(b) == 0 {
		return []float32{}, nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}
