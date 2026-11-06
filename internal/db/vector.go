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
var ErrSemanticUnavailable = errors.New("semantic search not available")

// SemanticUnavailableError carries a backend-specific reason while retaining
// ErrSemanticUnavailable as its machine-readable classification. Its rendered
// message adds the backend's own recovery guidance.
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
	// PrepareSemanticSearch embeds query once for this request. The returned
	// page function fetches up to limit unit-level hits, best first, and can
	// run again with a larger limit. The int is the engine's KNN candidate
	// ceiling, or zero when the engine has none.
	PrepareSemanticSearch(ctx context.Context, query string) (VectorSearchPage, int, error)
	// ResolveMessageUnits maps each ref to the unit containing it. The
	// result is parallel to refs; a ref with no containing unit yields a
	// zero UnitRef (DocKey == "").
	ResolveMessageUnits(ctx context.Context, refs []MessageRef) ([]UnitRef, error)
}

// VectorSearchPage returns exhaustion measured before chunk rollup and hydration.
type VectorSearchPage func(context.Context, int) ([]VectorHit, bool, error)

// SemanticReadiness is a bounded, read-only snapshot of the semantic search
// index. It deliberately contains no filesystem paths or provider secrets.
// Producers set Generation only when Embedded and Missing are measured counts.
type SemanticReadiness struct {
	State      string
	Reason     string
	Generation string
	Embedded   int64
	Missing    int64
}

// SemanticReadinessProvider is implemented by vector searchers and stores
// that can describe semantic availability without encoding a probe query.
type SemanticReadinessProvider interface {
	SemanticReadiness(context.Context) (SemanticReadiness, error)
}

// MemoryBackendNamer gives readiness surfaces a stable backend label without
// requiring service code to import every concrete storage package.
type MemoryBackendNamer interface {
	MemoryBackendName() string
}

func (db *DB) MemoryBackendName() string { return "sqlite" }

// RecallVectorHit is one semantic recall-entry match, ranked best first.
type RecallVectorHit struct {
	EntryID string
	Score   float32
}

// RecallVectorSnapshot identifies the embedding generation and served Recall
// corpus revision used to rank a set of vector hits.
type RecallVectorSnapshot struct {
	GenerationFingerprint string
	CorpusRevision        string
}

// RecallVectorSearcher is the seam through which recall querying reaches the
// recall-entry embedding index without introducing an internal/db ->
// internal/vector import cycle.
type RecallVectorSearcher interface {
	SearchRecall(
		ctx context.Context, query string, limit int,
	) (hits []RecallVectorHit, exhausted bool, snapshot RecallVectorSnapshot, err error)
	ValidateRecallSnapshot(ctx context.Context, snapshot RecallVectorSnapshot) error
	// MaxRecallSearchCandidates returns the largest accepted SearchRecall
	// limit, or zero when the backend has no candidate ceiling.
	MaxRecallSearchCandidates() int
}

// SetVectorSearcher wires (or, with nil, clears) the semantic search
// backend. Safe to call concurrently with SearchContent/HasSemantic.
func (db *DB) SetVectorSearcher(v VectorSearcher) {
	db.vectorMu.Lock()
	defer db.vectorMu.Unlock()
	db.vectorSearcher = v
}

// SetRecallVectorSearcher wires (or clears with nil) semantic recall search.
func (db *DB) SetRecallVectorSearcher(v RecallVectorSearcher) {
	db.vectorMu.Lock()
	defer db.vectorMu.Unlock()
	db.recallSearcher = v
}

// HasSemantic reports whether a VectorSearcher has been wired in.
func (db *DB) HasSemantic() bool {
	return db.getVectorSearcher() != nil
}

// SemanticReadiness reports metadata without waiting for an index replacement.
// Searchers predating this capability remain usable but have unknown detail.
func (db *DB) SemanticReadiness(ctx context.Context) (SemanticReadiness, error) {
	if !db.vectorMu.TryRLock() {
		return SemanticReadiness{State: "unknown", Reason: "index_changing"}, nil
	}
	defer db.vectorMu.RUnlock()
	if provider, ok := db.vectorSearcher.(SemanticReadinessProvider); ok {
		return provider.SemanticReadiness(ctx)
	}
	if db.vectorSearcher == nil {
		return SemanticReadiness{State: "unavailable", Reason: "not_configured"}, nil
	}
	return SemanticReadiness{State: "unknown", Reason: "status_unsupported"}, nil
}

// getVectorSearcher returns the currently wired VectorSearcher, or nil.
func (db *DB) getVectorSearcher() VectorSearcher {
	db.vectorMu.RLock()
	defer db.vectorMu.RUnlock()
	return db.vectorSearcher
}

// semanticSearcher validates f and returns the wired VectorSearcher, held
// against replacement until release.
func (db *DB) semanticSearcher(f ContentSearchFilter) (VectorSearcher, func(), error) {
	if err := ValidateSemanticFilter(f); err != nil {
		return nil, func() {}, err
	}
	db.vectorMu.RLock()
	if db.vectorSearcher == nil {
		db.vectorMu.RUnlock()
		return nil, func() {}, ErrSemanticUnavailable
	}
	return db.vectorSearcher, db.vectorMu.RUnlock, nil
}

// getRecallVectorSearcher returns the wired RecallVectorSearcher, or nil,
// held against replacement until release.
func (db *DB) getRecallVectorSearcher() (RecallVectorSearcher, func()) {
	db.vectorMu.RLock()
	return db.recallSearcher, db.vectorMu.RUnlock
}
