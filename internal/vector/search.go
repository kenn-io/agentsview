package vector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	kitvec "go.kenn.io/kit/vector"
)

// snippetMaxRunes bounds the length of a Hit's Snippet; longer chunks are
// truncated with a trailing ellipsis.
const snippetMaxRunes = 200

// Hit is one unit-level semantic search result, anchored to a specific
// message. For a run document Ordinal is the anchor: the member message
// whose rune span contains the matched chunk's center rune (see
// anchorOrdinal), while OrdinalStart/OrdinalEnd span the whole run. For a
// user document all three ordinals are the message's own ordinal.
type Hit struct {
	SessionID    string
	Ordinal      int // anchor ordinal
	OrdinalStart int
	OrdinalEnd   int
	Subordinate  bool
	Score        float32
	Snippet      string
}

// ErrNoActiveGeneration is returned by Search when the index has no active
// embedding generation to query — either nothing has ever been built, or a
// build is in progress but has not yet activated (see BuildingError).
var ErrNoActiveGeneration = errors.New("no active embedding generation")

// BuildingError is returned by Search when only a building generation
// exists: a first build is in progress and has not yet activated, so no
// generation is queryable yet.
type BuildingError struct {
	// Percent is the building generation's coverage of the current
	// vector_messages mirror, 0-100.
	Percent int
}

func (e *BuildingError) Error() string {
	return fmt.Sprintf("embedding index is building (%d%% complete)", e.Percent)
}

// QueryEncodeError reports that Search failed because embedding the query
// text itself failed — the embeddings endpoint being down, slow, timing
// out, or erroring at search time — as distinct from ErrNoActiveGeneration
// and BuildingError, which both mean semantic search has nothing queryable
// yet. A QueryEncodeError means the index is otherwise ready; only this
// particular request failed, and it is generally worth retrying rather
// than reporting semantic search as unconfigured.
type QueryEncodeError struct {
	Err error
}

func (e *QueryEncodeError) Error() string {
	return fmt.Sprintf("embed query: %v", e.Err)
}

func (e *QueryEncodeError) Unwrap() error { return e.Err }

// Search embeds query and returns up to limit message-level hits, best
// first. It returns ErrNoActiveGeneration when no live generation exists,
// and a *BuildingError (carrying the completion Percent) when only a
// building generation exists.
//
// Search deliberately queries only the active generation rather than
// kitvec.Search's default of every live (building + active) generation: a
// model or dimension change leaves a building generation coexisting with
// the active one, and kitvec.Search would encode the query once with enc
// and reuse that single vector for both, which errors (or, for a same-
// dimension model change, silently ranks against the wrong model's space)
// once their dimensions or embedding spaces differ. Encoding only for the
// caller-chosen generation and querying it directly with
// store.QueryGeneration sidesteps that; a system-level staleness gate
// elsewhere already rejects an encoder that no longer matches the active
// generation's fingerprint, so this mismatch cannot arise in practice once
// that gate is wired up.
//
// Search also returns ErrMirrorVersionMismatch, before touching any table,
// when ix was opened read-only against a vectors.db whose mirror schema
// version does not match this binary's (see prepareMirrorSchema): a
// read-only Index cannot reset the mirror itself, so it fails closed rather
// than risk misreading rows shaped by a different schema.
func (ix *Index) Search(
	ctx context.Context, enc kitvec.EncodeFunc, query string, limit int,
) ([]Hit, error) {
	if ix.versionMismatch {
		return nil, ErrMirrorVersionMismatch
	}

	active, hasActive, err := ix.ActiveFingerprint(ctx)
	if err != nil {
		return nil, err
	}
	if !hasActive {
		return nil, ix.noActiveGenerationError(ctx)
	}

	vectors, err := kitvec.EncodeBatched(ctx, enc,
		[]kitvec.Chunk{{Index: 0, Text: query}}, kitvec.BatchOptions{})
	if err != nil {
		return nil, &QueryEncodeError{Err: err}
	}

	hits, err := ix.store.QueryGeneration(ctx, active, vectors[0], limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	hits = kitvec.RollupByDocument(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}

	return ix.hydrateHits(ctx, hits)
}

// noActiveGenerationError distinguishes an empty index (ErrNoActiveGeneration)
// from one with an in-progress first build (*BuildingError), for a caller
// that already knows there is no active generation.
func (ix *Index) noActiveGenerationError(ctx context.Context) error {
	buildingFP, hasBuilding, err := ix.BuildingFingerprint(ctx)
	if err != nil {
		return err
	}
	if !hasBuilding {
		return ErrNoActiveGeneration
	}
	percent, err := ix.buildingPercent(ctx, buildingFP)
	if err != nil {
		return err
	}
	return &BuildingError{Percent: percent}
}

// buildingPercent reports fingerprint's coverage of the current
// vector_messages mirror as a 0-100 percentage, guarding the
// divide-by-zero case of an empty mirror.
func (ix *Index) buildingPercent(ctx context.Context, fingerprint string) (int, error) {
	ordinal, err := ix.ordinalForFingerprint(ctx, fingerprint)
	if err != nil {
		return 0, err
	}
	info, err := ix.GenerationByID(ctx, ordinal)
	if err != nil {
		return 0, err
	}
	total := info.Embedded + info.Missing
	if total == 0 {
		return 0, nil
	}
	return int(info.Embedded * 100 / total), nil
}

// mirrorDoc is the subset of a vector_messages row Search needs to hydrate a
// kit hit into an agentsview Hit. offsets is empty for user documents.
type mirrorDoc struct {
	sessionID   string
	ordinal     int
	ordinalEnd  int
	subordinate bool
	offsets     []db.UnitOffset
	content     string
}

// hydrateHits maps kit's doc-key-level hits to agentsview Hits: it looks up
// each doc_key's mirror row in one query, anchors run hits to a member
// ordinal, and computes a snippet by re-splitting content. Hits whose
// doc_key vanished from the mirror mid-flight (a concurrent Refresh
// reconciled it away) are dropped rather than erroring, since the search
// itself still succeeded.
func (ix *Index) hydrateHits(ctx context.Context, hits []kitvec.Hit[string]) ([]Hit, error) {
	if len(hits) == 0 {
		return nil, nil
	}

	docKeys := make([]string, len(hits))
	for i, h := range hits {
		docKeys[i] = h.Doc
	}
	docs, err := ix.lookupMirrorDocs(ctx, docKeys)
	if err != nil {
		return nil, err
	}

	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		doc, ok := docs[h.Doc]
		if !ok {
			continue
		}
		out = append(out, ix.resolveHit(h, doc))
	}
	return out, nil
}

// resolveHit builds the Hit for one matched document. A user document
// (empty offsets) anchors to its own mirror ordinal; a run document anchors
// to the member whose rune span contains the matched chunk's center rune.
func (ix *Index) resolveHit(h kitvec.Hit[string], doc mirrorDoc) Hit {
	hit := Hit{
		SessionID:    doc.sessionID,
		Ordinal:      doc.ordinal,
		OrdinalStart: doc.ordinal,
		OrdinalEnd:   doc.ordinalEnd,
		Subordinate:  doc.subordinate,
		Score:        h.Score,
		Snippet:      ix.snippet(doc.content, h.ChunkIndex),
	}
	if len(doc.offsets) > 0 {
		hit.Ordinal = anchorOrdinal(doc.offsets,
			utf8.RuneCountInString(doc.content), h.ChunkIndex, ix.split)
	}
	return hit
}

// chunkWindow returns the [start, end) rune window of content's
// chunkIndex'th chunk, mirroring kitvec.Split's semantics exactly: content
// that fits MaxRunes (or unbounded splitting) is one whole-content chunk;
// otherwise chunks start at multiples of the stride (MaxRunes minus the
// clamped overlap) and the final chunk is capped at the content's end, so
// end-start is the chunk's ACTUAL rune length, not always MaxRunes.
// TestChunkWindowMatchesKitSplit cross-checks this against kitvec.Split.
func chunkWindow(contentRunes, chunkIndex int, o kitvec.SplitOptions) (start, end int) {
	if o.MaxRunes <= 0 || contentRunes <= o.MaxRunes {
		return 0, contentRunes
	}
	overlap := min(max(o.Overlap, 0), o.MaxRunes-1)
	stride := o.MaxRunes - overlap
	start = chunkIndex * stride
	end = min(start+o.MaxRunes, contentRunes)
	return start, end
}

// anchorOrdinal maps a matched chunk back to the run member whose rune span
// contains the chunk's center rune (chunk_start + actual_chunk_runes/2).
// Separator runes between members belong to no member's span, so a center
// falling there resolves to the preceding member: the earlier member wins a
// boundary tie. offsets must be non-empty (run documents only); user
// documents pass their ordinal through without calling this.
func anchorOrdinal(offsets []db.UnitOffset, contentRunes, chunkIndex int, o kitvec.SplitOptions) int {
	start, end := chunkWindow(contentRunes, chunkIndex, o)
	center := start + (end-start)/2
	anchor := offsets[0].Ordinal
	for _, off := range offsets {
		if off.RuneStart <= center {
			anchor = off.Ordinal
		} else {
			break
		}
	}
	return anchor
}

// lookupMirrorDocs reads each of docKeys' mirror rows (session_id, ordinal
// range, subordinate flag, member offsets, content) from vector_messages,
// keyed by doc_key, in maxSQLVars-sized chunks: a deep semantic overfetch
// (large limit * over-fetch factor) can carry thousands of doc keys, well
// past what a single IN (...) clause can bind. A key with no matching row is
// simply absent from the result.
func (ix *Index) lookupMirrorDocs(ctx context.Context, docKeys []string) (map[string]mirrorDoc, error) {
	docs := make(map[string]mirrorDoc, len(docKeys))
	err := chunkKeys(docKeys, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		rows, err := ix.db.QueryContext(ctx, `
SELECT doc_key, session_id, ordinal, ordinal_end, subordinate, offsets, content
  FROM vector_messages
 WHERE doc_key IN `+placeholders, args...)
		if err != nil {
			return fmt.Errorf("look up search hit documents: %w", err)
		}
		for rows.Next() {
			var key, offsets string
			var doc mirrorDoc
			if err := rows.Scan(&key, &doc.sessionID, &doc.ordinal,
				&doc.ordinalEnd, &doc.subordinate, &offsets, &doc.content); err != nil {
				rows.Close()
				return fmt.Errorf("scan search hit document: %w", err)
			}
			if err := json.Unmarshal([]byte(offsets), &doc.offsets); err != nil {
				rows.Close()
				return fmt.Errorf("parse offsets for search hit %s: %w", key, err)
			}
			docs[key] = doc
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("look up search hit documents: %w", err)
		}
		return rows.Close()
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// snippet re-splits content the same way Build did and returns the text of
// its chunkIndex'th chunk, truncated to snippetMaxRunes runes with a
// trailing ellipsis when truncated. A chunkIndex outside the re-split
// content's chunk count (content changed since embedding) yields an empty
// snippet rather than a panic.
func (ix *Index) snippet(content string, chunkIndex int) string {
	chunks := kitvec.Split(content, ix.split)
	if chunkIndex < 0 || chunkIndex >= len(chunks) {
		return ""
	}
	return truncateRunes(chunks[chunkIndex].Text, snippetMaxRunes)
}

// truncateRunes truncates s to at most maxRunes runes, appending an
// ellipsis when truncation occurs. It measures in runes so multi-byte
// characters are never torn apart.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// StaleActive reports whether the active generation's fingerprint differs
// from want (the fingerprint of the current config) — the "index stale"
// signal surfaced by the db layer. It returns false when there is no active
// generation at all: Search already distinguishes that case with
// ErrNoActiveGeneration / BuildingError.
func (ix *Index) StaleActive(ctx context.Context, want string) (bool, error) {
	active, hasActive, err := ix.ActiveFingerprint(ctx)
	if err != nil {
		return false, err
	}
	if !hasActive {
		return false, nil
	}
	return active != want, nil
}
