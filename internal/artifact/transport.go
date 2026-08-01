package artifact

import (
	"context"
)

// Transport exchanges immutable artifact objects through their external wire
// representation. Implementations pull supported peer objects but publish only
// the explicitly authorized local origin. They never receive the Docbank
// physical layout.
type Transport interface {
	Prepare(context.Context, ArtifactStore) error
	Exchange(
		context.Context,
		ArtifactStore,
		string,
	) (ExchangeResult, error)
	Close() error
}

// ExchangeResult reports the logical effects of one transport exchange.
type ExchangeResult struct {
	Received  int
	Published int
	More      bool
}

// FolderTransportOptions configures a bounded folder exchange. RepairPublished
// verifies an already-completed authoritative generation and journals only
// restored or rejection-marked objects so advanced peers can retry them. It is
// intended for explicit full synchronization.
type FolderTransportOptions struct {
	ForbiddenRoots  []string
	MaxObjects      int
	MaxBytes        int64
	StateStore      FolderTransportStateStore
	RepairPublished bool
}

// FolderTransportStateStore persists target-bound continuation state between
// bounded exchanges. Implementations must treat namespaceID as an opaque key.
type FolderTransportStateStore interface {
	LoadFolderTransportState(context.Context, string) (string, error)
	SaveFolderTransportState(context.Context, string, string) error
}

type transportChangeRecorder interface {
	RecordTransportChanged(context.Context, Entry) error
}
