package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"

	"go.kenn.io/agentsview/internal/db"
)

type artifactPublicationAuthority interface {
	artifactPublicationStreamer
	GetArtifactCheckpointHead(
		context.Context,
		string,
	) (db.ArtifactCheckpointHead, bool, error)
}

// authoritativePublicationStore exposes only the current closure selected by
// the local publication ledger. The embedded store remains available for pull,
// but its unproven collection listings never become outbound authority.
type authoritativePublicationStore struct {
	ArtifactStore

	origin         string
	head           db.ArtifactCheckpointHead
	manifestHashes []string
}

func newAuthoritativePublicationStore(
	ctx context.Context,
	authority artifactPublicationAuthority,
	store ArtifactStore,
	origin string,
) (_ *authoritativePublicationStore, retErr error) {
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
	mapSpool, mapDigest, revision, err := spoolArtifactPublicationMap(
		ctx,
		authority,
		origin,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		retErr = errors.Join(retErr, closeAndRemoveExportSpool(mapSpool))
	}()
	if revision != head.PublicationRevision ||
		mapDigest != head.SessionMapSHA256 {
		return nil, fmt.Errorf(
			"%w: authoritative publication snapshot differs from checkpoint head",
			ErrArtifactConflict,
		)
	}
	info, err := mapSpool.Stat()
	if err != nil {
		return nil, fmt.Errorf("stating authoritative publication map: %w", err)
	}
	if info.Size() > checkpointDecodedLimit {
		return nil, fmt.Errorf(
			"%w: authoritative publication map exceeds checkpoint limit",
			ErrArtifactInvalid,
		)
	}
	manifestHashes, err := decodePublicationManifestHashes(
		mapSpool,
		origin,
	)
	if err != nil {
		return nil, fmt.Errorf("decoding authoritative publication map: %w", err)
	}
	return &authoritativePublicationStore{
		ArtifactStore:  store,
		origin:         origin,
		head:           head,
		manifestHashes: manifestHashes,
	}, nil
}

func decodePublicationManifestHashes(
	reader io.Reader,
	origin string,
) ([]string, error) {
	decoder := json.NewDecoder(reader)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("artifact publication map is not an object")
	}
	hashes := make([]string, 0, 64)
	prefix := origin + "~"
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		gid, ok := token.(string)
		if !ok || len(gid) <= len(prefix) || gid[:len(prefix)] != prefix {
			return nil, errors.New("artifact publication identity is invalid")
		}
		var hash string
		if err := decoder.Decode(&hash); err != nil {
			return nil, err
		}
		if err := validateHashHex(hash); err != nil {
			return nil, err
		}
		hashes = append(hashes, hash)
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return nil, errors.New("artifact publication map is incomplete")
	}
	if token, err = decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf(
			"artifact publication map has trailing token %v",
			token,
		)
	}
	return hashes, nil
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
		refs := make([]authorizedPublicationRef, len(s.manifestHashes))
		for index, hash := range s.manifestHashes {
			ref, err := NewRef(origin, KindManifests, hash+".json")
			if err != nil {
				return nil, err
			}
			refs[index] = authorizedPublicationRef{ref: ref}
		}
		return &publicationRefIterator{
			store: s.ArtifactStore,
			refs:  refs,
		}, nil
	case KindSegments:
		return &publicationSegmentIterator{
			store:          s.ArtifactStore,
			origin:         origin,
			manifestHashes: s.manifestHashes,
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
	var logicalBytes int64
	cache := folderTransportPageCache{manifestIndex: -1}
	for maxObjects <= 0 || len(entries) < maxObjects {
		entry, next, done, err := s.nextFolderTransportEntry(ctx, cursor, &cache)
		if err != nil {
			return nil, cursor, false, err
		}
		if done {
			return entries, cursor, false, nil
		}
		if maxBytes > 0 && len(entries) > 0 && entry.Identity.Size > maxBytes-logicalBytes {
			return entries, cursor, true, nil
		}
		entries = append(entries, entry)
		logicalBytes += entry.Identity.Size
		cursor = next
	}
	_, _, done, err := s.nextFolderTransportEntry(ctx, cursor, &cache)
	if err != nil {
		return nil, cursor, false, err
	}
	return entries, cursor, !done, nil
}

type folderTransportPageCache struct {
	manifestIndex int
	segments      []string
}

func (s *authoritativePublicationStore) nextFolderTransportEntry(
	ctx context.Context,
	cursor folderPushCursor,
	cache *folderTransportPageCache,
) (Entry, folderPushCursor, bool, error) {
	for {
		switch cursor.KindIndex {
		case 0:
			if cursor.ManifestIndex >= len(s.manifestHashes) {
				cursor.KindIndex = 1
				cursor.Offset = 0
				cursor.ManifestIndex = 0
				cursor.SegmentIndex = 0
				continue
			}
			if cache.manifestIndex != cursor.ManifestIndex {
				segments, err := authorizedManifestSegments(
					ctx,
					s.ArtifactStore,
					s.origin,
					s.manifestHashes[cursor.ManifestIndex],
				)
				if err != nil {
					return Entry{}, cursor, false, err
				}
				cache.manifestIndex = cursor.ManifestIndex
				cache.segments = segments
			}
			if cursor.SegmentIndex >= len(cache.segments) {
				cursor.ManifestIndex++
				cursor.SegmentIndex = 0
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
			next := cursor
			next.SegmentIndex++
			return entry, next, false, nil
		case 1:
			if cursor.Offset >= len(s.manifestHashes) {
				cursor.KindIndex = 2
				cursor.Offset = 0
				continue
			}
			ref, err := NewRef(
				s.origin,
				KindManifests,
				s.manifestHashes[cursor.Offset]+".json",
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
			next := cursor
			next.Offset++
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
			next := cursor
			next.Offset = 1
			return entry, next, false, nil
		default:
			return Entry{}, cursor, true, nil
		}
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

type publicationSegmentIterator struct {
	mu sync.Mutex

	store          ArtifactStore
	origin         string
	manifestHashes []string
	manifestIndex  int
	segmentHashes  []string
	segmentIndex   int
	closed         bool
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
		if i.manifestIndex == len(i.manifestHashes) {
			return entries, io.EOF
		}
		segments, err := authorizedManifestSegments(
			ctx,
			i.store,
			i.origin,
			i.manifestHashes[i.manifestIndex],
		)
		if err != nil {
			return nil, err
		}
		i.manifestIndex++
		i.segmentHashes = segments
		i.segmentIndex = 0
	}
	return entries, nil
}

func (i *publicationSegmentIterator) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	i.segmentHashes = nil
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
) (_ []string, retErr error) {
	ref, err := NewRef(origin, KindManifests, hash+".json")
	if err != nil {
		return nil, err
	}
	entry, reader, err := store.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer func() {
		retErr = errors.Join(retErr, reader.Close())
	}()
	if entry.Ref != ref {
		return nil, fmt.Errorf(
			"%w: authoritative manifest reference changed",
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
