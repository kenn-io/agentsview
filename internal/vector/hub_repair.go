package vector

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	kitvec "go.kenn.io/kit/vector"
)

// pullInvalidRepair uses only this projection's existing durable repair queue.
// The caller selects the target projection before entering this helper.
func (ix *Index) pullInvalidRepair(ctx context.Context, src UnitSource, reserved snapshotSource, hub HubLookup, gen kitvec.Generation, o PullOptions) (result BuildResult, retErr error) {
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
	captured := pullSource{UnitSource: src, identities: map[string]HubIdentity{}, mapSession: o.MapSession}
	if _, err := captured.ScanEmbeddableUnits(ctx, "", o.IncludeAutomated, RunMaxRunes(ix.split.MaxRunes), func(db.EmbeddableUnit) error { return nil }); err != nil {
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
		for i, p := range batch {
			identities[i] = captured.identities[p.Doc]
			if identities[i].ContentHash != p.Revision {
				return result, errors.New("local document changed before invalid vector repair")
			}
		}
		vectors, absent, err := ix.pullBatch(ctx, hub, target, gen.Dimensions, batch, identities)
		if err != nil {
			return result, err
		}
		err = reserved.WithEmbeddableSnapshot(ctx, func(snapshot db.EmbeddableSnapshot) error {
			current := pullSource{identities: map[string]HubIdentity{}}
			sessions := map[string]bool{}
			occurrences := map[string]int{}
			for _, p := range batch {
				var sid string
				if err := ix.db.QueryRowContext(ctx, "SELECT session_id FROM "+ix.spec.DocsTable+" WHERE doc_key=?", p.Doc).Scan(&sid); err != nil {
					return err
				}
				if sessions[sid] {
					continue
				}
				sessions[sid] = true
				if err := snapshot.ScanEmbeddableUnitsForSession(ctx, sid, o.IncludeAutomated, RunMaxRunes(ix.split.MaxRunes), func(u db.EmbeddableUnit) error { current.record(u, occurrences); return nil }); err != nil {
					return err
				}
			}
			for i, p := range batch {
				if absent[i] || current.identities[p.Doc].ContentHash != identities[i].ContentHash {
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
