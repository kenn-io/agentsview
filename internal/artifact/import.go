package artifact

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"go.kenn.io/agentsview/internal/db"
)

const artifactImportDrainLimit = 128

type ImportResult struct {
	Sessions    int
	Messages    int
	Deferred    int
	Quarantined int
	More        bool
}

type importCoordinatorHooks struct {
	afterPeerHead     func() error
	afterSessionWrite func() error
	afterProvenance   func() error
	afterLanding      func() error
	observePending    func(limit, count int)
	observeProvenance func(count int)
}

type StoreImportCoordinator struct {
	database    *db.DB
	store       ArtifactStore
	localOrigin string
	hooks       *importCoordinatorHooks

	runMu                   sync.Mutex
	signalMu                sync.Mutex
	generation              uint64
	completed               uint64
	activeAttemptGeneration int64
}

func NewStoreImportCoordinator(
	database *db.DB, store ArtifactStore, localOrigin string,
) *StoreImportCoordinator {
	return &StoreImportCoordinator{
		database: database, store: store, localOrigin: localOrigin,
		generation: 1,
	}
}

func (c *StoreImportCoordinator) requestDrain() {
	c.signalMu.Lock()
	c.generation++
	c.signalMu.Unlock()
}

func (c *StoreImportCoordinator) RecordChanged(
	ctx context.Context, entry Entry,
) error {
	if c == nil || c.database == nil || c.store == nil {
		return errors.New("artifact import coordinator is required")
	}
	if err := validateStoreRef(entry.Ref); err != nil {
		return err
	}
	if err := validateStoreIdentity(entry.Identity); err != nil {
		return err
	}
	if err := validateRefIdentity(entry.Ref, entry.Identity); err != nil {
		return err
	}
	if entry.Ref.Origin == c.localOrigin {
		return nil
	}
	if entry.Ref.Kind != KindCheckpoints {
		c.requestDrain()
		return nil
	}
	sequence, err := checkpointSequence(entry.Ref.Name)
	if err != nil {
		return err
	}
	advanced, err := c.database.RecordArtifactPeerCheckpointHead(
		ctx,
		db.ArtifactPeerCheckpointHead{
			Origin: entry.Ref.Origin, Sequence: sequence,
			CheckpointSHA256: entry.Identity.SHA256,
			CheckpointSize:   entry.Identity.Size,
		},
	)
	if err != nil {
		return err
	}
	if advanced && c.hooks != nil && c.hooks.afterPeerHead != nil {
		if err := c.hooks.afterPeerHead(); err != nil {
			return err
		}
	}
	if !advanced {
		head, found, err := c.database.GetArtifactPeerCheckpointHead(
			ctx, entry.Ref.Origin,
		)
		if err != nil {
			return err
		}
		if found && head.Sequence > sequence {
			return nil
		}
	}
	err = c.database.EnqueueArtifactImport(ctx, db.ArtifactImportWork{
		Origin: entry.Ref.Origin, Kind: string(entry.Ref.Kind),
		Name: entry.Ref.Name, SHA256: entry.Identity.SHA256,
		Size:                      entry.Identity.Size,
		RequiredCheckpointVersion: checkpointFormatVersion,
		RequiredManifestVersion:   manifestFormatVersion,
		RequiredSegmentVersion:    messageSegmentFormatVersion,
	})
	if err != nil {
		return err
	}
	c.requestDrain()
	return nil
}

func (c *StoreImportCoordinator) Finalize(
	ctx context.Context,
) (ImportResult, error) {
	var result ImportResult
	if c == nil || c.database == nil || c.store == nil {
		return result, errors.New("artifact import coordinator is required")
	}
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}

	c.signalMu.Lock()
	drainGeneration := c.generation
	completed := c.completed
	c.signalMu.Unlock()
	if c.activeAttemptGeneration == 0 {
		if completed >= drainGeneration {
			return result, nil
		}
		attempt, err := c.database.ReserveArtifactImportAttemptGeneration(ctx)
		if err != nil {
			return result, err
		}
		c.activeAttemptGeneration = attempt
	}
	work, err := c.database.PendingArtifactImports(
		ctx,
		db.ArtifactImportVersions{
			Checkpoint: checkpointFormatVersion,
			Manifest:   manifestFormatVersion,
			Segment:    messageSegmentFormatVersion,
		},
		c.activeAttemptGeneration,
		artifactImportDrainLimit,
	)
	if err != nil {
		return result, err
	}
	if c.hooks != nil && c.hooks.observePending != nil {
		c.hooks.observePending(artifactImportDrainLimit, len(work))
	}
	for _, claim := range work {
		if err := c.processImportClaim(ctx, claim, &result); err != nil {
			return result, err
		}
	}
	if len(work) == artifactImportDrainLimit {
		result.More = true
		return result, nil
	}
	c.activeAttemptGeneration = 0
	c.signalMu.Lock()
	if drainGeneration > c.completed {
		c.completed = drainGeneration
	}
	result.More = c.generation > drainGeneration
	c.signalMu.Unlock()
	return result, nil
}

func (c *StoreImportCoordinator) processImportClaim(
	ctx context.Context,
	work db.ArtifactImportWork,
	result *ImportResult,
) error {
	sequence, err := checkpointSequence(work.Name)
	if err != nil {
		return err
	}
	head, found, err := c.database.GetArtifactPeerCheckpointHead(ctx, work.Origin)
	if err != nil {
		return err
	}
	if found && head.Sequence > sequence {
		_, err := c.database.AcknowledgeArtifactImport(ctx, work)
		return err
	}
	landing, _, landed, err := c.database.GetArtifactCheckpointLanding(
		ctx, work.Origin,
	)
	if err != nil {
		return err
	}
	if landed &&
		landing.Sequence == sequence &&
		landing.CheckpointSHA256 == work.SHA256 &&
		landing.CheckpointSize == work.Size {
		_, err := c.database.AcknowledgeArtifactImport(ctx, work)
		return err
	}

	ref, err := NewRef(work.Origin, KindCheckpoints, work.Name)
	if err != nil {
		return err
	}
	entry := Entry{
		Ref: ref,
		Identity: Identity{
			SHA256: work.SHA256,
			Size:   work.Size,
		},
	}
	body, err := readVerifiedImportArtifact(
		ctx, c.store, entry, checkpointDecodedLimit,
	)
	if err != nil {
		if isInvalidImportDependencyError(err) {
			return c.quarantineCheckpoint(ctx, work, ref, result)
		}
		return err
	}
	checkpoint, err := decodeImportCheckpoint(
		body, work.Origin, work.Name,
	)
	if err != nil {
		var future *futureArtifactVersionError
		if errors.As(err, &future) {
			updated := work
			updated.RequiredCheckpointVersion = max(
				updated.RequiredCheckpointVersion, future.Version,
			)
			if err := c.deferImportClaim(ctx, updated); err != nil {
				return err
			}
			result.Deferred++
			return nil
		}
		if errors.Is(err, ErrArtifactInvalid) {
			return c.quarantineCheckpoint(ctx, work, ref, result)
		}
		return err
	}
	return c.importCheckpointSessions(ctx, work, entry, checkpoint, result)
}

func (c *StoreImportCoordinator) importCheckpointSessions(
	ctx context.Context,
	work db.ArtifactImportWork,
	entry Entry,
	checkpoint importCheckpoint,
	result *ImportResult,
) error {
	gids := make([]string, 0, len(checkpoint.Sessions))
	for gid := range checkpoint.Sessions {
		gids = append(gids, gid)
	}
	slices.Sort(gids)
	provenance := make(map[string]string, len(gids))
	for start := 0; start < len(gids); start += 1024 {
		end := min(start+1024, len(gids))
		page, err := c.database.ArtifactImportedManifestHashes(
			ctx, work.Origin, gids[start:end],
		)
		if err != nil {
			return err
		}
		if c.hooks != nil && c.hooks.observeProvenance != nil {
			c.hooks.observeProvenance(end - start)
		}
		maps.Copy(provenance, page)
	}

	deferred := false
	updatedWork := work
	for _, gid := range gids {
		manifestHash := checkpoint.Sessions[gid]
		if provenance[gid] == manifestHash {
			continue
		}
		write, outcome, err := loadImportedSession(
			ctx, c.database, c.store, work.Origin,
			gid, manifestHash, productionArtifactLimits(),
		)
		if err != nil {
			var future *futureArtifactVersionError
			if errors.As(err, &future) {
				switch future.Kind {
				case KindManifests:
					updatedWork.RequiredManifestVersion = max(
						updatedWork.RequiredManifestVersion,
						future.Version,
					)
				case KindSegments:
					updatedWork.RequiredSegmentVersion = max(
						updatedWork.RequiredSegmentVersion,
						future.Version,
					)
				default:
					return err
				}
				deferred = true
				continue
			}
			return err
		}
		switch outcome {
		case importClosureDeferred:
			deferred = true
			continue
		case importClosureInvalid:
			deferred = true
			result.Quarantined++
			continue
		case importClosureComplete:
		default:
			return errors.New("artifact import closure returned invalid outcome")
		}
		batch, err := c.database.WriteSessionBatchAtomic(
			[]db.SessionBatchWrite{write},
		)
		switch {
		case err == nil:
			result.Sessions += batch.WrittenSessions
			result.Messages += batch.WrittenMessages
			if c.hooks != nil && c.hooks.afterSessionWrite != nil {
				if err := c.hooks.afterSessionWrite(); err != nil {
					return err
				}
			}
		case errors.Is(err, db.ErrSessionExcluded),
			errors.Is(err, db.ErrSessionTrashed):
		default:
			return err
		}
		if err := c.database.RecordArtifactImportedSession(
			ctx, db.ArtifactImportedSession{
				Origin: work.Origin, GID: gid,
				ManifestHash:      manifestHash,
				ImportedSessionID: gid,
			},
		); err != nil {
			return err
		}
		if c.hooks != nil && c.hooks.afterProvenance != nil {
			if err := c.hooks.afterProvenance(); err != nil {
				return err
			}
		}
	}
	if deferred {
		if err := c.deferImportClaim(ctx, updatedWork); err != nil {
			return err
		}
		result.Deferred++
		return nil
	}
	if err := c.database.RecordArtifactCheckpointLanding(
		ctx,
		db.ArtifactCheckpointLanding{
			Origin: work.Origin, Sequence: checkpoint.Sequence,
			CheckpointSHA256: entry.Identity.SHA256,
			CheckpointSize:   entry.Identity.Size,
		},
		checkpoint.Sessions,
	); err != nil {
		return err
	}
	if c.hooks != nil && c.hooks.afterLanding != nil {
		if err := c.hooks.afterLanding(); err != nil {
			return err
		}
	}
	_, err := c.database.AcknowledgeArtifactImport(ctx, work)
	return err
}

func (c *StoreImportCoordinator) deferImportClaim(
	ctx context.Context, work db.ArtifactImportWork,
) error {
	if err := c.database.EnqueueArtifactImport(ctx, work); err != nil {
		return err
	}
	marked, err := c.database.MarkArtifactImportAttempted(
		ctx, work, c.activeAttemptGeneration,
	)
	if err != nil {
		return err
	}
	if !marked {
		return fmt.Errorf(
			"%w: artifact import claim changed while deferring",
			db.ErrArtifactImportConflict,
		)
	}
	return nil
}

func (c *StoreImportCoordinator) quarantineCheckpoint(
	ctx context.Context,
	work db.ArtifactImportWork,
	ref Ref,
	result *ImportResult,
) error {
	if err := c.store.Quarantine(
		ctx, ref, "invalid import checkpoint",
	); err != nil {
		return err
	}
	_, err := c.database.AcknowledgeArtifactImport(ctx, work)
	if err != nil {
		return err
	}
	result.Quarantined++
	return nil
}
