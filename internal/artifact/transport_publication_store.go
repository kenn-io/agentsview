package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/db"
)

type artifactPublicationAuthority interface {
	GetArtifactCheckpointHead(
		context.Context,
		string,
	) (db.ArtifactCheckpointHead, bool, error)
	ArtifactPublicationPage(
		context.Context,
		string,
		string,
		int,
	) ([]db.ArtifactPublication, int64, bool, error)
}

var errArtifactPublicationPageBoundary = errors.New(
	"artifact publication page boundary",
)

// authoritativePublicationStore exposes only the current closure selected by
// the local publication ledger. The embedded store remains available for pull,
// but its unproven collection listings never become outbound authority.
type authoritativePublicationStore struct {
	ArtifactStore

	authority artifactPublicationAuthority
	origin    string
	head      db.ArtifactCheckpointHead
}

func newAuthoritativePublicationStore(
	ctx context.Context,
	authority artifactPublicationAuthority,
	store ArtifactStore,
	origin string,
) (*authoritativePublicationStore, error) {
	if authority == nil {
		return nil, errors.New("artifact publication authority is required")
	}
	if store == nil {
		return nil, errors.New("artifact publication store is required")
	}
	if err := validateOriginID(origin); err != nil {
		return nil, err
	}
	head, found, err := authority.GetArtifactCheckpointHead(ctx, origin)
	if err != nil {
		return nil, fmt.Errorf("reading authoritative artifact checkpoint: %w", err)
	}
	if !found {
		return nil, errors.New("authoritative artifact checkpoint is missing")
	}
	if head.Origin != origin {
		return nil, fmt.Errorf(
			"%w: authoritative checkpoint origin mismatch",
			ErrArtifactConflict,
		)
	}
	if head.Sequence <= 0 || head.PublicationRevision < 0 {
		return nil, fmt.Errorf(
			"%w: authoritative checkpoint head is invalid",
			ErrArtifactInvalid,
		)
	}
	if err := validateHashHex(head.SessionMapSHA256); err != nil {
		return nil, fmt.Errorf("authoritative session map identity: %w", err)
	}
	if _, err := NewIdentity(head.CheckpointSHA256, head.CheckpointSize); err != nil {
		return nil, fmt.Errorf("authoritative checkpoint identity: %w", err)
	}
	if _, err := NewRef(
		origin,
		KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", head.Sequence),
	); err != nil {
		return nil, err
	}
	return &authoritativePublicationStore{
		ArtifactStore: store,
		authority:     authority,
		origin:        origin,
		head:          head,
	}, nil
}

func (s *authoritativePublicationStore) Entries(
	ctx context.Context,
	origin string,
	kind Kind,
) (EntryIterator, error) {
	if err := validateStoreCollection(origin, kind); err != nil {
		return nil, err
	}
	if origin != s.origin {
		return nil, fmt.Errorf(
			"%w: publication origin is not authoritative",
			ErrArtifactInvalid,
		)
	}
	switch kind {
	case KindCheckpoints:
		ref, err := NewRef(
			origin,
			KindCheckpoints,
			fmt.Sprintf("cp-%010d.json", s.head.Sequence),
		)
		if err != nil {
			return nil, err
		}
		identity, err := NewIdentity(
			s.head.CheckpointSHA256,
			s.head.CheckpointSize,
		)
		if err != nil {
			return nil, err
		}
		return &publicationRefIterator{
			store: s.ArtifactStore,
			refs: []authorizedPublicationRef{{
				ref:      ref,
				identity: &identity,
			}},
		}, nil
	case KindManifests:
		return &publicationManifestIterator{
			store: s.ArtifactStore,
			publications: publicationPageCursor{
				authority:        s.authority,
				origin:           origin,
				expectedRevision: s.head.PublicationRevision,
			},
		}, nil
	case KindSegments:
		return &publicationSegmentIterator{
			store:  s.ArtifactStore,
			origin: origin,
			publications: publicationPageCursor{
				authority:        s.authority,
				origin:           origin,
				expectedRevision: s.head.PublicationRevision,
			},
		}, nil
	default:
		return nil, fmt.Errorf(
			"%w: artifact kind is not publication-authoritative",
			ErrArtifactInvalid,
		)
	}
}

func (s *authoritativePublicationStore) folderTransportGeneration() string {
	return fmt.Sprintf(
		"%d:%s:%d",
		s.head.Sequence,
		s.head.CheckpointSHA256,
		s.head.CheckpointSize,
	)
}

func (s *authoritativePublicationStore) folderTransportPage(
	ctx context.Context,
	cursor folderPushCursor,
	maxObjects int,
	maxBytes int64,
) ([]Entry, folderPushCursor, bool, error) {
	entries := make([]Entry, 0, min(maxObjects, 64))
	budget := newFolderExchangeBudget(maxObjects, maxBytes)
	cache := folderTransportPageCache{}
	for !budget.objectLimitReached() {
		entry, next, done, err := s.nextFolderTransportEntry(
			ctx,
			cursor,
			&cache,
			budget,
		)
		if errors.Is(err, errArtifactPublicationPageBoundary) {
			return entries, next, true, nil
		}
		if err != nil {
			return nil, cursor, false, err
		}
		if done {
			return entries, cursor, false, nil
		}
		entries = append(entries, entry)
		budget.consumeObject(entry.Identity.Size)
		cursor = next
	}
	return entries, cursor, true, nil
}

type folderTransportPageCache struct {
	kindIndex        int
	loaded           bool
	publications     []db.ArtifactPublication
	publicationIndex int
	more             bool
	pageBoundary     bool
	manifestSession  string
	segments         []string
}

func (s *authoritativePublicationStore) nextFolderTransportEntry(
	ctx context.Context,
	cursor folderPushCursor,
	cache *folderTransportPageCache,
	budget *folderExchangeBudget,
) (Entry, folderPushCursor, bool, error) {
	for {
		switch cursor.KindIndex {
		case 0:
			allowOversizedObject := budget.objects == 0
			publication, found, err := s.currentFolderPublication(
				ctx,
				cursor,
				cache,
			)
			if err != nil {
				return Entry{}, cursor, false, err
			}
			if !found {
				cursor.KindIndex = 1
				cursor.Offset = 0
				cursor.PublicationSessionID = ""
				cursor.SegmentIndex = 0
				continue
			}
			if cache.manifestSession != publication.SessionID {
				manifestEntry, err := statAuthorizedManifest(
					ctx,
					s.ArtifactStore,
					s.origin,
					publication.ManifestHash,
				)
				if err != nil {
					return Entry{}, cursor, false, err
				}
				if !budget.permitsBytes(
					manifestEntry.Identity.Size,
					budget.objects == 0 && budget.bytes == 0,
				) {
					return Entry{}, cursor, false, errArtifactPublicationPageBoundary
				}
				segments, err := readAuthorizedManifestSegments(
					ctx,
					s.ArtifactStore,
					s.origin,
					manifestEntry,
				)
				if err != nil {
					return Entry{}, cursor, false, err
				}
				budget.consumeInspection(manifestEntry.Identity.Size)
				cache.manifestSession = publication.SessionID
				cache.segments = segments
			}
			if cursor.SegmentIndex >= len(cache.segments) {
				cursor.PublicationSessionID = publication.SessionID
				cursor.SegmentIndex = 0
				cache.advancePublication()
				continue
			}
			ref, err := NewRef(
				s.origin,
				KindSegments,
				cache.segments[cursor.SegmentIndex]+".ndjson",
			)
			if err != nil {
				return Entry{}, cursor, false, err
			}
			entry, err := statAuthorizedPublication(
				ctx,
				s.ArtifactStore,
				authorizedPublicationRef{ref: ref},
			)
			if err != nil {
				return Entry{}, cursor, false, err
			}
			if !budget.permitsBytes(entry.Identity.Size, allowOversizedObject) {
				return Entry{}, cursor, false, errArtifactPublicationPageBoundary
			}
			next := cursor
			next.SegmentIndex++
			return entry, next, false, nil
		case 1:
			publication, found, err := s.currentFolderPublication(
				ctx,
				cursor,
				cache,
			)
			if err != nil {
				return Entry{}, cursor, false, err
			}
			if !found {
				cursor.KindIndex = 2
				cursor.Offset = 0
				cursor.PublicationSessionID = ""
				continue
			}
			ref, err := NewRef(
				s.origin,
				KindManifests,
				publication.ManifestHash+".json",
			)
			if err != nil {
				return Entry{}, cursor, false, err
			}
			entry, err := statAuthorizedPublication(
				ctx,
				s.ArtifactStore,
				authorizedPublicationRef{ref: ref},
			)
			if err != nil {
				return Entry{}, cursor, false, err
			}
			if !budget.permitsBytes(entry.Identity.Size, budget.objects == 0) {
				return Entry{}, cursor, false, errArtifactPublicationPageBoundary
			}
			next := cursor
			next.PublicationSessionID = publication.SessionID
			cache.advancePublication()
			return entry, next, false, nil
		case 2:
			if cursor.Offset > 0 {
				cursor.KindIndex = 3
				continue
			}
			ref, err := NewRef(
				s.origin,
				KindCheckpoints,
				fmt.Sprintf("cp-%010d.json", s.head.Sequence),
			)
			if err != nil {
				return Entry{}, cursor, false, err
			}
			identity, err := NewIdentity(
				s.head.CheckpointSHA256,
				s.head.CheckpointSize,
			)
			if err != nil {
				return Entry{}, cursor, false, err
			}
			entry, err := statAuthorizedPublication(
				ctx,
				s.ArtifactStore,
				authorizedPublicationRef{ref: ref, identity: &identity},
			)
			if err != nil {
				return Entry{}, cursor, false, err
			}
			if !budget.permitsBytes(entry.Identity.Size, budget.objects == 0) {
				return Entry{}, cursor, false, errArtifactPublicationPageBoundary
			}
			next := cursor
			next.Offset = 1
			return entry, next, false, nil
		default:
			return Entry{}, cursor, true, nil
		}
	}
}

func (s *authoritativePublicationStore) currentFolderPublication(
	ctx context.Context,
	cursor folderPushCursor,
	cache *folderTransportPageCache,
) (db.ArtifactPublication, bool, error) {
	if !cache.loaded || cache.kindIndex != cursor.KindIndex {
		if cache.pageBoundary && cache.kindIndex == cursor.KindIndex {
			cache.pageBoundary = false
			return db.ArtifactPublication{}, false,
				errArtifactPublicationPageBoundary
		}
		page, revision, more, err := s.authority.ArtifactPublicationPage(
			ctx,
			s.origin,
			cursor.PublicationSessionID,
			transportStorePageSize,
		)
		if err != nil {
			return db.ArtifactPublication{}, false, err
		}
		if err := validateArtifactPublicationPage(
			page,
			revision,
			s.head.PublicationRevision,
			s.origin,
			cursor.PublicationSessionID,
		); err != nil {
			return db.ArtifactPublication{}, false, err
		}
		if len(page) == 0 && more {
			return db.ArtifactPublication{}, false, fmt.Errorf(
				"%w: empty artifact publication page has continuation",
				ErrArtifactConflict,
			)
		}
		cache.kindIndex = cursor.KindIndex
		cache.loaded = true
		cache.publications = page
		cache.publicationIndex = 0
		cache.more = more
		cache.manifestSession = ""
		cache.segments = nil
	}
	if cache.publicationIndex >= len(cache.publications) {
		return db.ArtifactPublication{}, false, nil
	}
	return cache.publications[cache.publicationIndex], true, nil
}

func (c *folderTransportPageCache) advancePublication() {
	c.publicationIndex++
	c.manifestSession = ""
	c.segments = nil
	if c.publicationIndex == len(c.publications) && c.more {
		c.loaded = false
		c.pageBoundary = true
	}
}

func (s *authoritativePublicationStore) RecordTransportChanged(
	ctx context.Context,
	entry Entry,
) error {
	recorder, ok := s.ArtifactStore.(transportChangeRecorder)
	if !ok {
		return errors.New("artifact transport change recorder is required")
	}
	return recorder.RecordTransportChanged(ctx, entry)
}

type authorizedPublicationRef struct {
	ref      Ref
	identity *Identity
}

type publicationRefIterator struct {
	mu sync.Mutex

	store  ArtifactStore
	refs   []authorizedPublicationRef
	index  int
	closed bool
}

func (i *publicationRefIterator) Next(
	ctx context.Context,
	limit int,
) ([]Entry, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := validatePublicationIteratorNext(ctx, limit, i.closed); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, min(limit, len(i.refs)-i.index))
	for i.index < len(i.refs) && len(entries) < limit {
		authorized := i.refs[i.index]
		entry, err := statAuthorizedPublication(
			ctx,
			i.store,
			authorized,
		)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		i.index++
	}
	if i.index == len(i.refs) {
		return entries, io.EOF
	}
	return entries, nil
}

func (i *publicationRefIterator) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	return nil
}

type publicationPageCursor struct {
	authority        artifactPublicationAuthority
	origin           string
	expectedRevision int64
	afterSessionID   string
	page             []db.ArtifactPublication
	index            int
	more             bool
	pageBoundary     bool
	exhausted        bool
}

func (c *publicationPageCursor) current(
	ctx context.Context,
) (db.ArtifactPublication, bool, error) {
	if c.index < len(c.page) {
		return c.page[c.index], true, nil
	}
	if c.exhausted {
		return db.ArtifactPublication{}, false, nil
	}
	if c.pageBoundary {
		c.pageBoundary = false
		return db.ArtifactPublication{}, false,
			errArtifactPublicationPageBoundary
	}
	page, revision, more, err := c.authority.ArtifactPublicationPage(
		ctx,
		c.origin,
		c.afterSessionID,
		transportStorePageSize,
	)
	if err != nil {
		return db.ArtifactPublication{}, false, err
	}
	if err := validateArtifactPublicationPage(
		page,
		revision,
		c.expectedRevision,
		c.origin,
		c.afterSessionID,
	); err != nil {
		return db.ArtifactPublication{}, false, err
	}
	if len(page) == 0 {
		if more {
			return db.ArtifactPublication{}, false, fmt.Errorf(
				"%w: empty artifact publication page has continuation",
				ErrArtifactConflict,
			)
		}
		c.exhausted = true
		return db.ArtifactPublication{}, false, nil
	}
	c.page = page
	c.index = 0
	c.more = more
	return c.page[0], true, nil
}

func (c *publicationPageCursor) advance() {
	c.afterSessionID = c.page[c.index].SessionID
	c.index++
	if c.index == len(c.page) && !c.more {
		c.exhausted = true
	} else if c.index == len(c.page) {
		c.pageBoundary = true
	}
}

type publicationManifestIterator struct {
	mu sync.Mutex

	store        ArtifactStore
	publications publicationPageCursor
	closed       bool
}

func (i *publicationManifestIterator) Next(
	ctx context.Context,
	limit int,
) ([]Entry, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := validatePublicationIteratorNext(ctx, limit, i.closed); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, min(limit, 64))
	for len(entries) < limit {
		publication, found, err := i.publications.current(ctx)
		if errors.Is(err, errArtifactPublicationPageBoundary) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		if !found {
			return entries, io.EOF
		}
		ref, err := NewRef(
			i.publications.origin,
			KindManifests,
			publication.ManifestHash+".json",
		)
		if err != nil {
			return nil, err
		}
		entry, err := statAuthorizedPublication(
			ctx,
			i.store,
			authorizedPublicationRef{ref: ref},
		)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		i.publications.advance()
	}
	return entries, nil
}

func (i *publicationManifestIterator) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	i.publications.page = nil
	return nil
}

type publicationSegmentIterator struct {
	mu sync.Mutex

	store           ArtifactStore
	origin          string
	publications    publicationPageCursor
	manifestSession string
	segmentHashes   []string
	segmentIndex    int
	closed          bool
}

func (i *publicationSegmentIterator) Next(
	ctx context.Context,
	limit int,
) ([]Entry, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := validatePublicationIteratorNext(ctx, limit, i.closed); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, min(limit, 64))
	for len(entries) < limit {
		if i.segmentIndex < len(i.segmentHashes) {
			ref, err := NewRef(
				i.origin,
				KindSegments,
				i.segmentHashes[i.segmentIndex]+".ndjson",
			)
			if err != nil {
				return nil, err
			}
			entry, err := statAuthorizedPublication(
				ctx,
				i.store,
				authorizedPublicationRef{ref: ref},
			)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
			i.segmentIndex++
			continue
		}
		if i.manifestSession != "" {
			i.publications.advance()
			i.manifestSession = ""
			i.segmentHashes = nil
			i.segmentIndex = 0
		}
		publication, found, err := i.publications.current(ctx)
		if errors.Is(err, errArtifactPublicationPageBoundary) {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		if !found {
			return entries, io.EOF
		}
		if i.manifestSession != publication.SessionID {
			segments, err := authorizedManifestSegments(
				ctx,
				i.store,
				i.origin,
				publication.ManifestHash,
			)
			if err != nil {
				return nil, err
			}
			i.manifestSession = publication.SessionID
			i.segmentHashes = segments
			i.segmentIndex = 0
		}
		if len(i.segmentHashes) == 0 {
			i.publications.advance()
			i.manifestSession = ""
		}
	}
	return entries, nil
}

func (i *publicationSegmentIterator) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	i.publications.page = nil
	i.segmentHashes = nil
	return nil
}

func validateArtifactPublicationPage(
	publications []db.ArtifactPublication,
	revision int64,
	expectedRevision int64,
	origin string,
	afterSessionID string,
) error {
	if revision != expectedRevision {
		return fmt.Errorf(
			"%w: authoritative publication revision changed",
			ErrArtifactConflict,
		)
	}
	previous := afterSessionID
	for _, publication := range publications {
		if publication.Origin != origin ||
			publication.SessionID == "" ||
			strings.Contains(publication.SessionID, "~") ||
			publication.SessionID <= previous {
			return fmt.Errorf(
				"%w: authoritative publication page is invalid",
				ErrArtifactConflict,
			)
		}
		if err := validateHashHex(publication.ManifestHash); err != nil {
			return fmt.Errorf("authoritative publication manifest: %w", err)
		}
		previous = publication.SessionID
	}
	return nil
}

func validatePublicationIteratorNext(
	ctx context.Context,
	limit int,
	closed bool,
) error {
	if closed {
		return fs.ErrClosed
	}
	if ctx == nil {
		return fmt.Errorf("%w: artifact iterator context is required", ErrArtifactInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if limit <= 0 || limit > maxArtifactListPageSize {
		return fmt.Errorf(
			"%w: page limit must be between 1 and %d",
			ErrArtifactInvalid,
			maxArtifactListPageSize,
		)
	}
	return nil
}

func statAuthorizedPublication(
	ctx context.Context,
	store ArtifactStore,
	authorized authorizedPublicationRef,
) (Entry, error) {
	entry, err := store.Stat(ctx, authorized.ref)
	if err != nil {
		return Entry{}, err
	}
	if entry.Ref != authorized.ref {
		return Entry{}, fmt.Errorf(
			"%w: authoritative artifact reference changed",
			ErrArtifactConflict,
		)
	}
	if err := validateRefIdentity(entry.Ref, entry.Identity); err != nil {
		return Entry{}, err
	}
	if authorized.identity != nil &&
		entry.Identity != *authorized.identity {
		return Entry{}, fmt.Errorf(
			"%w: authoritative artifact identity differs from publication ledger",
			ErrArtifactConflict,
		)
	}
	return entry, nil
}

func authorizedManifestSegments(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	hash string,
) ([]string, error) {
	ref, err := NewRef(origin, KindManifests, hash+".json")
	if err != nil {
		return nil, err
	}
	entry, err := statAuthorizedPublication(
		ctx,
		store,
		authorizedPublicationRef{ref: ref},
	)
	if err != nil {
		return nil, err
	}
	return readAuthorizedManifestSegments(ctx, store, origin, entry)
}

func statAuthorizedManifest(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	hash string,
) (Entry, error) {
	ref, err := NewRef(origin, KindManifests, hash+".json")
	if err != nil {
		return Entry{}, err
	}
	return statAuthorizedPublication(
		ctx,
		store,
		authorizedPublicationRef{ref: ref},
	)
}

func readAuthorizedManifestSegments(
	ctx context.Context,
	store ArtifactStore,
	origin string,
	expected Entry,
) (_ []string, retErr error) {
	entry, reader, err := store.Open(ctx, expected.Ref)
	if err != nil {
		return nil, err
	}
	defer func() {
		retErr = errors.Join(retErr, reader.Close())
	}()
	if entry != expected {
		return nil, fmt.Errorf(
			"%w: authoritative manifest changed during inspection",
			ErrArtifactConflict,
		)
	}
	if err := validateRefIdentity(entry.Ref, entry.Identity); err != nil {
		return nil, err
	}
	if entry.Identity.Size > manifestDecodedLimit {
		return nil, fmt.Errorf(
			"%w: authoritative manifest exceeds decoded limit",
			ErrArtifactInvalid,
		)
	}
	body, err := io.ReadAll(io.LimitReader(reader, manifestDecodedLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > manifestDecodedLimit {
		return nil, fmt.Errorf(
			"%w: authoritative manifest exceeds decoded limit",
			ErrArtifactInvalid,
		)
	}
	if err := reader.Verify(); err != nil {
		return nil, err
	}
	manifest, err := decodeManifestWithLimits(
		body,
		productionArtifactLimits(),
	)
	if err != nil {
		return nil, err
	}
	if manifest.Origin != origin {
		return nil, fmt.Errorf(
			"%w: authoritative manifest origin mismatch",
			ErrArtifactConflict,
		)
	}
	return manifest.Segments, nil
}
