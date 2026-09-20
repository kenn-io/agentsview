package storage

import (
	"context"
	"errors"
)

// VectorGenerationInfo identifies the local embedding generation being pushed.
// Machines whose embedding config produces the same Fingerprint share one
// replica generation (and one chunk table); Model and Dimension are recorded
// for diagnostics and to size the vector column.
type VectorGenerationInfo struct {
	Fingerprint string
	Model       string
	Dimension   int
}

// VectorPushChunk is one embedded slice of a document. ChunkIndex is stable
// within a doc_key so re-pushes overwrite the same (doc_key, chunk_index) row.
type VectorPushChunk struct {
	ChunkIndex int
	Embedding  []float32
}

// VectorPushDoc mirrors one local vectors.db document row plus its embeddings.
// DocKey is globally unique and shared across generations, which is why a
// replica keeps one backend-agnostic vector_documents table upserted by
// doc_key rather than a per-generation table.
type VectorPushDoc struct {
	DocKey      string
	SessionID   string
	SourceUUID  string
	Ordinal     int
	OrdinalEnd  int
	Subordinate bool
	OffsetsJSON string
	Content     string
	ContentHash string
	Chunks      []VectorPushChunk
}

// VectorPushSource supplies one transaction-owned local export for a replica
// push phase. The export keeps generation metadata, aggregate hashes, and
// document and chunk reads on one SQLite snapshot.
type VectorPushSource interface {
	BeginExport(ctx context.Context, sessionIDs []string) (VectorExport, bool, error)
}

// VectorExport is one open read of the local vector index.
type VectorExport interface {
	Generation() VectorGenerationInfo
	SessionDocHashes(
		ctx context.Context, sessionIDs []string,
	) (map[string]string, error)
	SessionDocs(
		ctx context.Context, sessionID string,
	) ([]VectorPushDoc, string, error)
	Close() error
}

// ErrVectorSourceNotReady marks a Generation error meaning the local vector
// index exists but is not safe to export right now: an embeddings build is
// rewriting it (or one was interrupted), so its session coverage is partial. A
// push that ran anyway would read that partial view as truth and evict or
// overwrite valid replica vectors. The push turns this into a clean phase
// skip; the next push after the build completes sends everything that
// changed.
var ErrVectorSourceNotReady = errors.New(
	"local vector index is not fully embedded (build in progress or interrupted)")
