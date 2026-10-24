package vector

import (
	"context"
	"errors"
	"fmt"

	kitvec "go.kenn.io/kit/vector"
)

// ErrHubRepairIncomplete means a completed hub repair pass left queued targets
// without hub coverage. Other failures, including stale source data, do not match
// this error and must not trigger local producer fallback.
var ErrHubRepairIncomplete = errors.New("hub invalid vector repair incomplete")

// pullInvalidRepair uses only this projection's existing durable repair queue.
// The caller selects the target projection before entering this helper.
func (ix *Index) pullInvalidRepair(ctx context.Context, reserved snapshotSource, hub HubLookup, gen kitvec.Generation, o PullOptions) (result BuildResult, retErr error) {
	target := gen.Fingerprint()
	result.Fingerprint = target
	repair, err := ix.repairInvalidVectors(ctx, target)
	result.Repair = repair.Stats
	if !repair.Stats.Scanned {
		return result, err
	}
	store := &repairStore{base: ix.store, db: ix.db, spec: ix.spec, fingerprint: target, ordinal: repair.Ordinal, queueTable: ix.spec.repairQueueTable(), split: ix.split}
	defer func() {
		remaining, known, countErr := repairRemaining(ctx, store, max(result.Repair.Documents-result.Fill.Documents, 0))
		result.Repair.Remaining, result.Repair.RemainingKnown = remaining, known
		retErr = errors.Join(retErr, countErr)
		if result.Fill.Stale > 0 {
			retErr = errors.Join(retErr, kitvec.ErrStale)
		}
		if retErr == nil && remaining > 0 {
			retErr = fmt.Errorf("%w: %d targets remain queued", ErrHubRepairIncomplete, remaining)
		}
	}()
	if err != nil {
		return result, err
	}
	var after string
	for {
		batch, err := store.pendingAfter(ctx, target, after, 256)
		if err != nil {
			return result, err
		}
		if len(batch) == 0 {
			return result, nil
		}
		after = batch[len(batch)-1].Doc
		identities := make([]HubIdentity, len(batch))
		err = ix.withPullIdentities(ctx, reserved, batch, o, func(current map[string]HubIdentity) error {
			for i, p := range batch {
				identities[i] = current[p.Doc]
				if identities[i].ContentHash != p.Revision {
					return fmt.Errorf("local document changed before invalid vector repair: %w", kitvec.ErrStale)
				}
				if o.MapSession != nil {
					identities[i].SessionID = o.MapSession(identities[i].SessionID)
				}
			}
			return nil
		})
		if err != nil {
			return result, err
		}
		vectors, absent, err := ix.pullBatch(ctx, hub, target, gen.Dimensions, batch, identities)
		if err != nil {
			return result, err
		}
		err = ix.withPullIdentities(ctx, reserved, batch, o, func(current map[string]HubIdentity) error {
			for i, p := range batch {
				if current[p.Doc].ContentHash != identities[i].ContentHash {
					result.Fill.Stale++
					continue
				}
				if absent[i] {
					continue
				}
				if err := store.SaveVectors(ctx, target, p.Doc, p.Revision, vectors[i]); err != nil {
					if errors.Is(err, kitvec.ErrStale) {
						result.Fill.Stale++
						continue
					}
					result.Repair.Failed++
					return err
				}
				result.Fill.Documents++
				result.Fill.Chunks += len(vectors[i])
			}
			return nil
		})
		if err != nil {
			return result, err
		}
		if o.Progress != nil {
			o.Progress(BuildProgress{Phase: "pulling", Done: int64(result.Fill.Documents), Total: int64(result.Repair.Documents)})
		}
	}
}
