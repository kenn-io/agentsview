package clickhouse

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// vectorLookupChunk caps the keys one hydration or unit-resolution query carries.
const vectorLookupChunk = 200

// LookupVectorGeneration resolves a config fingerprint to the dimension of
// the generation the mirror holds for it; found is false when no push
// registered that fingerprint. A mirror whose vector tables do not exist yet
// reports not found rather than an error.
func LookupVectorGeneration(
	ctx context.Context, conn *sql.DB, fingerprint string,
) (int, bool, error) {
	var dim int64
	err := conn.QueryRowContext(ctx,
		`SELECT dimension FROM vector_generations WHERE fingerprint = ?`, fingerprint,
	).Scan(&dim)
	if err == sql.ErrNoRows || isMissingTableError(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("looking up clickhouse vector generation: %w", err)
	}
	return int(dim), true, nil
}

// ListVectorGenerationInfo returns every generation the mirror holds, oldest
// first, for the serve startup notice when none matches the local config.
func ListVectorGenerationInfo(
	ctx context.Context, conn *sql.DB,
) ([]storage.VectorGenerationInfo, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT fingerprint, model, dimension FROM vector_generations ORDER BY created_at, fingerprint`)
	if isMissingTableError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing clickhouse vector generations: %w", err)
	}
	defer rows.Close()
	var gens []storage.VectorGenerationInfo
	for rows.Next() {
		var g storage.VectorGenerationInfo
		var dim int64
		if err := rows.Scan(&g.Fingerprint, &g.Model, &dim); err != nil {
			return nil, fmt.Errorf("scanning clickhouse vector generation: %w", err)
		}
		g.Dimension = int(dim)
		gens = append(gens, g)
	}
	return gens, rows.Err()
}

// vectorSearcher is the ClickHouse-backed db.VectorSearcher: an exact
// cosine ranking over one generation's rows in vector_chunks, doc-level
// rollup, and hydration against vector_documents. It mirrors the PostgreSQL
// searcher's semantics; the ranking is exact (no approximate index), which
// matches the local sqlite-vec backend.
type vectorSearcher struct {
	conn          *sql.DB
	fingerprint   string
	dimension     int
	maxInputChars int
	encode        storage.VectorQueryEncoder
}

// NewVectorSearcher builds a searcher over the generation identified by
// fingerprint. dimension must match the pushed embeddings; maxInputChars is
// the build-time chunk size vector.DocAnchor re-splits content with.
func NewVectorSearcher(
	conn *sql.DB, fingerprint string, dimension, maxInputChars int,
	encode storage.VectorQueryEncoder,
) db.VectorSearcher {
	return &vectorSearcher{
		conn: conn, fingerprint: fingerprint, dimension: dimension,
		maxInputChars: maxInputChars, encode: encode,
	}
}

// SemanticSearch embeds query, ranks the generation's chunks by cosine
// distance, rolls chunks up to one hit per document (best chunk wins, order
// preserved), truncates to limit documents, and hydrates each against
// vector_documents. An encoder failure is wrapped in db.ErrSemanticTransient;
// a document that vanished between ranking and hydration is dropped.
func (v *vectorSearcher) SemanticSearch(
	ctx context.Context, query string, limit int,
) ([]db.VectorHit, error) {
	vec, err := v.encode(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", db.ErrSemanticTransient, err)
	}
	if len(vec) != v.dimension {
		return nil, fmt.Errorf(
			"query embedding has %d dimensions, generation expects %d",
			len(vec), v.dimension)
	}
	chunks, err := v.nearestChunks(ctx, vec, limit)
	if err != nil {
		return nil, err
	}
	docs := kitvec.RollupByDocument(chunks)
	if limit >= 0 && len(docs) > limit {
		docs = docs[:limit]
	}
	if len(docs) == 0 {
		return nil, nil
	}
	return v.hydrateHits(ctx, docs)
}

// chunkHit is one ranked chunk: its document, which chunk matched, and its
// cosine similarity (higher is better).
type chunkHit = kitvec.Hit[string]

// nearestChunks returns up to exactly limit chunks ordered nearest first.
// Fetching exactly limit chunks matches the local searcher's candidate pool
// (backend parity): after rollup the doc pool can be smaller than limit when
// one document contributes several neighbors, and every backend must shrink
// identically. The ORDER BY distance LIMIT shape is the one ClickHouse's
// vector similarity index accelerates, so adding an index later changes no
// query text.
func (v *vectorSearcher) nearestChunks(
	ctx context.Context, vec []float32, limit int,
) ([]chunkHit, error) {
	k := max(limit, 0)
	rows, err := v.conn.QueryContext(ctx, `
		SELECT doc_key, chunk_index, 1 - cosineDistance(embedding, ?) AS score
		FROM vector_chunks
		WHERE generation_fingerprint = ?
		ORDER BY cosineDistance(embedding, ?) ASC, doc_key ASC, chunk_index ASC
		LIMIT ?`, vec, v.fingerprint, vec, k)
	if err != nil {
		return nil, fmt.Errorf("clickhouse chunk ranking: %w", err)
	}
	defer rows.Close()
	var hits []chunkHit
	for rows.Next() {
		var h chunkHit
		var chunkIndex int64
		var score float64
		if err := rows.Scan(&h.Doc, &chunkIndex, &score); err != nil {
			return nil, fmt.Errorf("scanning clickhouse chunk ranking: %w", err)
		}
		h.ChunkIndex = int(chunkIndex)
		h.Score = float32(score)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// vectorDoc is the subset of a vector_documents row needed to hydrate a hit.
type vectorDoc struct {
	sessionID   string
	ordinal     int
	ordinalEnd  int
	subordinate bool
	offsets     []db.UnitOffset
	content     string
}

// hydrateHits looks up each hit's document and builds its db.VectorHit,
// resolving the anchor ordinal and display snippet with vector.DocAnchor.
func (v *vectorSearcher) hydrateHits(
	ctx context.Context, hits []chunkHit,
) ([]db.VectorHit, error) {
	docKeys := make([]string, len(hits))
	for i, h := range hits {
		docKeys[i] = h.Doc
	}
	docs, err := v.lookupDocs(ctx, docKeys)
	if err != nil {
		return nil, err
	}
	out := make([]db.VectorHit, 0, len(hits))
	for _, h := range hits {
		doc, ok := docs[h.Doc]
		if !ok {
			continue
		}
		anchorOrdinal, snippet := vector.DocAnchor(
			doc.content, doc.offsets, doc.ordinal, h.ChunkIndex, v.maxInputChars)
		out = append(out, db.VectorHit{
			SessionID:    doc.sessionID,
			Ordinal:      anchorOrdinal,
			OrdinalStart: doc.ordinal,
			OrdinalEnd:   doc.ordinalEnd,
			Subordinate:  doc.subordinate,
			Score:        h.Score,
			Snippet:      snippet,
		})
	}
	return out, nil
}

func (v *vectorSearcher) lookupDocs(
	ctx context.Context, docKeys []string,
) (map[string]vectorDoc, error) {
	docs := make(map[string]vectorDoc, len(docKeys))
	for chunk := range slices.Chunk(docKeys, vectorLookupChunk) {
		placeholders, args := inArgs(chunk)
		if err := func() error {
			rows, err := v.conn.QueryContext(ctx, `
				SELECT doc_key, session_id, ordinal, ordinal_end, subordinate, offsets, content
				FROM vector_documents WHERE doc_key IN (`+placeholders+`)`, args...)
			if err != nil {
				return fmt.Errorf("looking up clickhouse search hit documents: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var key, offsets string
				var ordinal, ordinalEnd int64
				var doc vectorDoc
				if err := rows.Scan(&key, &doc.sessionID, &ordinal, &ordinalEnd,
					&doc.subordinate, &offsets, &doc.content); err != nil {
					return fmt.Errorf("scanning clickhouse search hit document: %w", err)
				}
				doc.ordinal, doc.ordinalEnd = int(ordinal), int(ordinalEnd)
				if err := json.Unmarshal([]byte(offsets), &doc.offsets); err != nil {
					return fmt.Errorf("parsing offsets for search hit %s: %w", key, err)
				}
				docs[key] = doc
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
	}
	return docs, nil
}

// ResolveMessageUnits maps each ref to the vector_documents unit containing
// it. The result is parallel to refs; a ref with no containing unit yields a
// zero db.UnitRef. Units within a session do not overlap, so containment is
// one range test per ref, batched through an inline refs relation.
func (v *vectorSearcher) ResolveMessageUnits(
	ctx context.Context, refs []db.MessageRef,
) ([]db.UnitRef, error) {
	out := make([]db.UnitRef, len(refs))
	for start := 0; start < len(refs); start += vectorLookupChunk {
		end := min(start+vectorLookupChunk, len(refs))
		if err := v.resolveMessageUnitChunk(ctx, refs, start, end, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (v *vectorSearcher) resolveMessageUnitChunk(
	ctx context.Context, refs []db.MessageRef, start, end int, out []db.UnitRef,
) error {
	parts := make([]string, 0, end-start)
	args := make([]any, 0, (end-start)*3)
	for i := start; i < end; i++ {
		parts = append(parts, "SELECT toInt64(?) AS idx, ? AS session_id, toInt64(?) AS ordinal")
		args = append(args, i, refs[i].SessionID, refs[i].Ordinal)
	}
	rows, err := v.conn.QueryContext(ctx, `
		WITH refs AS (`+strings.Join(parts, " UNION ALL ")+`)
		SELECT r.idx, d.doc_key, d.ordinal, d.ordinal_end, d.subordinate
		FROM refs r
		JOIN vector_documents d ON d.session_id = r.session_id
		WHERE d.ordinal <= r.ordinal AND r.ordinal <= d.ordinal_end`, args...)
	if err != nil {
		return fmt.Errorf("resolving clickhouse message units: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var idx, ordinal, ordinalEnd int64
		var unit db.UnitRef
		if err := rows.Scan(&idx, &unit.DocKey, &ordinal, &ordinalEnd, &unit.Subordinate); err != nil {
			return fmt.Errorf("scanning clickhouse message unit: %w", err)
		}
		unit.SessionID = refs[idx].SessionID
		unit.OrdinalStart, unit.OrdinalEnd = int(ordinal), int(ordinalEnd)
		out[idx] = unit
	}
	return rows.Err()
}

// SetVectorSearcher installs (or, with nil, clears) the semantic search
// backend. Safe to call concurrently with SearchContent and HasSemantic.
func (s *Store) SetVectorSearcher(searcher db.VectorSearcher) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorSearcher = searcher
}

func (s *Store) getVectorSearcher() db.VectorSearcher {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.vectorSearcher
}

// SetSemanticUnavailableReason records why semantic search could not be
// wired, surfaced through db.ErrSemanticUnavailable by SearchContent.
func (s *Store) SetSemanticUnavailableReason(reason string) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.semanticUnavailableReason = reason
}

// HasSemantic reports whether a vector searcher was installed at startup.
func (s *Store) HasSemantic() bool { return s.getVectorSearcher() != nil }

// semanticUnavailableError wraps db.ErrSemanticUnavailable with the recorded
// reason when one is set.
func (s *Store) semanticUnavailableError() error {
	s.vectorMu.RLock()
	reason := s.semanticUnavailableReason
	s.vectorMu.RUnlock()
	if reason == "" {
		return db.NewSemanticUnavailableError(
			"semantic search is not supported by the ClickHouse backend")
	}
	return db.NewSemanticUnavailableError(reason)
}
