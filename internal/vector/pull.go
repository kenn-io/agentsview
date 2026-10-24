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
	IncludeAutomated bool
	MapSession       func(string) string
	Progress         func(BuildProgress)
}

type pullSource struct {
	UnitSource
	identities map[string]HubIdentity
	mapSession func(string) string
}

func (s pullSource) ScanEmbeddableUnits(ctx context.Context, since string, scope bool, limit int, fn func(db.EmbeddableUnit) error) (string, error) {
	occurrences := map[string]int{}
	return s.UnitSource.ScanEmbeddableUnits(ctx, since, scope, limit, func(u db.EmbeddableUnit) error {
		s.record(u, occurrences)
		return fn(u)
	})
}

func (s pullSource) record(u db.EmbeddableUnit, occurrences map[string]int) {
	n := unitOccurrence(u, occurrences)
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
	active, hasActive, err := ix.ActiveFingerprint(ctx)
	if err != nil {
		return BuildResult{}, err
	}
	if hasActive && active != gen.Fingerprint() {
		stage, err := ix.newPullStage(ctx, gen)
		if err != nil {
			return BuildResult{}, err
		}
		published := false
		result, err := stage.pull(ctx, src, reserved, hub, gen, o, func() error {
			if err := ix.publishPullStage(ctx, stage, gen.Fingerprint()); err != nil {
				return err
			}
			published = true
			return nil
		})
		result.Activated = published
		return result, err
	}
	return ix.pull(ctx, src, reserved, hub, gen, o, nil)
}

func (ix *Index) pull(ctx context.Context, src UnitSource, reserved snapshotSource, hub HubLookup, gen kitvec.Generation, o PullOptions, publish func() error) (BuildResult, error) {
	ix = ix.forGeneration(gen)
	// A failed reshape must force a full scan when the previous build cap returns.
	if err := ix.metaDelete(ctx, runMaxRunesMetaKey); err != nil {
		return BuildResult{}, err
	}
	storedScope, hasScope, err := ix.storedIncludeAutomatedScope(ctx)
	if err != nil {
		return BuildResult{}, err
	}
	if !hasScope || storedScope != o.IncludeAutomated {
		if err := ix.metaDelete(ctx, scopeIncludeAutomatedKey); err != nil {
			return BuildResult{}, fmt.Errorf("clearing changed embedding scope: %w", err)
		}
	}
	captured := pullSource{UnitSource: src, identities: map[string]HubIdentity{}, mapSession: o.MapSession}
	stats, err := ix.Refresh(ctx, captured, true, o.IncludeAutomated)
	if err != nil {
		return BuildResult{}, err
	}
	if err = ix.setIncludeAutomatedScope(ctx, o.IncludeAutomated); err != nil {
		return BuildResult{}, err
	}
	target, building, err := ix.resolveBuildTarget(ctx, gen, gen.Fingerprint(), embedmodel.Descriptor{}, false)
	if err != nil {
		return BuildResult{}, err
	}
	if err := ix.recordGenerationSplit(ctx, target); err != nil {
		return BuildResult{}, err
	}
	result := BuildResult{Fingerprint: target, Refresh: stats}
	raw, err := json.Marshal(gen)
	if err != nil {
		return result, err
	}
	if err = ix.metaSet(ctx, "hub_recipe:"+target, string(raw)); err != nil {
		return result, err
	}
	ordinal, err := ix.ordinalForFingerprint(ctx, target)
	if err != nil {
		return result, err
	}
	total, err := ix.MissingEmbeddedDocs(ctx, target, nil)
	if err != nil {
		return result, err
	}
	saving := &repairQueueCompletingStore{Store: ix.store, db: ix.db, spec: ix.spec}
	var after string
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		batch, err := ix.pullPendingPage(ctx, ordinal, after)
		if err != nil {
			return result, err
		}
		if len(batch) == 0 {
			break
		}
		// Advance even when the hub has no vectors for every document in this page.
		after = batch[len(batch)-1].Doc
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
				if (absent[i] && len(vectors[i]) != 0) || current.identities[p.Doc].ContentHash != identities[i].ContentHash {
					continue
				}
				if err := saving.SaveVectors(ctx, target, p.Doc, p.Revision, vectors[i]); err != nil && !errors.Is(err, kitvec.ErrStale) {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return result, err
		}

		done += int64(len(batch))
		if o.Progress != nil {
			o.Progress(BuildProgress{Phase: "pulling", Done: done, Total: total})
		}
	}
	err = reserved.WithEmbeddableSnapshot(ctx, func(snapshot db.EmbeddableSnapshot) error {
		if _, err := ix.Refresh(ctx, snapshot, true, o.IncludeAutomated); err != nil {
			return err
		}
		if err := ix.metaSet(ctx, runMaxRunesMetaKey, strconv.Itoa(RunMaxRunes(ix.split.MaxRunes))); err != nil {
			return err
		}
		var err error
		result.Activated, err = ix.maybeActivate(ctx, target, building)
		if err != nil {
			return err
		}
		missing, err := ix.MissingEmbeddedDocs(ctx, target, nil)
		if err != nil || missing != 0 {
			return err
		}
		if err := ix.clearActiveFullRebuildPending(ctx, target); err != nil {
			return err
		}
		if publish != nil {
			result.Activated = false
			if err := publish(); err != nil {
				return err
			}
			result.Activated = true
		}
		return nil
	})
	return result, err
}

// Resolve the bounded key page before loading content. This preserves kit's
// coverage predicate, including documents invalidated after their last stamp.
// Pinned kit PendingForGeneration has no after cursor: retrying its first page
// would starve later documents when the hub lacks coverage for earlier ones.
func (ix *Index) pullPendingPage(ctx context.Context, ordinal int64, after string) ([]kitvec.Pending[string], error) {
	rows, err := ix.db.QueryContext(ctx, `
WITH pending AS MATERIALIZED (
 SELECT d.doc_key FROM `+ix.spec.DocsTable+` d
 LEFT JOIN `+ix.spec.stampsTable()+` s ON s.ordinal = ? AND s.doc_key = d.doc_key
 WHERE d.doc_key > ?
   AND NOT (d.embed_gen IS NOT NULL AND s.doc_key IS NOT NULL AND d.content_hash IS s.revision)
 ORDER BY d.doc_key LIMIT 256
)
SELECT d.doc_key, d.content, d.content_hash FROM pending p
 JOIN `+ix.spec.DocsTable+` d ON d.doc_key = p.doc_key ORDER BY d.doc_key`, ordinal, after)
	if err != nil {
		return nil, fmt.Errorf("read pending hub documents: %w", err)
	}
	defer rows.Close()
	var batch []kitvec.Pending[string]
	for rows.Next() {
		var p kitvec.Pending[string]
		if err := rows.Scan(&p.Doc, &p.Content, &p.Revision); err != nil {
			return nil, err
		}
		batch = append(batch, p)
	}
	return batch, rows.Err()
}

func (ix *Index) pullBatch(ctx context.Context, hub HubLookup, fp string, dimension int, docs []kitvec.Pending[string], identities []HubIdentity) ([][]kitvec.ChunkVector, []bool, error) {
	vectors := make([][]kitvec.ChunkVector, len(docs))
	seen := make([][]bool, len(docs))
	positions := make([]map[int]int, len(docs))
	absent := make([]bool, len(docs))
	for i, p := range docs {
		chunks := kitvec.Split(p.Content, ix.split)
		n := len(chunks)
		vectors[i] = make([]kitvec.ChunkVector, n)
		seen[i] = make([]bool, n)
		positions[i] = make(map[int]int, n)
		for j, chunk := range chunks {
			positions[i][chunk.Index] = j
		}
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
			if i < 0 || i >= len(docs) {
				return nil, nil, errors.New("invalid hub document index")
			}
			position, ok := positions[i][k]
			if !ok || c.ChunkCount != len(vectors[i]) || seen[i][position] || absent[i] {
				return nil, nil, errors.New("invalid hub chunk set")
			}
			v, err := DecodeHubVector(c.Vector, dimension)
			if err != nil {
				return nil, nil, err
			}
			vectors[i][position] = kitvec.ChunkVector{ChunkIndex: k, Vector: v}
			seen[i][position] = true
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
		return gen, false, errors.New("stored hub recipe fingerprint mismatch")
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
