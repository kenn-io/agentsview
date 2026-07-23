package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

const (
	docbankLiveRoot       = "/v1"
	docbankQuarantineRoot = "/.quarantine/v1"
)

type docbankStore struct {
	vault        *docbank.Vault
	quarantineMu sync.Mutex
}

type docbankTraversal struct {
	walker        docbankWalker
	page          []docbank.WalkEntry
	pageOffset    int
	lastOrigin    string
	pendingOrigin string
	nextEntry     *Entry
}

func newDocbankContent(vault *docbank.Vault) *docbankStore {
	store := &docbankStore{vault: vault}
	return store
}

func (s *docbankStore) Create(
	ctx context.Context,
	ref Ref,
	identity Identity,
	mediaType string,
	body io.Reader,
) (CreateResult, error) {
	if err := ctx.Err(); err != nil {
		return CreateResult{}, artifactStoreError("create", ref, err)
	}
	if err := validateStoreRef(ref); err != nil {
		return CreateResult{}, artifactStoreError("create", ref, err)
	}
	if err := validateStoreIdentity(identity); err != nil {
		return CreateResult{}, artifactStoreError("create", ref, err)
	}
	if err := validateRefIdentity(ref, identity); err != nil {
		return CreateResult{}, artifactStoreError("create", ref, err)
	}
	if body == nil {
		return CreateResult{}, artifactStoreError("create", ref,
			fmt.Errorf("%w: artifact body is required", ErrArtifactInvalid))
	}
	receipt, err := s.vault.Create(ctx, docbankPath(ref), body, docbank.CreateOptions{
		MediaType: mediaType,
		Expected: docbank.ContentIdentity{
			SHA256: identity.SHA256,
			Size:   identity.Size,
		},
	})
	if err != nil {
		return CreateResult{}, artifactStoreError("create", ref, mapDocbankError(err))
	}
	entry, err := docbankEntry(ref, receipt.Node)
	if err != nil {
		return CreateResult{}, artifactStoreError("create", ref, err)
	}
	result := CreateResult{
		Entry:   entry,
		Created: receipt.Created,
	}
	if receipt.PhysicalCreated {
		result.Physical = PhysicalWrite{
			Kind:         receipt.Physical.Kind,
			Encoding:     receipt.Physical.Encoding,
			LogicalBytes: receipt.Physical.LogicalBytes,
			StoredBytes:  receipt.Physical.StoredBytes,
			PackEligible: receipt.Physical.PackEligible,
		}
	}
	return result, nil
}

func (s *docbankStore) Stat(ctx context.Context, ref Ref) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, artifactStoreError("stat", ref, err)
	}
	if err := validateStoreRef(ref); err != nil {
		return Entry{}, artifactStoreError("stat", ref, err)
	}
	node, err := s.vault.Stat(ctx, docbankPath(ref))
	if err != nil {
		return Entry{}, artifactStoreError("stat", ref, mapDocbankError(err))
	}
	entry, err := docbankEntry(ref, node)
	if err != nil {
		return Entry{}, artifactStoreError("stat", ref, err)
	}
	return entry, nil
}

func (s *docbankStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, nil, artifactStoreError("open", ref, err)
	}
	if err := validateStoreRef(ref); err != nil {
		return Entry{}, nil, artifactStoreError("open", ref, err)
	}
	content, err := s.vault.OpenContent(ctx, docbankPath(ref))
	if err != nil {
		return Entry{}, nil, artifactStoreError("open", ref, mapDocbankError(err))
	}
	entry, err := docbankEntry(ref, content.Node)
	if err != nil {
		closeErr := content.Reader.Close()
		return Entry{}, nil, artifactStoreError("open", ref, errors.Join(err, closeErr))
	}
	return entry, &docbankVerifiedReader{reader: content.Reader}, nil
}

type docbankIterator struct {
	mu sync.Mutex

	vault *docbank.Vault
	root  string
	state docbankTraversal

	opened bool
	done   bool
	closed bool
}

type docbankOriginIterator struct {
	iterator docbankIterator
}

type docbankEntryIterator struct {
	iterator docbankIterator
	origin   string
	kind     Kind
}

func (s *docbankStore) Origins(ctx context.Context) (OriginIterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, artifactStoreError("iterate origins", Ref{}, err)
	}
	return &docbankOriginIterator{iterator: docbankIterator{
		vault: s.vault,
		root:  docbankLiveRoot,
	}}, nil
}

func (s *docbankStore) Entries(
	ctx context.Context, origin string, kind Kind,
) (EntryIterator, error) {
	ref := Ref{Origin: origin, Kind: kind}
	if err := ctx.Err(); err != nil {
		return nil, artifactStoreError("iterate entries", ref, err)
	}
	if err := validateStoreCollection(origin, kind); err != nil {
		return nil, artifactStoreError("iterate entries", ref, err)
	}
	return &docbankEntryIterator{
		iterator: docbankIterator{
			vault: s.vault,
			root:  docbankLiveRoot + "/" + origin + "/" + string(kind),
		},
		origin: origin,
		kind:   kind,
	}, nil
}

func (i *docbankOriginIterator) Next(
	ctx context.Context, limit int,
) ([]string, error) {
	i.iterator.mu.Lock()
	defer i.iterator.mu.Unlock()
	if err := i.iterator.beforeNext(ctx, limit); err != nil {
		return nil, artifactStoreError("iterate origins", Ref{}, err)
	}
	items, more, err := i.iterator.state.nextOrigins(ctx, limit)
	if err != nil {
		return nil, artifactStoreError(
			"iterate origins", Ref{}, i.iterator.fail(err),
		)
	}
	if more {
		return items, nil
	}
	closeErr := i.iterator.finish()
	return items, artifactStoreError("iterate origins", Ref{}, errors.Join(io.EOF, closeErr))
}

func (i *docbankOriginIterator) Close() error {
	i.iterator.mu.Lock()
	defer i.iterator.mu.Unlock()
	return artifactStoreErrorIfError("close origin iterator", Ref{}, i.iterator.close())
}

func (i *docbankEntryIterator) Next(
	ctx context.Context, limit int,
) ([]Entry, error) {
	i.iterator.mu.Lock()
	defer i.iterator.mu.Unlock()
	ref := Ref{Origin: i.origin, Kind: i.kind}
	if err := i.iterator.beforeNext(ctx, limit); err != nil {
		return nil, artifactStoreError("iterate entries", ref, err)
	}
	items, more, err := i.iterator.state.nextEntries(ctx, i.origin, i.kind, limit)
	if err != nil {
		return nil, artifactStoreError(
			"iterate entries", ref, i.iterator.fail(err),
		)
	}
	if more {
		return items, nil
	}
	closeErr := i.iterator.finish()
	return items, artifactStoreError("iterate entries", ref, errors.Join(io.EOF, closeErr))
}

func (i *docbankEntryIterator) Close() error {
	i.iterator.mu.Lock()
	defer i.iterator.mu.Unlock()
	ref := Ref{Origin: i.origin, Kind: i.kind}
	return artifactStoreErrorIfError("close entry iterator", ref, i.iterator.close())
}

func (i *docbankIterator) beforeNext(ctx context.Context, limit int) error {
	if i.closed {
		return fs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return i.fail(err)
	}
	if limit <= 0 || limit > maxArtifactListPageSize {
		return fmt.Errorf(
			"%w: page limit must be between 1 and %d",
			ErrArtifactInvalid,
			maxArtifactListPageSize,
		)
	}
	if i.done {
		return io.EOF
	}
	if i.opened {
		return nil
	}
	i.opened = true
	walker, err := i.vault.Walk(ctx, i.root, docbank.WalkOptions{
		PageSize: min(limit, docbank.MaxWalkPageSize),
	})
	if errors.Is(err, docbank.ErrNotFound) {
		i.done = true
		return io.EOF
	}
	if err != nil {
		i.closed = true
		return mapDocbankError(err)
	}
	i.state.walker = walker
	return nil
}

func (i *docbankIterator) fail(err error) error {
	i.closed = true
	return errors.Join(mapDocbankError(err), i.closeWalker())
}

func (i *docbankIterator) finish() error {
	i.done = true
	return i.closeWalker()
}

func (i *docbankIterator) close() error {
	if i.closed {
		return nil
	}
	i.closed = true
	return i.closeWalker()
}

func (i *docbankIterator) closeWalker() error {
	if i.state.walker == nil {
		return nil
	}
	err := i.state.walker.Close()
	i.state.walker = nil
	return mapDocbankError(err)
}

func artifactStoreErrorIfError(op string, ref Ref, err error) error {
	if err == nil {
		return nil
	}
	return artifactStoreError(op, ref, err)
}

func (s *docbankStore) Quarantine(ctx context.Context, ref Ref, _ string) error {
	if err := ctx.Err(); err != nil {
		return artifactStoreError("quarantine", ref, err)
	}
	if err := validateStoreRef(ref); err != nil {
		return artifactStoreError("quarantine", ref, err)
	}
	s.quarantineMu.Lock()
	defer s.quarantineMu.Unlock()
	parent := docbankQuarantineParent(ref)
	if err := s.ensureQuarantineParent(ctx, parent); err != nil {
		return artifactStoreError("quarantine", ref, mapDocbankError(err))
	}
	for range 4 {
		id, err := newDocbankQuarantineID()
		if err != nil {
			return artifactStoreError("quarantine", ref, err)
		}
		destination := parent + "/" + id + "-" + ref.Name
		_, err = s.vault.MovePath(ctx, docbankPath(ref), destination, docbank.RevisionOptions{})
		if err == nil {
			return nil
		}
		if !errors.Is(err, docbank.ErrExists) {
			return artifactStoreError("quarantine", ref, mapDocbankError(err))
		}
	}
	return artifactStoreError("quarantine", ref,
		fmt.Errorf("%w: quarantine identifier collisions", ErrArtifactConflict))
}

func (s *docbankStore) Trash(ctx context.Context, ref Ref) error {
	if err := ctx.Err(); err != nil {
		return artifactStoreError("trash", ref, err)
	}
	if err := validateStoreRef(ref); err != nil {
		return artifactStoreError("trash", ref, err)
	}
	_, err := s.vault.TrashPath(ctx, docbankPath(ref), docbank.RevisionOptions{})
	if err != nil {
		return artifactStoreError("trash", ref, mapDocbankError(err))
	}
	return nil
}

// RepairContent replaces corrupt physical bytes for an existing canonical
// identity without changing any logical artifact reference.
func (s *docbankStore) RepairContent(
	ctx context.Context, identity Identity, trusted io.Reader,
) error {
	if err := validateStoreIdentity(identity); err != nil {
		return artifactStoreError("repair content", Ref{}, err)
	}
	if trusted == nil {
		return artifactStoreError("repair content", Ref{},
			fmt.Errorf("%w: trusted repair body is required", ErrArtifactInvalid))
	}
	_, err := s.vault.RepairContent(ctx, docbank.ContentIdentity{
		SHA256: identity.SHA256,
		Size:   identity.Size,
	}, trusted)
	if err != nil {
		return artifactStoreError("repair content", Ref{}, mapDocbankError(err))
	}
	return nil
}

func (s *docbankStore) Close() error {
	if s == nil || s.vault == nil {
		return nil
	}
	err := s.vault.Close()
	s.vault = nil
	return err
}

type docbankWalker interface {
	Next(context.Context) ([]docbank.WalkEntry, error)
	Close() error
}

func (s *docbankStore) ensureQuarantineParent(ctx context.Context, parent string) error {
	if _, err := s.vault.Stat(ctx, parent); err == nil {
		return nil
	} else if !errors.Is(err, docbank.ErrNotFound) {
		return err
	}
	emptyHash := sha256.Sum256(nil)
	anchor := parent + "/.anchor"
	receipt, err := s.vault.Create(ctx, anchor, strings.NewReader(""), docbank.CreateOptions{
		MediaType: "application/octet-stream",
		Expected: docbank.ContentIdentity{
			SHA256: hex.EncodeToString(emptyHash[:]),
			Size:   0,
		},
	})
	if err != nil {
		return err
	}
	_, err = s.vault.TrashPath(ctx, anchor, docbank.RevisionOptions{
		IfRevision: receipt.Node.Revision,
	})
	return err
}

func (s *docbankTraversal) nextWalk(ctx context.Context) (docbank.WalkEntry, bool, error) {
	for {
		if s.pageOffset < len(s.page) {
			item := s.page[s.pageOffset]
			s.pageOffset++
			return item, true, nil
		}
		if s.walker == nil {
			return docbank.WalkEntry{}, false, nil
		}
		page, err := s.walker.Next(ctx)
		if errors.Is(err, io.EOF) {
			return docbank.WalkEntry{}, false, nil
		}
		if err != nil {
			return docbank.WalkEntry{}, false, err
		}
		s.page = page
		s.pageOffset = 0
	}
}

func (s *docbankTraversal) nextOrigin(ctx context.Context) (string, bool, error) {
	if s.pendingOrigin != "" {
		origin := s.pendingOrigin
		s.pendingOrigin = ""
		return origin, true, nil
	}
	for {
		item, ok, err := s.nextWalk(ctx)
		if err != nil || !ok {
			return "", ok, err
		}
		ref, valid := refFromDocbankPath(item.Path)
		if !valid || item.Node.BlobHash == "" || ref.Origin == s.lastOrigin {
			continue
		}
		s.lastOrigin = ref.Origin
		return ref.Origin, true, nil
	}
}

func (s *docbankTraversal) nextOrigins(
	ctx context.Context, limit int,
) ([]string, bool, error) {
	items := make([]string, 0, limit)
	for len(items) < limit {
		origin, ok, err := s.nextOrigin(ctx)
		if err != nil || !ok {
			return items, false, err
		}
		items = append(items, origin)
	}
	next, ok, err := s.nextOrigin(ctx)
	if err != nil || !ok {
		return items, false, err
	}
	s.pendingOrigin = next
	return items, true, nil
}

func (s *docbankTraversal) nextLogicalEntry(
	ctx context.Context, origin string, kind Kind,
) (Entry, bool, error) {
	if s.nextEntry != nil {
		entry := *s.nextEntry
		s.nextEntry = nil
		return entry, true, nil
	}
	for {
		item, ok, err := s.nextWalk(ctx)
		if err != nil || !ok {
			return Entry{}, ok, err
		}
		ref, valid := refFromDocbankPath(item.Path)
		if !valid || item.Node.BlobHash == "" || ref.Origin != origin || ref.Kind != kind {
			continue
		}
		entry, err := docbankEntry(ref, item.Node)
		return entry, true, err
	}
}

func (s *docbankTraversal) nextEntries(
	ctx context.Context, origin string, kind Kind, limit int,
) ([]Entry, bool, error) {
	items := make([]Entry, 0, limit)
	for len(items) < limit {
		entry, ok, err := s.nextLogicalEntry(ctx, origin, kind)
		if err != nil || !ok {
			return items, false, err
		}
		items = append(items, entry)
	}
	next, ok, err := s.nextLogicalEntry(ctx, origin, kind)
	if err != nil || !ok {
		return items, false, err
	}
	s.nextEntry = &next
	return items, true, nil
}

func docbankPath(ref Ref) string {
	return docbankLiveRoot + "/" + ref.Origin + "/" + string(ref.Kind) + "/" + ref.Name
}

func docbankQuarantineParent(ref Ref) string {
	return docbankQuarantineRoot + "/" + ref.Origin + "/" + string(ref.Kind)
}

func refFromDocbankPath(value string) (Ref, bool) {
	if !strings.HasPrefix(value, docbankLiveRoot+"/") {
		return Ref{}, false
	}
	parts := strings.Split(strings.TrimPrefix(value, docbankLiveRoot+"/"), "/")
	if len(parts) != 3 || value != docbankLiveRoot+"/"+strings.Join(parts, "/") {
		return Ref{}, false
	}
	ref, err := NewRef(parts[0], Kind(parts[1]), parts[2])
	return ref, err == nil
}

func docbankEntry(ref Ref, node docbank.Node) (Entry, error) {
	identity, err := NewIdentity(node.BlobHash, node.Size)
	if err != nil {
		return Entry{}, fmt.Errorf("%w: invalid Docbank node identity: %w", ErrArtifactCorrupt, err)
	}
	if err := validateRefIdentity(ref, identity); err != nil {
		return Entry{}, fmt.Errorf("%w: reference and Docbank blob identity differ: %w",
			ErrArtifactCorrupt, err)
	}
	modified, err := time.Parse(time.RFC3339Nano, node.ModifiedAt)
	if err != nil {
		return Entry{}, fmt.Errorf("%w: invalid Docbank modification time: %w", ErrArtifactCorrupt, err)
	}
	return Entry{Ref: ref, Identity: identity, Modified: modified}, nil
}

func newDocbankQuarantineID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func mapDocbankError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, docbank.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrArtifactNotFound, err)
	case errors.Is(err, docbank.ErrContentConflict), errors.Is(err, docbank.ErrExists):
		return fmt.Errorf("%w: %w", ErrArtifactConflict, err)
	case errors.Is(err, docbank.ErrStaleRevision):
		return fmt.Errorf("%w: %w", ErrArtifactConflict, err)
	case errors.Is(err, docbank.ErrDigestMismatch), errors.Is(err, docbank.ErrSizeMismatch),
		errors.Is(err, docbank.ErrInvalidMaintenanceCursor):
		return fmt.Errorf("%w: %w", ErrArtifactInvalid, err)
	case errors.Is(err, docbank.ErrContentUnavailable), errors.Is(err, packstore.ErrContentMismatch):
		return fmt.Errorf("%w: %w", ErrArtifactCorrupt, err)
	default:
		return err
	}
}

type docbankVerifiedReader struct {
	reader docbank.VerifiedReadCloser
}

func (r *docbankVerifiedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	return n, mapDocbankReadError(err)
}

func (r *docbankVerifiedReader) Verify() error {
	return mapDocbankReadError(r.reader.Verify())
}

func (r *docbankVerifiedReader) Close() error {
	return mapDocbankReadError(r.reader.Close())
}

func mapDocbankReadError(err error) error {
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, pack.ErrVerificationIncomplete):
		return fmt.Errorf("%w: %w", errIncompleteArtifact, err)
	default:
		return fmt.Errorf("%w: %w", ErrArtifactCorrupt, err)
	}
}

var _ ArtifactStore = (*docbankStore)(nil)
