package artifact

import (
	"context"
)

// Transport exchanges immutable artifact objects through their external wire
// representation. Implementations never receive the Docbank physical layout.
type Transport interface {
	Prepare(context.Context, ArtifactStore) error
	Exchange(context.Context, ArtifactStore) (ExchangeResult, error)
	Close() error
}

// ExchangeResult reports the logical effects of one transport exchange.
type ExchangeResult struct {
	Received  int
	Published int
}

// FolderTransportOptions defines roots that must remain disjoint from the
// external artifact target.
type FolderTransportOptions struct {
	ForbiddenRoots []string
}

type transportChangeRecorder interface {
	RecordTransportChanged(context.Context, Entry) error
}
