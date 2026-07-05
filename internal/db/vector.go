package db

import (
	"context"
	"errors"
)

// ErrSemanticUnavailable is returned by SearchContent for modes "semantic"
// and "hybrid" when no VectorSearcher has been wired in (SetVectorSearcher
// was never called, or the concrete backend doesn't support semantic search
// at all — PostgreSQL and DuckDB always return it for these modes).
var ErrSemanticUnavailable = errors.New(
	"semantic search not available: enable [vector] in config.toml and run 'agentsview embeddings build'")

// ErrSemanticTransient is returned by SearchContent's semantic/hybrid modes
// when the wired VectorSearcher's query-time embed call itself failed (the
// embeddings endpoint is down, slow, or erroring), as distinct from
// ErrSemanticUnavailable's "not configured, or a build has never
// completed" cases: semantic search IS configured and otherwise usable,
// this particular request just failed and can be retried. It is
// deliberately not wrapped by ErrSemanticUnavailable, so
// errors.Is(err, ErrSemanticUnavailable) stays false for it and callers
// don't mistake a transient endpoint outage for "semantic search is
// disabled".
var ErrSemanticTransient = errors.New(
	"semantic search embeddings endpoint is unavailable; the request can be retried")

// VectorHit is one message-level semantic search hit, ranked best first.
type VectorHit struct {
	SessionID string
	Ordinal   int
	Score     float32
	Snippet   string
}

// VectorSearcher is the seam through which internal/db reaches the vector
// embedding index without importing internal/vector directly, which would
// create an import cycle (internal/vector depends on internal/db's schema
// helpers). The concrete implementation is internal/vector's Index, wired in
// at startup via SetVectorSearcher.
type VectorSearcher interface {
	// SemanticSearch embeds query and returns up to limit message-level
	// hits, best first.
	SemanticSearch(ctx context.Context, query string, limit int) ([]VectorHit, error)
}

// SetVectorSearcher wires (or, with nil, clears) the semantic search
// backend. Safe to call concurrently with SearchContent/HasSemantic.
func (db *DB) SetVectorSearcher(v VectorSearcher) {
	db.vectorMu.Lock()
	defer db.vectorMu.Unlock()
	db.vectorSearcher = v
}

// HasSemantic reports whether a VectorSearcher has been wired in.
func (db *DB) HasSemantic() bool {
	return db.getVectorSearcher() != nil
}

// getVectorSearcher returns the currently wired VectorSearcher, or nil.
func (db *DB) getVectorSearcher() VectorSearcher {
	db.vectorMu.RLock()
	defer db.vectorMu.RUnlock()
	return db.vectorSearcher
}
