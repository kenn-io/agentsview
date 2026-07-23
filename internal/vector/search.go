package vector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	kitvec "go.kenn.io/kit/vector"
)

// snippetMaxRunes bounds the length of a Hit's Snippet; longer chunks are
// truncated with a trailing ellipsis.
const snippetMaxRunes = 200

// Hit is one unit-level semantic search result, anchored to a specific
// message. For a run document Ordinal is the anchor: the member message
// whose rune span contains the matched chunk's center rune (see
// anchorMemberIndex), while OrdinalStart/OrdinalEnd span the whole run. For a
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
	// mirror, 0-100.
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
	hits, _, err := ix.SearchPage(ctx, enc, query, limit)
	return hits, err
}

// SearchPage returns semantic hits and whether the vector store exhausted the
// active generation before reaching limit. Callers that progressively expand
// selective searches need the pre-rollup exhaustion signal: several chunk
// hits can collapse into one document, so len(hits) alone cannot prove that
// no lower-ranked documents remain.
func (ix *Index) SearchPage(
	ctx context.Context, enc kitvec.EncodeFunc, query string, limit int,
) ([]Hit, bool, error) {
	if ix.versionMismatch {
		return nil, false, ErrMirrorVersionMismatch
	}

	active, hasActive, err := ix.ActiveFingerprint(ctx)
	if err != nil {
		return nil, false, err
	}
	if !hasActive {
		return nil, false, ix.noActiveGenerationError(ctx)
	}

	vectors, err := kitvec.EncodeBatched(ctx, enc,
		[]kitvec.Chunk{{Index: 0, Text: query}}, kitvec.BatchOptions{})
	if err != nil {
		return nil, false, &QueryEncodeError{Err: err}
	}

	hits, err := ix.store.QueryGeneration(ctx, active, vectors[0], limit)
	if err != nil {
		return nil, false, fmt.Errorf("search: %w", err)
	}
	exhausted := len(hits) < limit
	hits = kitvec.RollupByDocument(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}

	hydrated, err := ix.hydrateHits(ctx, hits)
	return hydrated, exhausted, err
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

// buildingPercent reports fingerprint's coverage of the current mirror as a
// 0-100 percentage, guarding the divide-by-zero case of an empty mirror.
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

// mirrorDoc is the subset of a mirror row Search needs to hydrate a kit hit
// into an agentsview Hit. offsets is empty for user documents.
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
// (empty offsets) anchors to its own mirror ordinal and snippets the whole
// matched chunk (already message-local); a run document anchors to the
// member whose rune span contains the matched chunk's center rune, and its
// snippet is sliced down to that anchor member's own text (see
// resolveRunHit) so downstream snippet centering can locate it inside the
// anchor message's content.
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
		hit.Ordinal, hit.Snippet = ix.resolveRunHit(doc.content, doc.offsets, h.ChunkIndex)
	}
	return hit
}

// DocAnchor resolves the anchor ordinal and display snippet for a matched
// chunk of a document, for backends that store the mirror elsewhere (the PG
// vector searcher). It mirrors resolveHit exactly: a user document (empty
// offsets) anchors to its own docOrdinal with a chunk snippet (see
// chunkSnippet); a run document anchors to the member whose rune span
// contains the matched chunk's center rune, with a member-local snippet
// (see resolveRunAnchor).
func DocAnchor(
	content string, offsets []db.UnitOffset, docOrdinal, chunkIndex, maxInputChars int,
) (int, string) {
	split := kitvec.SplitOptions{
		MaxRunes: maxInputChars,
		Overlap:  ChunkOverlap(maxInputChars),
	}
	if len(offsets) == 0 {
		return docOrdinal, chunkSnippet(content, chunkIndex, split)
	}
	return resolveRunAnchor(content, offsets, chunkIndex, split)
}

// runMemberSeparatorRunes is the rune length of the "\n\n" separator
// db's runUnit joins run members with (see internal/db's runUnit). The
// separator belongs to no member's span, so a member's text within the
// joined content ends this many runes before the next member's RuneStart.
const runMemberSeparatorRunes = 2

// resolveRunHit computes a run hit's anchor ordinal and anchor-local
// snippet: the anchor is the member whose rune span contains the matched
// chunk's center rune (see anchorMemberIndex), and the snippet is the
// intersection of the chunk's rune window with that member's span — always
// a substring of the anchor message's own text, so the db layer's snippet
// centering (SemanticSnippet) can locate it inside the anchor message's
// content. A degenerate/stale ChunkIndex whose re-split window misses the
// member entirely falls back to the anchor member's whole span text; the
// result is never text from a different member, and never a panic.
func (ix *Index) resolveRunHit(
	content string, offsets []db.UnitOffset, chunkIndex int,
) (ordinal int, snippet string) {
	return resolveRunAnchor(content, offsets, chunkIndex, ix.split)
}

// resolveRunAnchor is resolveRunHit's package-level body, shared with
// DocAnchor for backends (the PG vector searcher) that resolve chunk hits
// against a document loaded outside an Index.
func resolveRunAnchor(
	content string, offsets []db.UnitOffset, chunkIndex int, split kitvec.SplitOptions,
) (ordinal int, snippet string) {
	runes := []rune(content)
	start, end := chunkWindow(len(runes), chunkIndex, split)
	member := anchorMemberIndex(offsets, start, end)

	memberStart := min(offsets[member].RuneStart, len(runes))
	memberEnd := len(runes)
	if member+1 < len(offsets) {
		memberEnd = offsets[member+1].RuneStart - runMemberSeparatorRunes
	}
	memberEnd = min(max(memberEnd, memberStart), len(runes))

	lo, hi := max(start, memberStart), min(end, memberEnd)
	if lo >= hi {
		lo, hi = memberStart, memberEnd
	}
	return offsets[member].Ordinal, truncateRunes(string(runes[lo:hi]), snippetMaxRunes)
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

// anchorMemberIndex returns the offsets index of the run member whose rune
// span contains the [start, end) chunk window's center rune, with the
// earlier member winning when the center falls in the separator between two
// members. offsets must be non-empty.
func anchorMemberIndex(offsets []db.UnitOffset, start, end int) int {
	center := start + (end-start)/2
	anchor := 0
	for i, off := range offsets {
		if off.RuneStart <= center {
			anchor = i
		} else {
			break
		}
	}
	return anchor
}

// lookupMirrorDocs reads each of docKeys' mirror rows (session_id, ordinal
// range, subordinate flag, member offsets, content) from the mirror table,
// keyed by doc_key, in maxSQLVars-sized chunks: a deep semantic overfetch
// (large limit * over-fetch factor) can carry thousands of doc keys, well
// past what a single IN (...) clause can bind. A key with no matching row is
// simply absent from the result. Rows parked at a negative sentinel ordinal
// by a concurrent Refresh (see evictSlotOccupant) are excluded the same way:
// mid-refresh state must never hydrate into a hit with a negative ordinal.
func (ix *Index) lookupMirrorDocs(ctx context.Context, docKeys []string) (map[string]mirrorDoc, error) {
	docs := make(map[string]mirrorDoc, len(docKeys))
	err := chunkKeys(docKeys, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		rows, err := ix.db.QueryContext(ctx, `
SELECT doc_key, session_id, ordinal, ordinal_end, subordinate, offsets, content
  FROM `+ix.spec.DocsTable+`
 WHERE ordinal >= 0 AND doc_key IN `+placeholders, args...)
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
	return chunkSnippet(content, chunkIndex, ix.split)
}

// chunkSnippet is the snippet method's package-level body, shared with
// DocAnchor for backends (the PG vector searcher) that resolve chunk hits
// against a document loaded outside an Index.
func chunkSnippet(content string, chunkIndex int, split kitvec.SplitOptions) string {
	chunks := kitvec.Split(content, split)
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

// ResolveMessageUnits maps each ref to the mirror unit containing it,
// returning a slice parallel to refs; a ref with no containing unit (its
// message lies outside the embeddable universe, or in a gap between units)
// yields a zero UnitRef. Each ref is a point lookup on the retained unique
// (session_id, ordinal) index — greatest unit ordinal <= ref ordinal, then a
// containment check against ordinal_end — via one prepared statement, so a
// batch of any size never approaches SQLite's bind-variable limit. Rows
// parked at a negative sentinel ordinal by a concurrent Refresh (see
// evictSlotOccupant) are skipped so a ref can never resolve into
// mid-refresh state and surface a negative ordinal range.
//
// Like Search and StaleActive, it fails closed with ErrMirrorVersionMismatch
// — before touching any table — when ix was opened read-only against a
// vectors.db written by a different mirror schema version.
func (ix *Index) ResolveMessageUnits(
	ctx context.Context, refs []db.MessageRef,
) ([]db.UnitRef, error) {
	if ix.versionMismatch {
		return nil, ErrMirrorVersionMismatch
	}
	out := make([]db.UnitRef, len(refs))
	if len(refs) == 0 {
		return out, nil
	}

	stmt, err := ix.db.PrepareContext(ctx, `
SELECT doc_key, ordinal, ordinal_end, subordinate
  FROM `+ix.spec.DocsTable+`
 WHERE session_id = ? AND ordinal >= 0 AND ordinal <= ?
 ORDER BY ordinal DESC LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("resolve message units: %w", err)
	}
	defer stmt.Close()

	for i, ref := range refs {
		var unit db.UnitRef
		err := stmt.QueryRowContext(ctx, ref.SessionID, ref.Ordinal).Scan(
			&unit.DocKey, &unit.OrdinalStart, &unit.OrdinalEnd, &unit.Subordinate)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf(
				"resolve message unit (%s, %d): %w", ref.SessionID, ref.Ordinal, err)
		}
		if ref.Ordinal > unit.OrdinalEnd {
			continue
		}
		unit.SessionID = ref.SessionID
		out[i] = unit
	}
	return out, nil
}

// StaleActive reports whether the active generation's fingerprint differs
// from want or the last successfully completed corpus revision differs from
// wantRevision. It returns false when there is no active generation at all:
// Search already distinguishes that case with ErrNoActiveGeneration /
// BuildingError. An expected revision with no completed stamp is stale.
//
// Like Search, StaleActive fails closed with ErrMirrorVersionMismatch —
// before touching any table — when ix was opened read-only against a
// vectors.db written by a different mirror schema version: callers check
// staleness before searching, so without this gate a version-mismatched
// index would surface a raw SQL error (or a wrong staleness verdict) here
// and the sentinel in Search would never be reached.
func (ix *Index) StaleActive(
	ctx context.Context, want, wantRevision string,
) (bool, error) {
	if ix.versionMismatch {
		return false, ErrMirrorVersionMismatch
	}
	active, hasActive, err := ix.ActiveFingerprint(ctx)
	if err != nil {
		return false, err
	}
	if !hasActive {
		return false, nil
	}
	if active != want {
		return true, nil
	}
	if wantRevision == "" {
		return false, nil
	}
	completed, ok, err := ix.metaGet(ctx, completedCorpusRevisionMetaKey+active)
	if err != nil {
		return false, fmt.Errorf("reading completed corpus revision: %w", err)
	}
	return !ok || completed != wantRevision, nil
}
