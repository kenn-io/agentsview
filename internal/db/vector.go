package db

import (
	"context"
	"errors"
)

// ErrSemanticUnavailable is returned by SearchContent for modes "semantic"
// and "hybrid" when no VectorSearcher has been wired in (SetVectorSearcher
// was never called, or the concrete backend doesn't support semantic search
// at all). Backends with more specific context wrap this classification with
// SemanticUnavailableError.
var ErrSemanticUnavailable = errors.New(
	"semantic search not available: enable [vector] in config.toml and run 'agentsview embeddings build'")

// SemanticUnavailableError carries a backend-specific reason while retaining
// ErrSemanticUnavailable as its machine-readable classification. Its rendered
// message deliberately omits the sentinel's local setup guidance, which is not
// valid for every storage backend.
type SemanticUnavailableError struct {
	Reason string
}

func (e *SemanticUnavailableError) Error() string {
	return "semantic search not available: " + e.Reason
}

func (e *SemanticUnavailableError) Unwrap() error {
	return ErrSemanticUnavailable
}

// NewSemanticUnavailableError returns a reasoned semantic capability error.
func NewSemanticUnavailableError(reason string) error {
	return &SemanticUnavailableError{Reason: reason}
}

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

// VectorHit is one unit-level semantic search hit, ranked best first.
// Ordinal is the anchor ordinal: for a run document, the member message
// whose rune span contains the matched chunk's center; for a user document
// it is the message's own ordinal. OrdinalStart/OrdinalEnd span the whole
// unit (both equal Ordinal for user documents), and Subordinate carries the
// unit's sidechain/subagent classification from the vector mirror.
type VectorHit struct {
	SessionID    string
	Ordinal      int // anchor ordinal
	OrdinalStart int
	OrdinalEnd   int
	Subordinate  bool
	Score        float32
	Snippet      string
}

// MessageRef identifies one message by its session and ordinal, the shape
// the hybrid FTS leg hands to ResolveMessageUnits.
type MessageRef struct {
	SessionID string
	Ordinal   int
}

// UnitRef locates the embedding unit (user document or assistant run)
// containing a message. The zero value (DocKey == "") means "no containing
// unit": the message lies outside the embeddable universe, and the hybrid
// path keeps such an FTS hit at message granularity rather than dropping it.
type UnitRef struct {
	DocKey       string
	SessionID    string
	OrdinalStart int
	OrdinalEnd   int
	Subordinate  bool
}

// VectorSearcher is the seam through which internal/db reaches the vector
// embedding index without importing internal/vector directly, which would
// create an import cycle (internal/vector depends on internal/db's schema
// helpers). The concrete implementation is internal/vector's Index, wired in
// at startup via SetVectorSearcher.
type VectorSearcher interface {
	// SemanticSearch embeds query and returns up to limit unit-level hits,
	// best first.
	SemanticSearch(ctx context.Context, query string, limit int) ([]VectorHit, error)
	// ResolveMessageUnits maps each ref to the unit containing it. The
	// result is parallel to refs; a ref with no containing unit yields a
	// zero UnitRef (DocKey == "").
	ResolveMessageUnits(ctx context.Context, refs []MessageRef) ([]UnitRef, error)
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
