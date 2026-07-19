package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
)

// QueryEncodeFunc embeds a single query string into the generation's vector
// space. It is the read-side counterpart of the build-time encoder, supplied
// by pg serve when it wires the searcher; a returned error means the
// embeddings endpoint failed for this request (transient), not that semantic
// search is unconfigured.
type QueryEncodeFunc func(ctx context.Context, text string) ([]float32, error)

// vectorSearcher is the PG-backed db.VectorSearcher: chunk-level KNN over one
// generation's pgvector chunk table, doc-level rollup, and hydration against
// the shared vector_documents mirror. It mirrors internal/vector's Index
// searcher semantics (Search + hydrateHits + resolveHit) for the PG backend.
type vectorSearcher struct {
	pg            *sql.DB
	genID         int64
	dimension     int
	maxInputChars int
	encode        QueryEncodeFunc
	chunkTable    string

	// schemaMu guards the lazily resolved, quoted pgvector extension schema.
	// The constructor takes no context, so it is resolved on first query
	// (mirroring vector_push.go's vectorGeneration.halfvecType): both the
	// ::halfvec cast and the `<=>` cosine operator must be schema-qualified,
	// or they fail to resolve when pgvector lives in a schema off search_path.
	schemaMu  sync.Mutex
	extSchema string
}

// NewVectorSearcher builds a PG vector searcher for generation genID. The
// chunk table (vector_chunks_g<genID>) and dimension must match what the push
// phase created; maxInputChars is the build-time chunk size, threaded into
// vector.DocAnchor so anchor/snippet re-splitting matches how chunks were cut.
func NewVectorSearcher(
	pg *sql.DB, genID int64, dimension, maxInputChars int, encode QueryEncodeFunc,
) db.VectorSearcher {
	return &vectorSearcher{
		pg:            pg,
		genID:         genID,
		dimension:     dimension,
		maxInputChars: maxInputChars,
		encode:        encode,
		chunkTable:    vectorChunkTable(genID),
	}
}

// resolveExtSchema returns the quoted schema pgvector's types and operators
// live in, resolving it once from pg_extension. It is cached because the
// pgvector schema cannot change for a live connection pool.
func (v *vectorSearcher) resolveExtSchema(ctx context.Context) (string, error) {
	v.schemaMu.Lock()
	defer v.schemaMu.Unlock()
	if v.extSchema != "" {
		return v.extSchema, nil
	}
	schema, err := vectorExtensionSchema(ctx, v.pg)
	if err != nil {
		return "", fmt.Errorf("resolving pgvector schema: %w", err)
	}
	v.extSchema = schema
	return v.extSchema, nil
}

// SemanticSearch embeds query, runs chunk-level KNN over the generation's
// chunk table, rolls chunks up to one hit per document (best chunk wins,
// score order preserved), truncates to limit documents, and hydrates each
// against vector_documents. It mirrors vector.Index.Search + searcherAdapter:
// an encoder failure is wrapped in db.ErrSemanticTransient; a document that
// vanished (or was parked at a negative-ordinal tombstone) between KNN and
// hydration is dropped rather than erroring.
func (v *vectorSearcher) SemanticSearch(
	ctx context.Context, query string, limit int,
) ([]db.VectorHit, error) {
	vec, err := v.encode(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", db.ErrSemanticTransient, err)
	}
	if len(vec) != v.dimension {
		return nil, fmt.Errorf(
			"query embedding has %d dimensions, generation expects %d",
			len(vec), v.dimension)
	}
	extSchema, err := v.resolveExtSchema(ctx)
	if err != nil {
		return nil, err
	}
	chunks, err := v.knnChunks(ctx, vec, extSchema, limit)
	if err != nil {
		return nil, err
	}
	docs := rollupChunkHits(chunks, limit)
	if len(docs) == 0 {
		return nil, nil
	}
	return v.hydrateHits(ctx, docs)
}

// chunkHit is one chunk-level KNN neighbor: the document it belongs to, which
// chunk matched, and its cosine similarity (1 - cosine distance, higher is
// better).
type chunkHit struct {
	docKey     string
	chunkIndex int
	score      float32
}

// knnChunks runs the chunk-level KNN, returning up to exactly limit neighbors
// ordered best (nearest) first. Fetching exactly `limit` chunks matches the
// local sqlite-vec searcher's candidate pool (kit's queryGenerationSQL also
// runs its KNN with LIMIT k before rollup), per the backend-parity rule: after
// rollup the doc pool can be smaller than `limit` when a high-ranking run doc
// contributes several neighbors, and both backends must shrink identically.
// The query vector binds once as a halfvec-cast parameter; both the cast and
// the cosine `<=>` operator are schema-qualified (::schema.halfvec,
// OPERATOR(schema.<=>)) so they resolve when pgvector lives in a schema off
// the connection's search_path.
//
// Recall contract: results are approximate ANN (HNSW), not the local backend's
// exact brute-force scan — that divergence is the accepted halfvec/HNSW spec
// trade-off. But the candidate pool must not silently cap below k: pgvector's
// default hnsw.ef_search (40) would return only ~40 rows regardless of LIMIT k.
// The KNN therefore runs inside a transaction with hnsw.ef_search set to k
// clamped to [40, 1000] (see tuneHNSWRecall / hnswEfSearch): small-k searches
// keep the stock 40 floor, larger k widens the pool up to pgvector's 1000
// ceiling, and past that ceiling iterative scan keeps filling.
func (v *vectorSearcher) knnChunks(
	ctx context.Context, vec []float32, extSchema string, limit int,
) ([]chunkHit, error) {
	k := max(limit, 0)
	literal, err := halfvecLiteral(vec)
	if err != nil {
		return nil, fmt.Errorf("query embedding: %w", err)
	}
	dist := fmt.Sprintf("embedding OPERATOR(%s.<=>) $1::%s.halfvec", extSchema, extSchema)
	q := fmt.Sprintf(`
SELECT doc_key, chunk_index, 1 - (%s) AS score
  FROM %s
 ORDER BY %s
 LIMIT $2`, dist, v.chunkTable, dist)

	tx, err := v.pg.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin chunk knn tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tuneHNSWRecall(ctx, tx, k); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, q, literal, k)
	if err != nil {
		return nil, fmt.Errorf("chunk knn query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []chunkHit
	for rows.Next() {
		var h chunkHit
		var score float64
		if err := rows.Scan(&h.docKey, &h.chunkIndex, &score); err != nil {
			return nil, fmt.Errorf("scanning chunk knn row: %w", err)
		}
		h.score = float32(score)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating chunk knn rows: %w", err)
	}
	return hits, nil
}

// tuneHNSWRecall raises pgvector's per-scan HNSW candidate pool for the current
// transaction so a KNN with LIMIT k returns k neighbors instead of silently
// capping at the default hnsw.ef_search (40). ef_search is set to k clamped to
// [40, 1000] — the recall knob is never tuned below its default, so small-k
// searches keep stock recall — and never above pgvector's ceiling. k is an int
// derived from the caller's limit and interpolated only after clamping, so the
// SET LOCAL statement carries no unvalidated input. SET LOCAL requires a
// transaction and resets on commit/rollback, so this never leaks into a pooled
// connection.
//
// For k above the 1000 ceiling, ef_search alone cannot fill the pool, so
// hnsw.iterative_scan = 'relaxed_order' (pgvector >= 0.8) lets the scan keep
// going past ef_search. Older pgvector lacks that GUC and would abort the tx on
// an unknown-GUC error, so it is probed first with current_setting(..., true)
// (missing_ok), which yields NULL instead of erroring; a NULL result skips the
// setting and leaves the 1000-row cap in place.
// hnswEfSearch clamps the KNN candidate-pool size k to pgvector's usable
// hnsw.ef_search range [40, 1000]: never below the stock default 40 (so small-k
// searches keep default recall) and never above pgvector's 1000 ceiling (past
// which ef_search alone cannot widen the pool — tuneHNSWRecall falls back to
// iterative scan there).
func hnswEfSearch(k int) int {
	return min(max(k, 40), 1000)
}

func tuneHNSWRecall(ctx context.Context, tx *sql.Tx, k int) error {
	efSearch := hnswEfSearch(k)
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
		return fmt.Errorf("setting hnsw.ef_search: %w", err)
	}
	if k <= 1000 {
		return nil
	}
	var current sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT current_setting('hnsw.iterative_scan', true)`,
	).Scan(&current); err != nil {
		return fmt.Errorf("probing hnsw.iterative_scan: %w", err)
	}
	if !current.Valid {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		"SET LOCAL hnsw.iterative_scan = 'relaxed_order'"); err != nil {
		return fmt.Errorf("setting hnsw.iterative_scan: %w", err)
	}
	return nil
}

// rollupChunkHits collapses chunk hits to one hit per document, keeping the
// first (best) chunk seen per doc_key and preserving order, then truncates to
// limit documents. hits must already be ordered best-first (the KNN query
// orders by distance), so first-seen equals best-scoring — matching kit's
// RollupByDocument intent without a re-sort.
func rollupChunkHits(hits []chunkHit, limit int) []chunkHit {
	seen := make(map[string]struct{}, len(hits))
	out := make([]chunkHit, 0, len(hits))
	for _, h := range hits {
		if _, ok := seen[h.docKey]; ok {
			continue
		}
		seen[h.docKey] = struct{}{}
		out = append(out, h)
	}
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// vectorDoc is the subset of a vector_documents row needed to hydrate a chunk
// hit into a db.VectorHit. offsets is empty for user documents and carries one
// entry per member for run documents.
type vectorDoc struct {
	sessionID   string
	ordinal     int
	ordinalEnd  int
	subordinate bool
	offsets     []db.UnitOffset
	content     string
}

// hydrateHits looks up each hit's document row and builds its db.VectorHit,
// resolving the anchor ordinal and display snippet via vector.DocAnchor (a run
// hit anchors to the member whose rune span contains the matched chunk's
// center, with a member-local snippet; a user hit anchors to its own ordinal).
// A hit whose document vanished from vector_documents mid-flight, or is parked
// at a negative-ordinal tombstone, is absent from the lookup and dropped.
func (v *vectorSearcher) hydrateHits(
	ctx context.Context, hits []chunkHit,
) ([]db.VectorHit, error) {
	docKeys := make([]string, len(hits))
	for i, h := range hits {
		docKeys[i] = h.docKey
	}
	docs, err := v.lookupDocs(ctx, docKeys)
	if err != nil {
		return nil, err
	}

	out := make([]db.VectorHit, 0, len(hits))
	for _, h := range hits {
		doc, ok := docs[h.docKey]
		if !ok {
			continue
		}
		anchorOrdinal, snippet := vector.DocAnchor(
			doc.content, doc.offsets, doc.ordinal, h.chunkIndex, v.maxInputChars)
		out = append(out, db.VectorHit{
			SessionID:    doc.sessionID,
			Ordinal:      anchorOrdinal,
			OrdinalStart: doc.ordinal,
			OrdinalEnd:   doc.ordinalEnd,
			Subordinate:  doc.subordinate,
			Score:        h.score,
			Snippet:      snippet,
		})
	}
	return out, nil
}

// lookupDocs reads the document rows for docKeys, keyed by doc_key. The whole
// key set binds as one array parameter (pgx expands ANY natively), so no IN
// chunking is needed. The `ordinal >= 0` guard excludes tombstone rows parked
// at a negative sentinel by the vector push's slot replacement, mirroring
// internal/vector's lookupMirrorDocs: a mid-refresh tombstone must never
// hydrate into a hit carrying a negative ordinal.
func (v *vectorSearcher) lookupDocs(
	ctx context.Context, docKeys []string,
) (map[string]vectorDoc, error) {
	docs := make(map[string]vectorDoc, len(docKeys))
	if len(docKeys) == 0 {
		return docs, nil
	}
	rows, err := v.pg.QueryContext(ctx, `
SELECT doc_key, session_id, ordinal, ordinal_end, subordinate, offsets, content
  FROM vector_documents
 WHERE ordinal >= 0 AND doc_key = ANY($1)`, docKeys)
	if err != nil {
		return nil, fmt.Errorf("looking up search hit documents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var key, offsets string
		var doc vectorDoc
		if err := rows.Scan(&key, &doc.sessionID, &doc.ordinal,
			&doc.ordinalEnd, &doc.subordinate, &offsets, &doc.content); err != nil {
			return nil, fmt.Errorf("scanning search hit document: %w", err)
		}
		if err := json.Unmarshal([]byte(offsets), &doc.offsets); err != nil {
			return nil, fmt.Errorf("parsing offsets for search hit %s: %w", key, err)
		}
		docs[key] = doc
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating search hit documents: %w", err)
	}
	return docs, nil
}

// ResolveMessageUnits maps each ref to the vector_documents unit containing
// it, returning a slice parallel to refs; a ref with no containing unit (its
// message lies outside the embeddable universe, or in a gap between units)
// yields a zero db.UnitRef. Each ref is a point lookup — greatest unit ordinal
// <= ref ordinal, then a containment check against ordinal_end — via one
// prepared statement, so a batch of any size never approaches PG's bind limit.
// The `ordinal >= 0` guard skips tombstone rows parked at a negative sentinel,
// so a ref can never resolve into mid-refresh state (parity with
// internal/vector's ResolveMessageUnits).
func (v *vectorSearcher) ResolveMessageUnits(
	ctx context.Context, refs []db.MessageRef,
) ([]db.UnitRef, error) {
	out := make([]db.UnitRef, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	stmt, err := v.pg.PrepareContext(ctx, `
SELECT doc_key, ordinal, ordinal_end, subordinate
  FROM vector_documents
 WHERE session_id = $1 AND ordinal >= 0 AND ordinal <= $2
 ORDER BY ordinal DESC LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("resolve message units: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for i, ref := range refs {
		var unit db.UnitRef
		err := stmt.QueryRowContext(ctx, ref.SessionID, ref.Ordinal).Scan(
			&unit.DocKey, &unit.OrdinalStart, &unit.OrdinalEnd, &unit.Subordinate)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf(
				"resolve message unit (%s, %d): %w", ref.SessionID, ref.Ordinal, err)
		}
		if ref.Ordinal > unit.OrdinalEnd {
			continue
		}
		unit.SessionID = ref.SessionID
		out[i] = unit
	}
	return out, nil
}

// SetVectorSearcher wires (or, with nil, clears) the PG semantic search
// backend. Safe to call concurrently with SearchContent/HasSemantic.
func (s *Store) SetVectorSearcher(searcher db.VectorSearcher) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorSearcher = searcher
}

// getVectorSearcher returns the currently wired PG vector searcher, or nil.
func (s *Store) getVectorSearcher() db.VectorSearcher {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.vectorSearcher
}

// SetSemanticUnavailableReason records a human explanation for why semantic
// search could not be wired (extension missing, no matching generation, stale
// build), surfaced by semanticUnavailableError. Safe to call concurrently.
func (s *Store) SetSemanticUnavailableReason(reason string) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.semanticUnavailableReason = reason
}

// semanticUnavailableError wraps db.ErrSemanticUnavailable with the recorded
// reason when one is set, so a caller (SearchContent's semantic/hybrid modes)
// can explain why semantic search is off; without a reason it returns the
// bare sentinel.
func (s *Store) semanticUnavailableError() error {
	s.vectorMu.RLock()
	reason := s.semanticUnavailableReason
	s.vectorMu.RUnlock()
	if reason == "" {
		return db.ErrSemanticUnavailable
	}
	return db.NewSemanticUnavailableError(reason)
}
