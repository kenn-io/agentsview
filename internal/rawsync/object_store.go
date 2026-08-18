package rawsync

import (
	"context"
	"io"
	"time"
)

// ObjectInfo describes one immutable semantic custody object.
type ObjectInfo struct {
	Ref      ObjectRef
	Modified time.Time
}

// PutResult distinguishes a new immutable object from an identical retry.
type PutResult struct {
	Info    ObjectInfo
	Created bool
}

// VerifiedObjectReader verifies the complete semantic object on demand.
type VerifiedObjectReader interface {
	io.ReadCloser
	Verify() error
}

// ObjectStore owns immutable source objects and canonical manifest envelopes.
type ObjectStore interface {
	PutObject(context.Context, string, ObjectRef, io.Reader) (PutResult, error)
	StatObject(context.Context, string, ObjectRef) (ObjectInfo, error)
	OpenObject(context.Context, string, ObjectRef) (ObjectInfo, VerifiedObjectReader, error)
	MissingObjects(context.Context, string, []ObjectRef) ([]ObjectRef, error)
	// VerifyObjects requires every supplied semantic identity to exist exactly.
	VerifyObjects(context.Context, string, []ObjectRef) error
	PutManifest(context.Context, CanonicalManifest) (PutResult, error)
	OpenManifest(context.Context, AuthIdentity, string) (ObjectInfo, VerifiedObjectReader, error)
}
