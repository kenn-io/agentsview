package vector

import (
	"context"
	"errors"
	"fmt"

	kitvec "go.kenn.io/kit/vector"
)

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
		if retErr == nil && remaining > 0 {
			retErr = fmt.Errorf("invalid vector repair incomplete: %d targets remain queued", remaining)
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
					return errors.New("local document changed before invalid vector repair")
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
				if absent[i] || current[p.Doc].ContentHash != identities[i].ContentHash {
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
