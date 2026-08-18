package rawsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"go.kenn.io/agentsview/internal/artifact"
)

const rawObjectStatConcurrency = 32

type artifactObjectStore struct {
	store artifact.ArtifactStore
}

// NewArtifactObjectStore adapts the verified artifact ledger for raw custody.
func NewArtifactObjectStore(store artifact.ArtifactStore) (ObjectStore, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: artifact store is required", ErrInvalid)
	}
	return &artifactObjectStore{store: store}, nil
}

func (s *artifactObjectStore) PutObject(
	ctx context.Context,
	tenantID string,
	object ObjectRef,
	body io.Reader,
) (PutResult, error) {
	if body == nil {
		return PutResult{}, fmt.Errorf("%w: raw object body is required", ErrInvalid)
	}
	ref, identity, err := rawArtifactCoordinates(tenantID, object)
	if err != nil {
		return PutResult{}, err
	}
	created, err := s.store.Create(ctx, ref, identity, "application/octet-stream", body)
	if err != nil {
		return PutResult{}, mapArtifactCreateError("putting raw object", err)
	}
	return putResultFromArtifact(created), nil
}

func (s *artifactObjectStore) StatObject(
	ctx context.Context,
	tenantID string,
	object ObjectRef,
) (ObjectInfo, error) {
	ref, expected, err := rawArtifactCoordinates(tenantID, object)
	if err != nil {
		return ObjectInfo{}, err
	}
	entry, err := s.store.Stat(ctx, ref)
	if err != nil {
		return ObjectInfo{}, mapArtifactError("stating raw object", err)
	}
	if entry.Identity != expected {
		return ObjectInfo{}, fmt.Errorf(
			"stating raw object: %w: semantic identity differs", ErrConflict,
		)
	}
	return objectInfoFromArtifact(entry), nil
}

func (s *artifactObjectStore) OpenObject(
	ctx context.Context,
	tenantID string,
	object ObjectRef,
) (ObjectInfo, VerifiedObjectReader, error) {
	ref, expected, err := rawArtifactCoordinates(tenantID, object)
	if err != nil {
		return ObjectInfo{}, nil, err
	}
	entry, reader, err := s.store.Open(ctx, ref)
	if err != nil {
		return ObjectInfo{}, nil, mapArtifactError("opening raw object", err)
	}
	if entry.Identity != expected {
		_ = reader.Close()
		return ObjectInfo{}, nil, fmt.Errorf(
			"opening raw object: %w: semantic identity differs", ErrConflict,
		)
	}
	return objectInfoFromArtifact(entry), reader, nil
}

func (s *artifactObjectStore) MissingObjects(
	ctx context.Context,
	tenantID string,
	objects []ObjectRef,
) ([]ObjectRef, error) {
	if err := validateOpaqueID("tenant", tenantID); err != nil {
		return nil, err
	}
	seen := make(map[string]ObjectRef, len(objects))
	unique := make([]ObjectRef, 0, len(objects))
	for _, object := range objects {
		validated, err := NewObjectRef(object.SHA256, object.Length)
		if err != nil || validated != object {
			return nil, fmt.Errorf("%w: invalid missing-object reference", ErrInvalid)
		}
		if previous, ok := seen[object.SHA256]; ok {
			if previous.Length != object.Length {
				return nil, fmt.Errorf("%w: digest has conflicting lengths", ErrConflict)
			}
			continue
		}
		seen[object.SHA256] = object
		unique = append(unique, object)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	missingAt := make([]bool, len(unique))
	var firstErr error
	var errorOnce sync.Once
	var workers sync.WaitGroup
	workerCount := min(rawObjectStatConcurrency, len(unique))
	for range workerCount {
		workers.Go(func() {
			for index := range jobs {
				_, err := s.StatObject(workCtx, tenantID, unique[index])
				switch {
				case errors.Is(err, ErrNotFound):
					missingAt[index] = true
				case err != nil:
					errorOnce.Do(func() {
						firstErr = err
						cancel()
					})
				}
			}
		})
	}
	for index := range unique {
		select {
		case jobs <- index:
		case <-workCtx.Done():
			close(jobs)
			workers.Wait()
			if firstErr != nil {
				return nil, firstErr
			}
			return nil, workCtx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	missing := make([]ObjectRef, 0)
	for index, object := range unique {
		if missingAt[index] {
			missing = append(missing, object)
		}
	}
	return missing, nil
}

func (s *artifactObjectStore) VerifyObjects(
	ctx context.Context,
	tenantID string,
	objects []ObjectRef,
) error {
	missing, err := s.MissingObjects(ctx, tenantID, objects)
	if err != nil {
		return err
	}
	if len(missing) != 0 {
		return fmt.Errorf("%w: %s", ErrMissingObject, missing[0].SHA256)
	}
	return nil
}

func (s *artifactObjectStore) PutManifest(
	ctx context.Context,
	manifest CanonicalManifest,
) (PutResult, error) {
	if err := ValidateCanonicalManifest(manifest); err != nil {
		return PutResult{}, err
	}
	origin, err := tenantArtifactOrigin(manifest.Identity.TenantID)
	if err != nil {
		return PutResult{}, err
	}
	ref, err := artifact.NewRef(origin, artifact.KindManifests, manifest.ManifestID+".json")
	if err != nil {
		return PutResult{}, mapArtifactError("constructing manifest reference", err)
	}
	identity, err := artifact.NewIdentity(manifest.ManifestID, int64(len(manifest.CanonicalJSON)))
	if err != nil {
		return PutResult{}, mapArtifactError("constructing manifest identity", err)
	}
	created, err := s.store.Create(
		ctx, ref, identity, "application/json", bytes.NewReader(manifest.CanonicalJSON),
	)
	if err != nil {
		return PutResult{}, mapArtifactCreateError("putting canonical manifest", err)
	}
	return putResultFromArtifact(created), nil
}

func (s *artifactObjectStore) OpenManifest(
	ctx context.Context,
	identity AuthIdentity,
	manifestID string,
) (ObjectInfo, VerifiedObjectReader, error) {
	canonical, err := NewAuthIdentity(identity.TenantID, identity.DeviceID)
	if err != nil || canonical != identity || !isCanonicalSHA256(manifestID) {
		return ObjectInfo{}, nil, fmt.Errorf("%w: invalid manifest identity", ErrInvalid)
	}
	origin, err := tenantArtifactOrigin(identity.TenantID)
	if err != nil {
		return ObjectInfo{}, nil, err
	}
	ref, err := artifact.NewRef(origin, artifact.KindManifests, manifestID+".json")
	if err != nil {
		return ObjectInfo{}, nil, mapArtifactError("constructing manifest reference", err)
	}
	entry, reader, err := s.store.Open(ctx, ref)
	if err != nil {
		return ObjectInfo{}, nil, mapArtifactError("opening canonical manifest", err)
	}
	return objectInfoFromArtifact(entry), reader, nil
}

func rawArtifactCoordinates(
	tenantID string,
	object ObjectRef,
) (artifact.Ref, artifact.Identity, error) {
	origin, err := tenantArtifactOrigin(tenantID)
	if err != nil {
		return artifact.Ref{}, artifact.Identity{}, err
	}
	validated, err := NewObjectRef(object.SHA256, object.Length)
	if err != nil || validated != object {
		return artifact.Ref{}, artifact.Identity{}, fmt.Errorf("%w: invalid object reference", ErrInvalid)
	}
	ref, err := artifact.NewRef(origin, artifact.KindRaw, object.SHA256)
	if err != nil {
		return artifact.Ref{}, artifact.Identity{}, mapArtifactError("constructing raw reference", err)
	}
	identity, err := artifact.NewIdentity(object.SHA256, object.Length)
	if err != nil {
		return artifact.Ref{}, artifact.Identity{}, mapArtifactError("constructing raw identity", err)
	}
	return ref, identity, nil
}

func tenantArtifactOrigin(tenantID string) (string, error) {
	if err := validateOpaqueID("tenant", tenantID); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(tenantID))
	return "tenant-" + hex.EncodeToString(sum[:]), nil
}

func objectInfoFromArtifact(entry artifact.Entry) ObjectInfo {
	return ObjectInfo{
		Ref: ObjectRef{
			SHA256: entry.Identity.SHA256,
			Length: entry.Identity.Size,
		},
		Modified: entry.Modified,
	}
}

func putResultFromArtifact(result artifact.CreateResult) PutResult {
	return PutResult{
		Info:    objectInfoFromArtifact(result.Entry),
		Created: result.Created,
	}
}

func mapArtifactError(operation string, err error) error {
	switch {
	case errors.Is(err, artifact.ErrArtifactNotFound):
		return fmt.Errorf("%s: %w: %w", operation, ErrNotFound, err)
	case errors.Is(err, artifact.ErrArtifactInvalid):
		return fmt.Errorf("%s: %w: %w", operation, ErrInvalid, err)
	case errors.Is(err, artifact.ErrArtifactConflict),
		errors.Is(err, artifact.ErrArtifactCorrupt):
		return fmt.Errorf("%s: %w: %w", operation, ErrConflict, err)
	default:
		return fmt.Errorf("%s: %w", operation, err)
	}
}

func mapArtifactCreateError(operation string, err error) error {
	if errors.Is(err, artifact.ErrArtifactInvalid) ||
		errors.Is(err, artifact.ErrArtifactConflict) ||
		errors.Is(err, artifact.ErrArtifactCorrupt) {
		return fmt.Errorf("%s: %w: %w", operation, ErrConflict, err)
	}
	return mapArtifactError(operation, err)
}
