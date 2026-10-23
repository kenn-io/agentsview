package vector

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/embedmodel"
	kitvec "go.kenn.io/kit/vector"
)

type HubLookup interface {
	LookupHubVectors(context.Context, HubLookupRequest) (HubLookupReply, error)
}
type PullOptions struct {
	IncludeAutomated           bool
	FullRebuild, RepairInvalid bool
	MapSession                 func(string) string
	Progress                   func(BuildProgress)
}

type pullSource struct {
	UnitSource
	identities map[string]HubIdentity
	mapSession func(string) string
}

func (s pullSource) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, cap int, fn func(db.EmbeddableUnit) error) (string, error) {
	occurrences := map[string]int{}
	return s.UnitSource.ScanEmbeddableUnits(ctx, since, scope, cap, func(u db.EmbeddableUnit) error {
		s.record(u, occurrences)
		return fn(u)
	})
}

func (s pullSource) record(u db.EmbeddableUnit, occurrences map[string]int) {
	n := 1
	if u.SourceUUID != "" {
		k := u.SessionID + "\x00" + u.SourceUUID
		occurrences[k]++
		n = occurrences[k]
	}
	id := u.SessionID
	if s.mapSession != nil {
		id = s.mapSession(id)
	}
	s.identities[DocKey(u.Kind, u.SessionID, u.SourceUUID, u.Ordinal, n)] = HubIdentity{SessionID: id, UnitKind: u.Kind, SourceUUID: u.SourceUUID, Occurrence: n, Ordinal: u.Ordinal, ContentHash: contentHash(u.Content)}
}

type snapshotSource interface {
	WithEmbeddableSnapshot(context.Context, func(db.EmbeddableSnapshot) error) error
}

// Pull runs under the caller's vector writer lock and manager exclusion.
// Each document is published only after its entire pinned chunk set arrives.
func (ix *Index) Pull(ctx context.Context, src UnitSource, hub HubLookup, gen kitvec.Generation, o PullOptions) (BuildResult, error) {
	if err := ix.requireWritable(); err != nil {
		return BuildResult{}, err
	}
	if ix.spec.Name != MessageIndexSpec().Name {
		return BuildResult{}, errors.New("hub vectors require the messages store")
	}
	reserved, ok := src.(snapshotSource)
	if !ok {
		return BuildResult{}, errors.New("hub import requires an archive writer reservation")
	}
	if maxChars, err := strconv.Atoi(gen.Params["max_input_chars"]); err == nil && maxChars > 0 {
		ix.split = kitvec.SplitOptions{MaxRunes: maxChars, Overlap: ChunkOverlap(maxChars)}
	}
	captured := pullSource{UnitSource: src, identities: map[string]HubIdentity{}, mapSession: o.MapSession}
	stats, err := ix.Refresh(ctx, captured, true, o.IncludeAutomated)
	if err != nil {
		return BuildResult{}, err
	}
	if err = ix.setIncludeAutomatedScope(ctx, o.IncludeAutomated); err != nil {
		return BuildResult{}, err
	}
	target, building, err := ix.resolveBuildTarget(ctx, gen, gen.Fingerprint(), embedmodel.Descriptor{}, o.FullRebuild)
	if err != nil {
		return BuildResult{}, err
	}
	result := BuildResult{Fingerprint: target, Refresh: stats}
	if o.RepairInvalid {
		repair, err := ix.repairInvalidVectors(ctx, target)
		result.Repair = repair.Stats
		if err != nil {
			return result, err
		}
	}
	store := &repairQueueCompletingStore{Store: ix.store, db: ix.db, spec: ix.spec}
	raw, err := json.Marshal(gen)
	if err != nil {
		return result, err
	}
	if err = ix.metaSet(ctx, "hub_recipe:"+target, string(raw)); err != nil {
		return result, err
	}
	pending, err := ix.store.PendingForGeneration(ctx, target, -1)
	if err != nil {
		return result, err
	}
	for start := 0; start < len(pending); start += 256 {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		batch := pending[start:min(start+256, len(pending))]
		identities := make([]HubIdentity, len(batch))
		for i, p := range batch {
			var ok bool
			identities[i], ok = captured.identities[p.Doc]
			if !ok {
				return result, errors.New("local document changed during scan")
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
			// Use local identities for validation; hub namespace mapping never touches keys.
			for _, p := range batch {
				var sid string
				err := ix.db.QueryRowContext(ctx, "SELECT session_id FROM "+ix.spec.DocsTable+" WHERE doc_key=?", p.Doc).Scan(&sid)
				if err != nil {
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
				if err := store.SaveVectors(ctx, target, p.Doc, p.Revision, vectors[i]); err != nil && !errors.Is(err, kitvec.ErrStale) {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return result, err
		}

		if o.Progress != nil {
			o.Progress(BuildProgress{Phase: "pulling", Done: int64(start + len(batch)), Total: int64(len(pending))})
		}
	}
	err = reserved.WithEmbeddableSnapshot(ctx, func(snapshot db.EmbeddableSnapshot) error {
		if _, err := ix.Refresh(ctx, snapshot, true, o.IncludeAutomated); err != nil {
			return err
		}
		pending, err := ix.store.PendingForGeneration(ctx, target, 1)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			if err := ix.clearActiveFullRebuildPending(ctx, target); err != nil {
				return err
			}
		}
		result.Activated, err = ix.maybeActivate(ctx, target, building)
		return err
	})
	return result, err
}

func (ix *Index) pullBatch(ctx context.Context, hub HubLookup, fp string, dimension int, docs []kitvec.Pending[string], identities []HubIdentity) ([][]kitvec.ChunkVector, []bool, error) {
	vectors := make([][]kitvec.ChunkVector, len(docs))
	seen := make([][]bool, len(docs))
	absent := make([]bool, len(docs))
	for i, p := range docs {
		n := len(kitvec.Split(p.Content, ix.split))
		vectors[i] = make([]kitvec.ChunkVector, n)
		seen[i] = make([]bool, n)
	}
	req := HubLookupRequest{Fingerprint: fp, Identities: identities}
	cursors := map[string]bool{"": true}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		reply, err := hub.LookupHubVectors(ctx, req)
		if err != nil {
			return nil, nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if reply.Version != HubVectorVersion || reply.Fingerprint != fp {
			return nil, nil, ErrHubGenerationChanged
		}
		for _, i := range reply.Absent {
			if i < 0 || i >= len(docs) || absent[i] || hasSeenChunk(seen[i]) {
				return nil, nil, errors.New("invalid absent identity")
			}
			absent[i] = true
		}
		for _, c := range reply.Chunks {
			i, k := c.Index, c.ChunkIndex
			if i < 0 || i >= len(docs) || k < 0 || k >= len(vectors[i]) || c.ChunkCount != len(vectors[i]) || seen[i][k] || absent[i] {
				return nil, nil, errors.New("invalid hub chunk set")
			}
			v, err := DecodeHubVector(c.Vector, dimension)
			if err != nil {
				return nil, nil, err
			}
			vectors[i][k] = kitvec.ChunkVector{ChunkIndex: k, Vector: v}
			seen[i][k] = true
		}
		if reply.NextCursor == "" {
			break
		}
		if len(reply.NextCursor) > 128 || len(reply.Absent)+len(reply.Chunks) == 0 {
			return nil, nil, errors.New("hub continuation made no progress")
		}
		if cursors[reply.NextCursor] {
			return nil, nil, errors.New("repeated hub continuation cursor")
		}
		cursors[reply.NextCursor] = true
		req.Cursor = reply.NextCursor
	}
	for i := range docs {
		for _, ok := range seen[i] {
			if !ok && !absent[i] {
				return nil, nil, errors.New("incomplete hub chunk set")
			}
		}
	}
	return vectors, absent, nil
}

// ActiveHubRecipe permits local query encoding without reading remote credentials.
func (ix *Index) ActiveHubRecipe(ctx context.Context) (kitvec.Generation, bool, error) {
	fp, ok, err := ix.ActiveFingerprint(ctx)
	if err != nil || !ok {
		return kitvec.Generation{}, false, err
	}
	raw, ok, err := ix.metaGet(ctx, "hub_recipe:"+fp)
	if err != nil || !ok {
		return kitvec.Generation{}, false, err
	}
	var gen kitvec.Generation
	if err = json.Unmarshal([]byte(raw), &gen); err != nil {
		return gen, false, err
	}
	if gen.Fingerprint() != fp {
		return gen, false, fmt.Errorf("stored hub recipe fingerprint mismatch")
	}
	return gen, true, nil
}

func hasSeenChunk(seen []bool) bool {
	for _, v := range seen {
		if v {
			return true
		}
	}
	return false
}
