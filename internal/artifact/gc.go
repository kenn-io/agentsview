package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const retentionPageSize = 256

// GCOptions configures conservative logical artifact retention. Store exposes
// canonical references; retention never inspects or removes physical files.
type GCOptions struct {
	Store           ArtifactStore
	Grace           time.Duration
	QuarantineGrace time.Duration
	DryRun          bool
	Now             time.Time
	Logf            func(string, ...any)
}

// GCResult summarizes one logical retention scan.
type GCResult struct {
	DryRun         bool
	Origins        int
	SkippedOrigins int
	Scanned        int
	Candidates     int
	Eligible       int
	KeptByGrace    int
	Deleted        int
	BytesEligible  int64
	BytesDeleted   int64

	QuarantinedScanned  int
	QuarantinedEligible int
	QuarantinedDeleted  int
	QuarantineSkipped   bool
}

type gcLiveRef struct {
	kind Kind
	name string
}

type gcOriginClosure map[gcLiveRef]struct{}

// GarbageCollect trashes canonical nodes unreachable from each origin's newest
// safe checkpoint. Origins without such a checkpoint are left untouched.
func GarbageCollect(ctx context.Context, opts GCOptions) (_ GCResult, retErr error) {
	if ctx == nil {
		return GCResult{}, fmt.Errorf("artifact gc context is required")
	}
	if opts.Store == nil {
		return GCResult{}, fmt.Errorf("artifact gc store is required")
	}
	if opts.Grace < 0 {
		return GCResult{}, fmt.Errorf("artifact gc grace must be >= 0")
	}
	if opts.QuarantineGrace < 0 {
		return GCResult{}, fmt.Errorf("artifact quarantine grace must be >= 0")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	result := GCResult{DryRun: opts.DryRun}

	origins, err := openStoreOriginIterator(ctx, opts.Store)
	if err != nil {
		return result, fmt.Errorf("listing artifact origins: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, origins.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		page, nextErr := origins.Next(ctx, retentionPageSize)
		if nextErr != nil && !errors.Is(nextErr, io.EOF) {
			return result, fmt.Errorf("listing artifact origins: %w", nextErr)
		}
		for _, origin := range page {
			result.Origins++
			closure, safe, err := collectGCOriginClosure(ctx, opts.Store, origin)
			if err != nil {
				return result, fmt.Errorf("scanning artifact origin %s: %w", origin, err)
			}
			if !safe {
				result.SkippedOrigins++
				logGC(opts, "artifact gc: skipping %s without a safe current checkpoint", origin)
				continue
			}
			if err := collectGCOrigin(ctx, opts, origin, closure, now, &result); err != nil {
				return result, fmt.Errorf("collecting artifact origin %s: %w", origin, err)
			}
		}
		if errors.Is(nextErr, io.EOF) {
			break
		}
	}
	if err := collectGCQuarantine(ctx, opts, now, &result); err != nil {
		return result, err
	}
	return result, nil
}

func collectGCOriginClosure(
	ctx context.Context, store ArtifactStore, origin string,
) (gcOriginClosure, bool, error) {
	latest, found, err := latestGCCheckpoint(ctx, store, origin)
	if err != nil || !found {
		return nil, false, err
	}
	closure := make(gcOriginClosure)
	if err := closure.indexCheckpoint(ctx, store, origin, latest); err != nil {
		if isUnsafeRetentionArtifact(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return closure, true, nil
}

func (c gcOriginClosure) indexCheckpoint(
	ctx context.Context, store ArtifactStore, origin string, latest Entry,
) error {
	data, err := readGCStoreArtifact(ctx, store, latest, checkpointDecodedLimit)
	if err != nil {
		return err
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return fmt.Errorf("%w: invalid checkpoint JSON", ErrArtifactInvalid)
	}
	canonical, err := canonicalJSON(cp)
	if err != nil || !bytes.Equal(canonical, data) {
		return fmt.Errorf("%w: checkpoint JSON is not canonical", ErrArtifactInvalid)
	}
	if err := validateCheckpoint(&cp, origin); err != nil {
		return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	if err := validateCheckpointSequenceIdentity(cp, latest.Ref.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrArtifactInvalid, err)
	}
	c[gcLiveRef{KindCheckpoints, latest.Ref.Name}] = struct{}{}
	manifestOwners := make(map[string]string, len(cp.Sessions))
	for gid, manifestHash := range cp.Sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if owner, found := manifestOwners[manifestHash]; found && owner != gid {
			return fmt.Errorf(
				"%w: manifest %s is referenced by multiple sessions", ErrArtifactInvalid, manifestHash)
		}
		manifestOwners[manifestHash] = gid
		manifestRef := Ref{Origin: origin, Kind: KindManifests, Name: manifestHash + ".json"}
		manifestEntry, err := store.Stat(ctx, manifestRef)
		if err != nil {
			return err
		}
		manifestData, err := readGCStoreArtifact(ctx, store, manifestEntry, manifestDecodedLimit)
		if err != nil {
			return err
		}
		if hashHex(manifestData) != manifestHash {
			return fmt.Errorf("%w: manifest hash mismatch", ErrArtifactInvalid)
		}
		manifest, err := decodeManifestWithLimits(manifestData, productionArtifactLimits())
		if err != nil || validateManifest(manifest, origin, gid) != nil {
			return fmt.Errorf("%w: invalid manifest", ErrArtifactInvalid)
		}
		if err := validateManifestReferencesWithLimits(manifest, productionArtifactLimits()); err != nil {
			return fmt.Errorf("%w: invalid manifest references: %v", ErrArtifactInvalid, err)
		}
		c[gcLiveRef{KindManifests, manifestRef.Name}] = struct{}{}
		for _, segmentHash := range manifest.Segments {
			key := gcLiveRef{KindSegments, segmentHash + ".ndjson"}
			if _, found := c[key]; found {
				continue
			}
			segmentRef := Ref{Origin: origin, Kind: key.kind, Name: key.name}
			entry, err := store.Stat(ctx, segmentRef)
			if err != nil {
				return err
			}
			segmentData, err := readGCStoreArtifact(ctx, store, entry, segmentDecodedLimit)
			if err != nil {
				return err
			}
			if hashHex(segmentData) != segmentHash {
				return fmt.Errorf("%w: segment hash mismatch", ErrArtifactInvalid)
			}
			if _, err := preflightSegmentData(segmentData, productionArtifactLimits()); err != nil {
				return fmt.Errorf("%w: invalid segment: %v", ErrArtifactInvalid, err)
			}
			c[key] = struct{}{}
		}
		if manifest.RawSource != nil && manifest.RawSource.Hash != "" {
			key := gcLiveRef{KindRaw, manifest.RawSource.Hash}
			if _, found := c[key]; found {
				continue
			}
			rawRef := Ref{Origin: origin, Kind: key.kind, Name: key.name}
			entry, err := store.Stat(ctx, rawRef)
			if err != nil {
				return err
			}
			if manifest.RawSource.Size != 0 && entry.Identity.Size != manifest.RawSource.Size {
				return fmt.Errorf("%w: raw source size mismatch", ErrArtifactInvalid)
			}
			if err := verifyGCStoreArtifact(ctx, store, entry); err != nil {
				return err
			}
			c[key] = struct{}{}
		}
	}
	return nil
}

func (c gcOriginClosure) contains(ctx context.Context, kind Kind, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, found := c[gcLiveRef{kind, name}]
	return found, nil
}

func latestGCCheckpoint(
	ctx context.Context, store ArtifactStore, origin string,
) (_ Entry, found bool, retErr error) {
	iterator, err := openStoreEntryIterator(ctx, store, origin, KindCheckpoints)
	if err != nil {
		return Entry{}, false, err
	}
	defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
	var latest Entry
	for {
		entries, nextErr := iterator.Next(ctx, retentionPageSize)
		if nextErr != nil && !errors.Is(nextErr, io.EOF) {
			return Entry{}, false, nextErr
		}
		for _, entry := range entries {
			if _, err := checkpointSequence(entry.Ref.Name); err == nil {
				latest, found = entry, true
			}
		}
		if errors.Is(nextErr, io.EOF) {
			return latest, found, nil
		}
	}
}

func collectGCOrigin(
	ctx context.Context,
	opts GCOptions,
	origin string,
	closure gcOriginClosure,
	now time.Time,
	result *GCResult,
) error {
	for _, kind := range []Kind{KindCheckpoints, KindManifests, KindSegments, KindRaw} {
		iterator, err := openStoreEntryIterator(ctx, opts.Store, origin, kind)
		if err != nil {
			return err
		}
		for {
			entries, nextErr := iterator.Next(ctx, retentionPageSize)
			if nextErr != nil && !errors.Is(nextErr, io.EOF) {
				return errors.Join(nextErr, iterator.Close())
			}
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return errors.Join(err, iterator.Close())
				}
				result.Scanned++
				live, err := closure.contains(ctx, kind, entry.Ref.Name)
				if err != nil {
					return errors.Join(err, iterator.Close())
				}
				if live {
					continue
				}
				result.Candidates++
				if entry.Modified.Add(opts.Grace).After(now) {
					result.KeptByGrace++
					continue
				}
				result.Eligible++
				result.BytesEligible += entry.Identity.Size
				if opts.DryRun {
					logGC(opts, "artifact gc: would trash %s/%s/%s (%d bytes)",
						entry.Ref.Origin, entry.Ref.Kind, entry.Ref.Name, entry.Identity.Size)
					continue
				}
				if err := opts.Store.Trash(ctx, entry.Ref); err != nil {
					if errors.Is(err, ErrArtifactNotFound) {
						continue
					}
					return errors.Join(err, iterator.Close())
				}
				result.Deleted++
				result.BytesDeleted += entry.Identity.Size
				logGC(opts, "artifact gc: trashed %s/%s/%s (%d bytes)",
					entry.Ref.Origin, entry.Ref.Kind, entry.Ref.Name, entry.Identity.Size)
			}
			if errors.Is(nextErr, io.EOF) {
				break
			}
		}
		if err := iterator.Close(); err != nil {
			return err
		}
	}
	return nil
}

func readGCStoreArtifact(
	ctx context.Context, store ArtifactStore, entry Entry, limit int64,
) (_ []byte, retErr error) {
	actual, reader, err := store.Open(ctx, entry.Ref)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	if actual.Identity != entry.Identity || actual.Ref != entry.Ref {
		return nil, fmt.Errorf("%w: artifact changed during retention read", ErrArtifactCorrupt)
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: artifact exceeds retention read limit", ErrArtifactCorrupt)
	}
	if err := reader.Verify(); err != nil {
		return nil, err
	}
	return data, nil
}

func verifyGCStoreArtifact(
	ctx context.Context, store ArtifactStore, entry Entry,
) (retErr error) {
	actual, reader, err := store.Open(ctx, entry.Ref)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	if actual.Identity != entry.Identity || actual.Ref != entry.Ref {
		return fmt.Errorf("%w: artifact changed during retention read", ErrArtifactCorrupt)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return err
	}
	return reader.Verify()
}

func isUnsafeRetentionArtifact(err error) bool {
	return errors.Is(err, ErrArtifactNotFound) ||
		errors.Is(err, ErrArtifactCorrupt) ||
		errors.Is(err, ErrArtifactUnsupported) ||
		errors.Is(err, ErrArtifactInvalid)
}

func collectGCQuarantine(
	ctx context.Context, opts GCOptions, now time.Time, result *GCResult,
) (retErr error) {
	quarantine, ok := opts.Store.(ArtifactQuarantineStore)
	if !ok {
		result.QuarantineSkipped = true
		logGC(opts, "artifact gc: quarantine retention unsupported by store")
		return nil
	}
	iterator, err := quarantine.Quarantined(ctx)
	if err != nil {
		return fmt.Errorf("listing artifact quarantine: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, iterator.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		items, nextErr := iterator.Next(ctx, retentionPageSize)
		if nextErr != nil && !errors.Is(nextErr, io.EOF) {
			return fmt.Errorf("listing artifact quarantine: %w", nextErr)
		}
		for _, entry := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			result.QuarantinedScanned++
			if entry.Modified.Add(opts.QuarantineGrace).After(now) {
				continue
			}
			result.QuarantinedEligible++
			if opts.DryRun {
				continue
			}
			if err := quarantine.TrashQuarantined(ctx, entry.Token); err != nil {
				if errors.Is(err, ErrArtifactNotFound) {
					continue
				}
				return fmt.Errorf("trashing artifact quarantine: %w", err)
			}
			result.QuarantinedDeleted++
		}
		if errors.Is(nextErr, io.EOF) {
			return nil
		}
	}
}

// QuarantinedEntry names one hidden logical artifact retained for diagnosis.
type QuarantinedEntry struct {
	Token    string
	Ref      Ref
	Identity Identity
	Modified time.Time
}

// ArtifactQuarantineStore is an optional stable-page view of hidden quarantine
// nodes. Tokens are opaque and may only be passed back to TrashQuarantined.
type ArtifactQuarantineStore interface {
	Quarantined(context.Context) (QuarantineIterator, error)
	TrashQuarantined(context.Context, string) error
}

func logGC(opts GCOptions, format string, args ...any) {
	if opts.Logf != nil {
		opts.Logf(format, args...)
	}
}
