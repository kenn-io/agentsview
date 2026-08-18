package rawsync

import (
	"context"
	"fmt"
)

// CommitResult is the durable receipt for one accepted source generation.
type CommitResult struct {
	ManifestID string
	Receipt    string
	Generation int64
	Created    bool
}

// HeadConflictError reports the current accepted source head.
type HeadConflictError struct {
	CurrentManifestID string
	CurrentReceipt    string
	CurrentGeneration int64
}

func (e *HeadConflictError) Error() string {
	if e == nil {
		return "raw sync source head conflict"
	}
	return fmt.Sprintf(
		"raw sync source head conflict at generation %d", e.CurrentGeneration,
	)
}

// Unwrap makes source-head conflicts discoverable through ErrConflict.
func (e *HeadConflictError) Unwrap() error { return ErrConflict }

// MetadataStore records verified custody and atomically accepts manifests.
type MetadataStore interface {
	RecordVerifiedObject(context.Context, AuthIdentity, ObjectRef) error
	RecordVerifiedObjects(context.Context, AuthIdentity, []ObjectRef) error
	MissingObjects(context.Context, AuthIdentity, []ObjectRef) ([]ObjectRef, error)
	CommitManifest(context.Context, CanonicalManifest, string) (CommitResult, error)
}
