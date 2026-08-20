package artifact

import (
	"context"
	"errors"
	"fmt"
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
	afterPrune        func() error
	observePending    func(limit, count int)
	observeProvenance func(count int)
	observeStage      func(count int)
}

type StoreImportCoordinator struct {
	database    *db.DB
	store       ArtifactStore
	localOrigin string
	hooks       *importCoordinatorHooks
	checkpoint  *retainedImportCheckpoint
	// Future requirements stay provisional until the active attempt has
	// visited every staged row. Attempt generations are durable, so a
	// restarted coordinator re-observes them before raising the queue gate.
	future map[importClaimKey]db.ArtifactImportVersions

	runMu                   sync.Mutex
	signalMu                sync.Mutex
	generation              uint64
	completed               uint64
	activeAttemptGeneration int64
	activeSignalGeneration  uint64
}

type importClaimKey struct {
	origin     string
	name       string
	sha256     string
	size       int64
	enqueuedAt string
}

type retainedImportCheckpoint struct {
	origin   string
	name     string
	sha256   string
	size     int64
	header   importCheckpoint
	sessions importCheckpointSessionStream
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
			_, more, err := c.database.PruneArtifactCheckpointStages(
				ctx, artifactImportDrainLimit,
			)
			if err != nil {
				return result, err
			}
			if c.hooks != nil && c.hooks.afterPrune != nil {
				if err := c.hooks.afterPrune(); err != nil {
					return result, err
				}
			}
			c.signalMu.Lock()
			result.More = more || c.generation > drainGeneration
			c.signalMu.Unlock()
			return result, nil
		}
		attempt, err := c.database.ReserveArtifactImportAttemptGeneration(ctx)
		if err != nil {
			return result, err
		}
		c.activeAttemptGeneration = attempt
		c.activeSignalGeneration = drainGeneration
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
	sessionBudget := artifactImportDrainLimit
	for _, claim := range work {
		if err := c.processImportClaim(
			ctx, claim, &sessionBudget, &result,
		); err != nil {
			return result, err
		}
		if result.More && sessionBudget == 0 {
			break
		}
	}
	_, pruneMore, err := c.database.PruneArtifactCheckpointStages(
		ctx, artifactImportDrainLimit,
	)
	if err != nil {
		return result, err
	}
	result.More = result.More || pruneMore
	if result.More || len(work) == artifactImportDrainLimit {
		result.More = true
		return result, nil
	}
	completedGeneration := c.activeSignalGeneration
	c.activeAttemptGeneration = 0
	c.activeSignalGeneration = 0
	c.signalMu.Lock()
	if completedGeneration > c.completed {
		c.completed = completedGeneration
	}
	result.More = c.generation > completedGeneration
	c.signalMu.Unlock()
	return result, nil
}

func (c *StoreImportCoordinator) processImportClaim(
	ctx context.Context,
	work db.ArtifactImportWork,
	sessionBudget *int,
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
		c.discardFutureRequirements(work)
		_, err := c.database.AcknowledgeArtifactImport(ctx, work)
		return err
	}
	landing, landed, err := c.database.GetArtifactCheckpointLandingIdentity(
		ctx, work.Origin,
	)
	if err != nil {
		return err
	}
	if landed &&
		landing.Sequence == sequence &&
		landing.CheckpointSHA256 == work.SHA256 &&
		landing.CheckpointSize == work.Size {
		c.discardFutureRequirements(work)
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
	landingIdentity := db.ArtifactCheckpointLanding{
		Origin: work.Origin, Sequence: sequence,
		CheckpointSHA256: entry.Identity.SHA256,
		CheckpointSize:   entry.Identity.Size,
	}
	if work.QuarantinePending {
		return c.finishCheckpointQuarantine(ctx, work, ref, result)
	}
	complete, err := c.database.ArtifactCheckpointStageComplete(
		ctx, landingIdentity,
	)
	if err != nil {
		return err
	}
	if complete {
		c.discardRetainedCheckpoint(work)
		return c.importCheckpointSessions(
			ctx, work, landingIdentity, sessionBudget, result,
		)
	}
	checkpoint, sessionsRaw, err := c.loadImportCheckpoint(ctx, work, entry)
	if err != nil {
		if future, ok := errors.AsType[*futureArtifactVersionError](err); ok {
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
		if isInvalidImportDependencyError(err) {
			c.discardRetainedCheckpoint(work)
			return c.quarantineCheckpoint(ctx, work, ref, result)
		}
		if errors.Is(err, ErrArtifactNotFound) {
			if err := c.deferImportClaim(ctx, work); err != nil {
				return err
			}
			result.Deferred++
			return nil
		}
		return err
	}
	if checkpoint.Sequence != landingIdentity.Sequence {
		return fmt.Errorf(
			"%w: decoded checkpoint sequence changed",
			db.ErrArtifactImportConflict,
		)
	}
	if err := c.database.BeginArtifactCheckpointStage(
		ctx, landingIdentity, checkpointFormatVersion,
	); err != nil {
		return err
	}
	complete, err = c.database.ArtifactCheckpointStageComplete(
		ctx, landingIdentity,
	)
	if err != nil {
		return err
	}
	if !complete {
		state, stateErr := c.database.ArtifactCheckpointStageProgress(
			ctx, landingIdentity,
		)
		if stateErr != nil {
			return stateErr
		}
		if state.DecodeOffset == int64(len(sessionsRaw.data)) {
			if err := c.database.CompleteArtifactCheckpointStage(
				ctx, landingIdentity, state.DecodedCount,
			); err != nil {
				if errors.Is(err, db.ErrArtifactImportConflict) {
					return c.quarantineCheckpoint(ctx, work, ref, result)
				}
				return err
			}
			c.discardRetainedCheckpoint(work)
			return c.importCheckpointSessions(
				ctx, work, landingIdentity, sessionBudget, result,
			)
		}
		if *sessionBudget == 0 {
			result.More = true
			return nil
		}
		page, nextOffset, done, decodeErr :=
			decodeImportCheckpointSessionPage(
				sessionsRaw, work.Origin, state.DecodeOffset, *sessionBudget,
			)
		if decodeErr != nil {
			if future, ok := errors.AsType[*futureArtifactVersionError](decodeErr); ok {
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
			if errors.Is(decodeErr, ErrArtifactInvalid) {
				return c.quarantineCheckpoint(ctx, work, ref, result)
			}
			return decodeErr
		}
		entries := make([]db.ArtifactCheckpointSession, len(page))
		for i, session := range page {
			entries[i] = db.ArtifactCheckpointSession{
				GID:          session.GID,
				ManifestHash: session.ManifestHash,
			}
		}
		if err := c.database.StageArtifactCheckpointSessionPage(
			ctx, landingIdentity, entries,
			state.DecodeOffset, nextOffset,
		); err != nil {
			if errors.Is(err, db.ErrArtifactImportConflict) {
				return c.quarantineCheckpoint(ctx, work, ref, result)
			}
			return err
		}
		*sessionBudget -= len(entries)
		if c.hooks != nil && c.hooks.observeStage != nil {
			c.hooks.observeStage(len(entries))
		}
		if !done {
			result.More = true
			return nil
		}
		if err := c.database.CompleteArtifactCheckpointStage(
			ctx, landingIdentity, state.DecodedCount+len(entries),
		); err != nil {
			if errors.Is(err, db.ErrArtifactImportConflict) {
				return c.quarantineCheckpoint(ctx, work, ref, result)
			}
			return err
		}
		c.discardRetainedCheckpoint(work)
	}
	return c.importCheckpointSessions(
		ctx, work, landingIdentity, sessionBudget, result,
	)
}

func (c *StoreImportCoordinator) loadImportCheckpoint(
	ctx context.Context,
	work db.ArtifactImportWork,
	entry Entry,
) (importCheckpoint, importCheckpointSessionStream, error) {
	if cached := c.checkpoint; cached != nil &&
		cached.origin == work.Origin &&
		cached.name == work.Name &&
		cached.sha256 == work.SHA256 &&
		cached.size == work.Size {
		return cached.header, cached.sessions, nil
	}
	c.checkpoint = nil
	body, err := readVerifiedImportArtifact(
		ctx, c.store, entry, checkpointDecodedLimit,
	)
	if err != nil {
		return importCheckpoint{}, importCheckpointSessionStream{}, err
	}
	header, sessions, err := decodeImportCheckpointHeader(
		body, work.Origin, work.Name,
	)
	if err != nil {
		return importCheckpoint{}, importCheckpointSessionStream{}, err
	}
	c.checkpoint = &retainedImportCheckpoint{
		origin: work.Origin, name: work.Name, sha256: work.SHA256, size: work.Size,
		header: header, sessions: sessions,
	}
	return header, sessions, nil
}

func (c *StoreImportCoordinator) discardRetainedCheckpoint(
	work db.ArtifactImportWork,
) {
	if cached := c.checkpoint; cached != nil &&
		cached.origin == work.Origin &&
		cached.name == work.Name &&
		cached.sha256 == work.SHA256 &&
		cached.size == work.Size {
		c.checkpoint = nil
	}
}

func (c *StoreImportCoordinator) importCheckpointSessions(
	ctx context.Context,
	work db.ArtifactImportWork,
	landing db.ArtifactCheckpointLanding,
	sessionBudget *int,
	result *ImportResult,
) error {
	if sessionBudget == nil || *sessionBudget < 0 {
		return errors.New("artifact import session budget is invalid")
	}
	if *sessionBudget == 0 {
		pending, err := c.database.ArtifactCheckpointStageHasPending(ctx, landing)
		if err != nil {
			return err
		}
		if pending {
			result.More = true
			return nil
		}
	}

	pending, err := c.database.PendingArtifactCheckpointSessions(
		ctx, landing, c.activeAttemptGeneration, max(*sessionBudget, 1),
	)
	if err != nil {
		return err
	}
	if c.hooks != nil && c.hooks.observeProvenance != nil {
		c.hooks.observeProvenance(len(pending))
	}
	deferred := false
	for _, staged := range pending {
		*sessionBudget--
		write, outcome, err := loadImportedSession(
			ctx, c.database, c.store, work.Origin,
			staged.GID, staged.ManifestHash, productionArtifactLimits(),
		)
		if err != nil {
			if future, ok := errors.AsType[*futureArtifactVersionError](err); ok {
				if err := c.rememberFutureRequirement(work, future); err != nil {
					return err
				}
				deferred = true
				if err := c.markCheckpointSessionAttempted(
					ctx, landing, staged,
				); err != nil {
					return err
				}
				continue
			}
			return err
		}
		switch outcome {
		case importClosureDeferred:
			deferred = true
			if err := c.markCheckpointSessionAttempted(
				ctx, landing, staged,
			); err != nil {
				return err
			}
			continue
		case importClosureInvalid:
			deferred = true
			result.Quarantined++
			if err := c.markCheckpointSessionAttempted(
				ctx, landing, staged,
			); err != nil {
				return err
			}
			continue
		case importClosureComplete:
		default:
			return errors.New("artifact import closure returned invalid outcome")
		}
		applied, err := c.database.ApplyStagedArtifactImportedSession(
			ctx, landing, staged,
			db.ArtifactImportedSession{
				Origin: work.Origin, GID: staged.GID,
				ManifestHash:      staged.ManifestHash,
				ImportedSessionID: staged.GID,
			},
			write,
		)
		if errors.Is(err, db.ErrSessionTrashed) {
			deferred = true
			if err := c.markCheckpointSessionAttempted(
				ctx, landing, staged,
			); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if applied.Written {
			result.Sessions++
			result.Messages += applied.WrittenMessages
			if c.hooks != nil && c.hooks.afterSessionWrite != nil {
				if err := c.hooks.afterSessionWrite(); err != nil {
					return err
				}
			}
		}
		if c.hooks != nil && c.hooks.afterProvenance != nil {
			if err := c.hooks.afterProvenance(); err != nil {
				return err
			}
		}
	}
	hasPending, err := c.database.ArtifactCheckpointStageHasPending(ctx, landing)
	if err != nil {
		return err
	}
	if hasPending {
		if *sessionBudget == 0 {
			result.More = true
			if deferred {
				result.Deferred++
			}
			return nil
		}
		if err := c.deferImportClaim(
			ctx, c.takeFutureRequirements(work),
		); err != nil {
			return err
		}
		result.Deferred++
		return nil
	}
	if err := c.database.RecordArtifactCheckpointLandingFromStage(
		ctx, landing,
	); err != nil {
		if !errors.Is(err, db.ErrArtifactImportConflict) {
			return err
		}
		head, found, headErr := c.database.GetArtifactPeerCheckpointHead(
			ctx, work.Origin,
		)
		if headErr != nil {
			return headErr
		}
		if !found || head.Sequence <= landing.Sequence {
			return err
		}
		c.discardFutureRequirements(work)
		if _, ackErr := c.database.AcknowledgeArtifactImport(
			ctx, work,
		); ackErr != nil {
			return ackErr
		}
		result.More = true
		return nil
	}
	if c.hooks != nil && c.hooks.afterLanding != nil {
		if err := c.hooks.afterLanding(); err != nil {
			return err
		}
	}
	c.discardFutureRequirements(work)
	_, err = c.database.AcknowledgeArtifactImport(ctx, work)
	return err
}

func (c *StoreImportCoordinator) rememberFutureRequirement(
	work db.ArtifactImportWork,
	future *futureArtifactVersionError,
) error {
	if future == nil {
		return errors.New("future artifact version is required")
	}
	if c.future == nil {
		c.future = make(map[importClaimKey]db.ArtifactImportVersions)
	}
	key := artifactImportClaimKey(work)
	requirements := c.future[key]
	switch future.Kind {
	case KindManifests:
		requirements.Manifest = max(requirements.Manifest, future.Version)
	case KindSegments:
		requirements.Segment = max(requirements.Segment, future.Version)
	default:
		return fmt.Errorf("unsupported future artifact kind %s", future.Kind)
	}
	c.future[key] = requirements
	return nil
}

func (c *StoreImportCoordinator) takeFutureRequirements(
	work db.ArtifactImportWork,
) db.ArtifactImportWork {
	key := artifactImportClaimKey(work)
	requirements := c.future[key]
	delete(c.future, key)
	work.RequiredManifestVersion = max(
		work.RequiredManifestVersion, requirements.Manifest,
	)
	work.RequiredSegmentVersion = max(
		work.RequiredSegmentVersion, requirements.Segment,
	)
	return work
}

func (c *StoreImportCoordinator) discardFutureRequirements(
	work db.ArtifactImportWork,
) {
	delete(c.future, artifactImportClaimKey(work))
}

func artifactImportClaimKey(work db.ArtifactImportWork) importClaimKey {
	return importClaimKey{
		origin: work.Origin, name: work.Name, sha256: work.SHA256,
		size: work.Size, enqueuedAt: work.EnqueuedAt,
	}
}

func (c *StoreImportCoordinator) markCheckpointSessionAttempted(
	ctx context.Context,
	landing db.ArtifactCheckpointLanding,
	session db.ArtifactCheckpointSession,
) error {
	marked, err := c.database.MarkArtifactCheckpointSessionAttempted(
		ctx, landing, session, c.activeAttemptGeneration,
	)
	if err != nil {
		return err
	}
	if !marked {
		return fmt.Errorf(
			"%w: staged artifact session changed while deferring",
			db.ErrArtifactImportConflict,
		)
	}
	return nil
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
	marked, err := c.database.MarkArtifactImportQuarantinePending(ctx, work)
	if err != nil {
		return err
	}
	if !marked {
		return fmt.Errorf(
			"%w: artifact import claim changed before quarantine",
			db.ErrArtifactImportConflict,
		)
	}
	work.QuarantinePending = true
	return c.finishCheckpointQuarantine(ctx, work, ref, result)
}

func (c *StoreImportCoordinator) finishCheckpointQuarantine(
	ctx context.Context,
	work db.ArtifactImportWork,
	ref Ref,
	result *ImportResult,
) error {
	err := c.store.Quarantine(
		ctx, ref, "invalid import checkpoint",
	)
	if err != nil && !errors.Is(err, ErrArtifactNotFound) {
		return err
	}
	c.discardFutureRequirements(work)
	_, err = c.database.AcknowledgeArtifactImportAndDiscardStage(ctx, work)
	if err != nil {
		return err
	}
	result.Quarantined++
	return nil
}
