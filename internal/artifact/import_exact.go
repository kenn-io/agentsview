package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

const artifactImportDrainLimit = 128

const (
	artifactImportReasonCheckpoint = "checkpoint dependencies incomplete"
	artifactImportReasonMetadata   = "metadata target unavailable"
)

func artifactImportWork(entry Entry, reason string, requiredVersion int) db.ArtifactImportWork {
	return db.ArtifactImportWork{
		Origin: entry.Ref.Origin, Kind: string(entry.Ref.Kind), Name: entry.Ref.Name,
		SHA256: entry.Identity.SHA256, Size: entry.Identity.Size,
		Reason: reason, RequiredFormatVersion: requiredVersion,
	}
}

// RecordChanged records only protocol references that can drive an import.
// Dependency arrivals merely request a bounded retry of already queued work.
func (c *StoreImportCoordinator) RecordChanged(ctx context.Context, entry Entry) error {
	if c == nil || c.database == nil || c.store == nil {
		return errors.New("artifact import coordinator is required")
	}
	if err := validateRefIdentity(entry.Ref, entry.Identity); err != nil {
		return err
	}
	if entry.Ref.Origin == c.localOrigin {
		return nil
	}
	switch entry.Ref.Kind {
	case KindCheckpoints:
		sequence, err := checkpointSequence(entry.Ref.Name)
		if err != nil {
			return err
		}
		head := db.ArtifactPeerCheckpointHead{
			Origin: entry.Ref.Origin, Sequence: sequence,
			CheckpointSHA256: entry.Identity.SHA256, CheckpointSize: entry.Identity.Size,
		}
		if err := c.database.RecordArtifactPeerCheckpointHead(ctx, head); err != nil {
			current, found, readErr := c.database.GetArtifactPeerCheckpointHead(ctx, entry.Ref.Origin)
			if readErr != nil {
				return errors.Join(err, readErr)
			}
			if !found || current.Sequence <= sequence {
				return err
			}
			return c.requestDrain()
		}
		if err := c.database.EnqueueArtifactImport(ctx,
			artifactImportWork(entry, artifactImportReasonCheckpoint, formatVersion)); err != nil {
			return err
		}
	case KindMeta:
		if err := c.database.EnqueueArtifactImport(ctx,
			artifactImportWork(entry, artifactImportReasonMetadata, formatVersion)); err != nil {
			return err
		}
	}
	return c.requestDrain()
}

func (c *StoreImportCoordinator) drainQueuedImports(
	ctx context.Context,
) (ImportResult, error) {
	work, err := c.database.PendingArtifactImports(ctx, formatVersion, artifactImportDrainLimit)
	if err != nil {
		return ImportResult{}, err
	}
	result := ImportResult{}
	clock := NewHLCClock(c.database, HLCClockOptions{Now: c.now})
	// Content checkpoints must land before metadata from the same bounded
	// page. Wire ordering places metadata ahead of checkpoints, while metadata
	// projections may target sessions introduced by those checkpoints.
	for _, phase := range []Kind{KindCheckpoints, KindMeta} {
		for _, item := range work {
			if Kind(item.Kind) != phase {
				continue
			}
			if err := ctx.Err(); err != nil {
				return result, err
			}
			var (
				itemResult  ImportResult
				acknowledge bool
			)
			switch phase {
			case KindCheckpoints:
				itemResult, acknowledge, err = c.importQueuedCheckpoint(ctx, item)
			case KindMeta:
				itemResult.Metadata, acknowledge, err = c.importQueuedMetadata(ctx, clock, item)
			}
			result.Sessions += itemResult.Sessions
			result.Messages += itemResult.Messages
			result.Metadata += itemResult.Metadata
			if err != nil {
				return result, err
			}
			if acknowledge {
				if _, err := c.database.AcknowledgeArtifactImport(ctx, item); err != nil {
					return result, err
				}
			}
		}
	}
	deferred, _, err := c.database.ArtifactImportQueueStats(ctx)
	if err != nil {
		return result, err
	}
	result.Deferred = deferred
	return result, nil
}

func queuedImportEntry(item db.ArtifactImportWork) (Entry, error) {
	ref, err := NewRef(item.Origin, Kind(item.Kind), item.Name)
	if err != nil {
		return Entry{}, err
	}
	identity, err := NewIdentity(item.SHA256, item.Size)
	if err != nil {
		return Entry{}, err
	}
	entry := Entry{Ref: ref, Identity: identity}
	if err := validateRefIdentity(ref, identity); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func (c *StoreImportCoordinator) importQueuedCheckpoint(
	ctx context.Context, item db.ArtifactImportWork,
) (ImportResult, bool, error) {
	entry, err := queuedImportEntry(item)
	if err != nil {
		return ImportResult{}, false, err
	}
	sequence, err := checkpointSequence(entry.Ref.Name)
	if err != nil {
		return ImportResult{}, false, err
	}
	head, found, err := c.database.GetArtifactPeerCheckpointHead(ctx, entry.Ref.Origin)
	if err != nil {
		return ImportResult{}, false, err
	}
	if found && head.Sequence > sequence {
		return ImportResult{}, true, nil
	}
	landing, landed, err := c.database.GetArtifactCheckpointLandingHead(ctx, entry.Ref.Origin)
	if err != nil {
		return ImportResult{}, false, err
	}
	if landed && landing.Sequence == sequence && found && head.Sequence == sequence &&
		head.CheckpointSHA256 == entry.Identity.SHA256 && head.CheckpointSize == entry.Identity.Size {
		return ImportResult{}, true, nil
	}
	data, err := readVerifiedStoreArtifact(
		ctx, c.database, c.store, entry, checkpointDecodedLimit,
	)
	if errors.Is(err, errIncompleteArtifact) {
		return ImportResult{}, false, nil
	}
	if err != nil {
		if errors.Is(err, ErrArtifactInvalid) {
			qerr := c.store.Quarantine(ctx, entry.Ref, err.Error())
			return ImportResult{}, true, qerr
		}
		return ImportResult{}, false, fmt.Errorf("reading checkpoint %s: %w", entry.Ref.Name, err)
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		qerr := c.store.Quarantine(ctx, entry.Ref, "checkpoint JSON is invalid")
		return ImportResult{}, true, qerr
	}
	if cp.Version > formatVersion {
		if err := c.database.EnqueueArtifactImport(ctx,
			artifactImportWork(entry, artifactImportReasonCheckpoint, cp.Version)); err != nil {
			return ImportResult{}, false, err
		}
		return ImportResult{}, false, nil
	}
	canonical, canonicalErr := canonicalJSON(cp)
	if canonicalErr != nil || !bytes.Equal(canonical, data) {
		qerr := c.store.Quarantine(ctx, entry.Ref, "checkpoint JSON is not canonical")
		return ImportResult{}, true, qerr
	}
	if err := validateCheckpoint(&cp, entry.Ref.Origin); err != nil {
		if errors.Is(err, errFutureArtifactVersion) {
			return ImportResult{}, false, nil
		}
		qerr := c.store.Quarantine(ctx, entry.Ref, err.Error())
		return ImportResult{}, true, qerr
	}
	if err := validateCheckpointSequenceIdentity(cp, entry.Ref.Name); err != nil {
		qerr := c.store.Quarantine(ctx, entry.Ref, err.Error())
		return ImportResult{}, true, qerr
	}
	outcome, err := inspectCheckpointClosureFromStore(
		ctx, c.database, c.store, entry.Ref.Origin, cp,
	)
	if err != nil {
		return ImportResult{}, false, err
	}
	if outcome != checkpointClosureComplete {
		return ImportResult{}, false, nil
	}
	result, err := importCheckpointFromStore(ctx, c.database, c.store, entry.Ref.Origin, cp)
	if err != nil || result.Deferred > 0 {
		return result, false, err
	}
	return result, true, nil
}

func (c *StoreImportCoordinator) importQueuedMetadata(
	ctx context.Context, clock *HLCClock, item db.ArtifactImportWork,
) (int, bool, error) {
	entry, err := queuedImportEntry(item)
	if err != nil {
		return 0, false, err
	}
	orderKey, err := metadataArtifactOrderKey(entry.Ref.Name)
	if err != nil {
		qerr := c.store.Quarantine(ctx, entry.Ref, err.Error())
		return 0, true, errors.Join(err, qerr)
	}
	applied, err := c.database.MetadataEventApplied(ctx, entry.Ref.Origin, orderKey)
	if err != nil || applied {
		return 0, applied, err
	}
	data, err := readVerifiedStoreArtifact(ctx, c.database, c.store, entry, manifestDecodedLimit)
	if errors.Is(err, errIncompleteArtifact) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	idx := strings.LastIndex(orderKey, "-")
	if idx < 0 {
		qerr := c.store.Quarantine(ctx, entry.Ref, "metadata filename lacks hash")
		return 0, true, qerr
	}
	art := metadataArtifact{
		path: entry.Ref.Name, orderKey: orderKey,
		hash: orderKey[idx+1:], hlc: orderKey[:idx],
	}
	var envelope metadataEventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		qerr := c.store.Quarantine(ctx, entry.Ref, "metadata JSON is invalid")
		return 0, true, qerr
	}
	if envelope.Version > formatVersion {
		if err := c.database.EnqueueArtifactImport(ctx,
			artifactImportWork(entry, artifactImportReasonMetadata, envelope.Version)); err != nil {
			return 0, false, err
		}
		return 0, false, nil
	}
	if err := json.Unmarshal(data, &art.event); err != nil {
		qerr := c.store.Quarantine(ctx, entry.Ref, "metadata JSON is invalid")
		return 0, true, qerr
	}
	canonical, canonicalErr := canonicalJSON(art.event)
	if canonicalErr != nil || !bytes.Equal(canonical, data) {
		qerr := c.store.Quarantine(ctx, entry.Ref, "metadata JSON is not canonical")
		return 0, true, qerr
	}
	stamp, err := ParseHLCTimestamp(art.hlc)
	if err != nil {
		qerr := markAppliedAndQuarantineMetadata(
			ctx, c.database, c.store, entry.Ref, art, "metadata HLC is invalid",
		)
		return 0, true, qerr
	}
	if err := validateMetadataArtifactEvent(art, entry.Ref.Origin); err != nil {
		qerr := markAppliedAndQuarantineMetadata(
			ctx, c.database, c.store, entry.Ref, art, err.Error(),
		)
		return 0, true, qerr
	}
	if err := validateMetadataOp(art.event.Op); err != nil {
		qerr := markAppliedAndQuarantineMetadata(
			ctx, c.database, c.store, entry.Ref, art, "metadata operation is unsupported",
		)
		return 0, true, qerr
	}
	projection, err := metadataProjection(art, c.localOrigin)
	if err != nil {
		qerr := markAppliedAndQuarantineMetadata(
			ctx, c.database, c.store, entry.Ref, art, "metadata payload is invalid",
		)
		return 0, true, qerr
	}
	if err := observeMetadataStamp(clock, stamp, art.hlc); err != nil {
		if errors.Is(err, ErrHLCDrift) {
			return 0, false, nil
		}
		return 0, false, err
	}
	result, err := c.database.ApplyMetadataProjection(ctx, projection)
	if errors.Is(err, db.ErrMetadataTargetUnavailable) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("replaying metadata event %s: %w", entry.Ref.Name, err)
	}
	if result.Applied || result.Conflict {
		return 1, true, nil
	}
	return 0, true, nil
}
