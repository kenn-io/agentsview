package storage

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

// VectorQueryEncoder embeds one query string into a generation's vector
// space. It is the read-side counterpart of the build-time encoder; a
// returned error means the embeddings endpoint failed for this request, not
// that semantic search is unconfigured.
type VectorQueryEncoder func(ctx context.Context, text string) ([]float32, error)

// VectorSearchStore is a ReplicaStore that routes semantic and hybrid
// content searches to an installed db.VectorSearcher, and explains why the
// modes are unavailable when none is installed.
type VectorSearchStore interface {
	ReplicaStore
	// SetVectorSearcher installs (or, with nil, clears) the searcher.
	SetVectorSearcher(searcher db.VectorSearcher)
	// SetSemanticUnavailableReason records the explanation the store returns
	// through db.ErrSemanticUnavailable while no searcher is installed.
	SetSemanticUnavailableReason(reason string)
}

// VectorSearchProvider is implemented by a Replica whose store can serve
// semantic search over the embeddings a push replicated. It is optional: a
// replica that does not implement it reports semantic search unsupported.
// The serve-side gate (config checks, fingerprint match, encoder) is shared;
// a provider only answers what its store holds and builds the searcher.
type VectorSearchProvider interface {
	// VectorGenerations lists every embedding generation pushed to store,
	// oldest first. A store whose vector tables were never created returns
	// none without error.
	VectorGenerations(
		ctx context.Context, store ReplicaStore,
	) ([]VectorGenerationInfo, error)
	// OpenVectorSearcher serves the pushed generation whose fingerprint
	// matches gen. maxInputChars is the build-time chunk size, needed to
	// re-split documents for anchors and snippets. A nil searcher comes with
	// a non-empty reason (the generation is missing or not fully written)
	// for the store to report; an error is an unexpected query failure.
	OpenVectorSearcher(
		ctx context.Context, store ReplicaStore, gen VectorGenerationInfo,
		maxInputChars int, encode VectorQueryEncoder,
	) (db.VectorSearcher, string, error)
}
