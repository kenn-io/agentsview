package vector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kit/vector/sqlitevec"
)

// ExportChunk is one embedded chunk of a document, decoded to float32s for
// replication to another backend.
type ExportChunk struct {
	ChunkIndex int
	Embedding  []float32
}

// ExportDoc is one embedded mirror document plus its chunk vectors, the
// unit a replica push replicates. OffsetsJSON is the mirror's raw offsets
// column.
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
	Dimension          int
}

// ErrExportNotReady marks a snapshot that contains a rebuild marker or
// incomplete coverage and therefore must not be exported.
var ErrExportNotReady = errors.New("vector export snapshot is not ready")

// exportDocColumns are the mirror columns an export reads alongside the
// document key, in scan order. Content is appended only when the export
// needs the body.
var exportDocColumns = []string{
	"session_id", "source_uuid", "ordinal", "ordinal_end", "subordinate", "offsets", "content_hash",
}

// Export owns the kit snapshot (one SQLite read transaction) for one vector
// push phase. Every read sees the index as it stood when the export began.
type Export struct {
	snap *sqlitevec.Snapshot[string, string]
	gen  ActiveExport
}

// BeginExport opens one snapshot for the requested scope. The rebuild marker
// is read before the snapshot opens; a rebuild that starts afterwards
// invalidates stamps, which the in-snapshot coverage check then reports as
// pending, so the export never claims coverage the vectors do not have.
func (ix *Index) BeginExport(ctx context.Context, scope []string) (*Export, bool, error) {
	gen, ok, err := ix.ActiveExport(ctx)
	if err != nil || !ok {
		return nil, false, err
	}
	value, found, err := ix.metaGet(ctx, activeFullRebuildKey)
	if err != nil {
		return nil, false, fmt.Errorf("reading active full rebuild marker: %w", err)
	}
	if found && value == gen.Fingerprint {
		return nil, false, fmt.Errorf("%w: active generation %q is being rebuilt in place", ErrExportNotReady, gen.Fingerprint)
	}
	snap, err := ix.store.Snapshot(ctx, gen.Fingerprint)
	if err != nil {
		return nil, false, fmt.Errorf("begin export snapshot: %w", err)
	}
	missing, err := missingEmbeddedDocs(ctx, snap, scope)
	if err != nil {
		_ = snap.Close()
		return nil, false, err
	}
	if missing > 0 {
		_ = snap.Close()
		return nil, false, fmt.Errorf("%w: %d document(s) pending", ErrExportNotReady, missing)
	}
	return &Export{snap: snap, gen: gen}, true, nil
}

func (e *Export) Generation() ActiveExport { return e.gen }

// Close releases the snapshot. Calling it again is a no-op.
func (e *Export) Close() error { return e.snap.Close() }

// SessionDocHashes returns, per session, a sha256 aggregate over the
// exported row identity of each doc embedded at its current revision. The
// aggregate covers doc_key, source_uuid, ordinal, ordinal_end, subordinate,
// offsets, and content_hash so a metadata-only change (an ordinal shift on
// unchanged content) still moves it. A nil sessionIDs covers every embedded
// session; non-nil limits the scan to those sessions.
func (e *Export) SessionDocHashes(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	out := make(map[string]string)
	if sessionIDs != nil && len(sessionIDs) == 0 {
		return out, nil
	}
	scan := func(where string, args []any) error {
		rows, err := e.snap.CoveredDocs(ctx, sqlitevec.DocQuery{
			Columns: exportDocColumns,
			Where:   "d.ordinal >= 0" + where,
			Args:    args,
			OrderBy: []string{"session_id", "doc_key"},
		})
		if err != nil {
			return fmt.Errorf("scan embedded doc hashes: %w", err)
		}
		defer func() { _ = rows.Close() }()
		var cur string
		h := sha256.New()
		flush := func() {
			if cur != "" {
				out[cur] = hex.EncodeToString(h.Sum(nil))
				h.Reset()
			}
		}
		for rows.Next() {
			var d ExportDoc
			if err := rows.Scan(&d.DocKey, &d.SessionID, &d.SourceUUID, &d.Ordinal, &d.OrdinalEnd,
				&d.Subordinate, &d.OffsetsJSON, &d.ContentHash); err != nil {
				return fmt.Errorf("scan embedded doc hash row: %w", err)
			}
			if d.SessionID != cur {
				flush()
				cur = d.SessionID
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
		return out, scan("", nil)
	}
	if err := chunkKeys(sessionIDs, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		return scan(" AND d.session_id IN "+placeholders, args)
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// SessionDocs returns sessionID's embedded docs with their chunk vectors,
// plus the aggregate hash of exactly the returned doc set ("" when no docs
// are embedded). Docs, chunks, and hash come from one snapshot, so a build
// rewriting the mirror concurrently cannot yield a doc set whose hash
// claims coverage the chunk reads no longer see.
func (e *Export) SessionDocs(ctx context.Context, sessionID string) ([]ExportDoc, string, error) {
	docs, err := e.sessionDocRows(ctx, sessionID)
	if err != nil {
		return nil, "", err
	}
	for i := range docs {
		chunks, err := e.snap.Chunks(ctx, docs[i].DocKey)
		if err != nil {
			return nil, "", fmt.Errorf("export chunks for %s: %w", docs[i].DocKey, err)
		}
		docs[i].Chunks = make([]ExportChunk, len(chunks))
		for j, c := range chunks {
			docs[i].Chunks[j] = ExportChunk{ChunkIndex: c.ChunkIndex, Embedding: c.Vector}
		}
	}
	return docs, aggregateEmbeddedDocHash(docs), nil
}

// sessionDocRows reads the session's covered documents and closes the row
// set before the caller issues further snapshot reads.
func (e *Export) sessionDocRows(ctx context.Context, sessionID string) ([]ExportDoc, error) {
	rows, err := e.snap.CoveredDocs(ctx, sqlitevec.DocQuery{
		Columns: append(slices.Clone(exportDocColumns), "content"),
		Where:   "d.session_id = ? AND d.ordinal >= 0",
		Args:    []any{sessionID},
		OrderBy: []string{"ordinal"},
	})
	if err != nil {
		return nil, fmt.Errorf("export session docs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var docs []ExportDoc
	for rows.Next() {
		var d ExportDoc
		if err := rows.Scan(&d.DocKey, &d.SessionID, &d.SourceUUID, &d.Ordinal, &d.OrdinalEnd,
			&d.Subordinate, &d.OffsetsJSON, &d.ContentHash, &d.Content); err != nil {
			return nil, fmt.Errorf("scan export doc: %w", err)
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
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
	gens, err := ix.store.Generations(ctx)
	if err != nil {
		return ActiveExport{}, false, fmt.Errorf("lookup active generation: %w", err)
	}
	idx := slices.IndexFunc(gens, func(g sqlitevec.GenerationInfo[string]) bool {
		return g.State == sqlitevec.StateActive
	})
	if idx < 0 {
		return ActiveExport{}, false, nil
	}
	exp := ActiveExport{Fingerprint: gens[idx].Key, Dimension: gens[idx].Dimension}
	model, _, err := ix.metaGet(ctx, "gen_model:"+exp.Fingerprint)
	if err != nil {
		return ActiveExport{}, false, fmt.Errorf("lookup generation model: %w", err)
	}
	exp.Model = model
	return exp, true, nil
}

// missingEmbeddedDocs counts the documents in scope (nil for the whole
// mirror) that the snapshot's generation cannot export: documents the
// generation has not covered at their current revision, plus parked rows
// (negative ordinal) that a refresh has not settled even when stamped.
func missingEmbeddedDocs(ctx context.Context, snap *sqlitevec.Snapshot[string, string], sessionIDs []string) (int64, error) {
	if sessionIDs != nil && len(sessionIDs) == 0 {
		return 0, nil
	}
	count := func(where string, args []any) (int64, error) {
		uncovered, err := snap.UncoveredCount(ctx, strings.TrimPrefix(where, " AND "), args...)
		if err != nil {
			return 0, fmt.Errorf("count generation missing docs: %w", err)
		}
		parked, err := countRows(snap.CoveredDocs(ctx, sqlitevec.DocQuery{
			Where: "d.ordinal < 0" + where, Args: args,
		}))
		if err != nil {
			return 0, fmt.Errorf("count parked embedded docs: %w", err)
		}
		return uncovered + parked, nil
	}
	if sessionIDs == nil {
		return count("", nil)
	}
	var total int64
	if err := chunkKeys(sessionIDs, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		n, err := count(" AND d.session_id IN "+placeholders, args)
		total += n
		return err
	}); err != nil {
		return 0, err
	}
	return total, nil
}

// countRows drains a row set and returns how many rows it held.
func countRows(rows *sql.Rows, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var n int64
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// writeEmbeddedDocIdentity appends one doc's exported row identity to h:
// NUL-terminated fields, a newline per doc. Content and chunk vectors are
// deliberately excluded; the aggregate detects doc-set and metadata drift,
// and content_hash already covers the body. Shared by the full-index
// aggregate scan and the single-session export hash so the two never drift.
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

// aggregateEmbeddedDocHash computes the SessionDocHashes value for one
// exported doc set: the docs hashed in doc_key order. An empty set yields
// "", matching the session's absence from the SessionDocHashes map.
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
