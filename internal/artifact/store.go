package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	ErrArtifactCorrupt     = errors.New("artifact corrupt")
	ErrArtifactUnsupported = errors.New("artifact unsupported")
)

const maxArtifactListPageSize = 5000

func validateStoreRef(ref Ref) error {
	canonical, err := NewRef(ref.Origin, ref.Kind, ref.Name)
	if err != nil {
		return err
	}
	if canonical != ref {
		return fmt.Errorf("%w: noncanonical artifact reference", ErrArtifactInvalid)
	}
	return nil
}

func validateStoreCollection(origin string, kind Kind) error {
	if err := validateOriginID(origin); err != nil {
		return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	switch kind {
	case KindCheckpoints, KindManifests, KindSegments, KindMeta, KindRaw:
		return nil
	default:
		return fmt.Errorf("%w: unsupported artifact kind %q", ErrArtifactInvalid, kind)
	}
}

func validateStoreIdentity(identity Identity) error {
	canonical, err := NewIdentity(identity.SHA256, identity.Size)
	if err != nil {
		return err
	}
	if canonical != identity {
		return fmt.Errorf("%w: noncanonical artifact identity", ErrArtifactInvalid)
	}
	return nil
}

func validateRefIdentity(ref Ref, identity Identity) error {
	sha, err := refIdentitySHA(ref)
	if err != nil {
		return err
	}
	if sha != "" && sha != identity.SHA256 {
		return fmt.Errorf("%w: reference and content identity differ", ErrArtifactInvalid)
	}
	return nil
}

func refIdentitySHA(ref Ref) (string, error) {
	switch ref.Kind {
	case KindCheckpoints:
		return "", nil
	case KindManifests:
		return strings.TrimSuffix(ref.Name, ".json"), nil
	case KindSegments:
		return strings.TrimSuffix(ref.Name, ".ndjson"), nil
	case KindMeta:
		_, sha, err := normalizeMetadataName(ref.Name)
		return sha, err
	case KindRaw:
		return ref.Name, nil
	default:
		return "", fmt.Errorf("%w: unsupported artifact kind %q", ErrArtifactInvalid, ref.Kind)
	}
}

func canonicalArtifactMediaType(kind Kind) string {
	switch kind {
	case KindCheckpoints, KindManifests, KindMeta:
		return "application/json"
	case KindSegments:
		return "application/x-ndjson"
	case KindRaw:
		return "application/octet-stream"
	default:
		return ""
	}
}

type contextArtifactReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextArtifactReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func artifactStoreError(op string, ref Ref, err error) error {
	return &ArtifactOpError{Op: op, Ref: ref, Err: err}
}

// Kind identifies one artifact protocol collection.
type Kind string

// Ref is a canonical logical artifact reference. Name never includes a wire
// compression extension.
type Ref struct {
	Origin string
	Kind   Kind
	Name   string
}

// NewRef validates and constructs a canonical logical artifact reference.
func NewRef(origin string, kind Kind, name string) (Ref, error) {
	if err := validateOriginID(origin); err != nil {
		return Ref{}, fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	if err := validateCanonicalArtifactName(kind, name); err != nil {
		return Ref{}, err
	}
	return Ref{Origin: origin, Kind: kind, Name: name}, nil
}

func validateCanonicalArtifactName(kind Kind, name string) error {
	if err := validateArtifactName(name); err != nil {
		return err
	}
	switch kind {
	case KindCheckpoints:
		canonical, err := normalizeCheckpointName(name)
		if err != nil {
			return err
		}
		if canonical != name {
			return fmt.Errorf("%w: checkpoint name is not canonical", ErrArtifactInvalid)
		}
	case KindManifests:
		if err := validateCanonicalHashName(name, ".json"); err != nil {
			return err
		}
	case KindSegments:
		if err := validateCanonicalHashName(name, ".ndjson"); err != nil {
			return err
		}
	case KindMeta:
		canonical, _, err := normalizeMetadataName(name)
		if err != nil {
			return err
		}
		if canonical != name {
			return fmt.Errorf("%w: metadata name is not canonical", ErrArtifactInvalid)
		}
	case KindRaw:
		if err := validateHashHex(name); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unsupported artifact kind %q", ErrArtifactInvalid, kind)
	}
	return nil
}

func validateCanonicalHashName(name, extension string) error {
	if !strings.HasSuffix(name, extension) {
		return fmt.Errorf("%w: artifact name must end in %s", ErrArtifactInvalid, extension)
	}
	hash := strings.TrimSuffix(name, extension)
	if err := validateHashHex(hash); err != nil {
		return err
	}
	return nil
}

// Identity is the canonical uncompressed content identity of an artifact.
type Identity struct {
	SHA256 string
	Size   int64
}

// NewIdentity validates and constructs a canonical content identity.
func NewIdentity(sha256 string, size int64) (Identity, error) {
	if err := validateHashHex(sha256); err != nil {
		return Identity{}, err
	}
	if size < 0 {
		return Identity{}, fmt.Errorf("%w: artifact size must not be negative", ErrArtifactInvalid)
	}
	return Identity{SHA256: sha256, Size: size}, nil
}

// Entry describes one live logical artifact.
type Entry struct {
	Ref      Ref
	Identity Identity
	Modified time.Time
}

// Cursor is an opaque stable-enumeration continuation token.
type Cursor string

// Page is one bounded page of logical artifacts.
type Page struct {
	Items []Entry
	Next  Cursor
}

// ArtifactOpError adds logical operation context without hiding its cause.
type ArtifactOpError struct {
	Op  string
	Ref Ref
	Err error
}

func (e *ArtifactOpError) Error() string {
	if e == nil {
		return "artifact operation failed"
	}
	target := e.Ref.Origin
	if e.Ref.Kind != "" {
		target += "/" + string(e.Ref.Kind)
	}
	if e.Ref.Name != "" {
		target += "/" + e.Ref.Name
	}
	if target == "" {
		return fmt.Sprintf("artifact %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("artifact %s %s: %v", e.Op, target, e.Err)
}

func (e *ArtifactOpError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// VerifiedReader yields authoritative bytes only after terminal io.EOF or a
// successful Verify. Closing an incomplete read does not drain it implicitly.
type VerifiedReader interface {
	io.ReadCloser
	Verify() error
}

// CreateResult describes an immutable logical create or identical retry.
type CreateResult struct {
	Entry    Entry
	Created  bool
	Physical PhysicalWrite
}

// PhysicalWrite reports the physical effect of a logical create.
type PhysicalWrite struct {
	Kind         string
	Encoding     string
	LogicalBytes int64
	StoredBytes  int64
	PackEligible bool
}

// LooseBacklog describes unpacked content eligible for bounded packing.
type LooseBacklog struct {
	EligibleObjects     int64
	EligibleBytes       int64
	EligibleStoredBytes int64
}

// PackResult describes one bounded physical packing pass.
type PackResult struct {
	PackedObjects int
	LogicalBytes  int64
	More          bool
}

// ArtifactStore stores canonical artifact bytes behind logical references.
type ArtifactStore interface {
	Create(context.Context, Ref, Identity, string, io.Reader) (CreateResult, error)
	Stat(context.Context, Ref) (Entry, error)
	Open(context.Context, Ref) (Entry, VerifiedReader, error)
	ListOrigins(context.Context, Cursor, int) ([]string, Cursor, error)
	List(context.Context, string, Kind, Cursor, int) (Page, error)
	Quarantine(context.Context, Ref, string) error
	Trash(context.Context, Ref) error
	Pack(context.Context, int64) (PackResult, error)
	LooseBacklog(context.Context) (LooseBacklog, error)
	Close() error
}

// ArtifactFolderTransportProvider proves that a folder exchange target does
// not overlap the retained identity of the opened local repository.
type ArtifactFolderTransportProvider interface {
	NewFolderTransport(string) (Transport, error)
}

// ArtifactCursorReleaser is implemented by stores whose stable pagination
// cursors retain a snapshot lease. Callers release the latest non-empty cursor
// when traversal stops before the final page.
type ArtifactCursorReleaser interface {
	ReleaseCursor(Cursor) error
}

// ArtifactImportScope is an opaque ownership token used to serialize imports
// that reach one underlying store through multiple decorators.
type ArtifactImportScope struct {
	_ [1]byte
}

// ArtifactImportScopeProvider exposes a stable import ownership token.
// Decorators around a provider should forward this method unchanged.
type ArtifactImportScopeProvider interface {
	ArtifactImportScope() *ArtifactImportScope
}

// WorkBudget bounds one archive-wide maintenance pass.
type WorkBudget struct {
	MaxObjects int
	MaxBytes   int64
	Cursor     string
}

// MaintenanceResult reports bounded maintenance progress and continuation.
type MaintenanceResult struct {
	Processed  int
	Bytes      int64
	NextCursor string
	More       bool
}

// ArtifactMaintainer is the optional archive-wide maintenance contract.
type ArtifactMaintainer interface {
	Verify(context.Context, WorkBudget) (MaintenanceResult, error)
	EmptyTrash(context.Context, time.Duration, WorkBudget) (MaintenanceResult, error)
	GarbageCollect(context.Context, WorkBudget) (MaintenanceResult, error)
	Repack(context.Context, WorkBudget) (MaintenanceResult, error)
}
