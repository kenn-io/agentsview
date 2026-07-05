package vector

import (
	"context"
	"errors"
	"fmt"
	"strings"

	kitvec "go.kenn.io/kit/vector"
)

// snippetMaxRunes bounds the length of a Hit's Snippet; longer chunks are
// truncated with a trailing ellipsis.
const snippetMaxRunes = 200

// Hit is one message-level semantic search result.
type Hit struct {
	SessionID string
	Ordinal   int
	Score     float32
	Snippet   string
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

// Search embeds query and returns up to limit message-level hits, best
// first. It returns ErrNoActiveGeneration when no live generation exists,
// and a *BuildingError (carrying the completion Percent) when only a
// building generation exists.
func (ix *Index) Search(
	ctx context.Context, enc kitvec.EncodeFunc, query string, limit int,
) ([]Hit, error) {
	_, hasActive, err := ix.ActiveFingerprint(ctx)
	if err != nil {
		return nil, err
	}
	if !hasActive {
		return nil, ix.noActiveGenerationError(ctx)
	}

	hits, err := kitvec.Search[string, string](ctx, ix.store, query,
		func(string) kitvec.EncodeFunc { return enc },
		kitvec.SearchOptions{
			PerGeneration: limit,
			Merge:         kitvec.MergeOptions{Limit: limit},
		})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
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
// kit hit into an agentsview Hit.
type mirrorDoc struct {
	sessionID string
	ordinal   int
	content   string
}

// hydrateHits maps kit's doc-key-level hits to agentsview Hits: it looks up
// each doc_key's (session_id, ordinal, content) in the mirror in one query
// and computes a snippet by re-splitting content. Hits whose doc_key
// vanished from the mirror mid-flight (a concurrent Refresh reconciled it
// away) are dropped rather than erroring, since the search itself still
// succeeded.
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
		out = append(out, Hit{
			SessionID: doc.sessionID,
			Ordinal:   doc.ordinal,
			Score:     h.Score,
			Snippet:   ix.snippet(doc.content, h.ChunkIndex),
		})
	}
	return out, nil
}

// lookupMirrorDocs reads (session_id, ordinal, content) for each of docKeys
// from vector_messages in a single query, keyed by doc_key. A key with no
// matching row is simply absent from the result.
func (ix *Index) lookupMirrorDocs(ctx context.Context, docKeys []string) (map[string]mirrorDoc, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(docKeys)), ",")
	args := make([]any, len(docKeys))
	for i, k := range docKeys {
		args[i] = k
	}

	rows, err := ix.db.QueryContext(ctx, `
SELECT doc_key, session_id, ordinal, content FROM vector_messages
 WHERE doc_key IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("look up search hit documents: %w", err)
	}
	defer rows.Close()

	docs := make(map[string]mirrorDoc, len(docKeys))
	for rows.Next() {
		var key string
		var doc mirrorDoc
		if err := rows.Scan(&key, &doc.sessionID, &doc.ordinal, &doc.content); err != nil {
			return nil, fmt.Errorf("scan search hit document: %w", err)
		}
		docs[key] = doc
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("look up search hit documents: %w", err)
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
