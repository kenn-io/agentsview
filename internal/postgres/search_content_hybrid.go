package postgres

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// pgHybridDisplay carries what one fused unit needs for presentation: the
// anchor (session, ordinal) the match reports and enriches by, the unit's
// ordinal span and subordinate flag (structurally derived for a unit-less
// message-granularity keyword hit, see classifyUnitlessHybridHitsPG), plus the
// leg's raw (unredacted) approximate snippet text used only to center the
// redacted window. It is the PG twin of internal/db's hybridDisplay.
type pgHybridDisplay struct {
	sessionID    string
	ordinal      int
	ordinalStart int
	ordinalEnd   int
	subordinate  bool
	snippet      string
}

// pgHybridLeg is one rank-ordered fusion leg: entries for db.RRFMerge plus each
// key's display info.
type pgHybridLeg struct {
	ranked  []db.RankedUnit
	display map[string]pgHybridDisplay
}

// searchContentHybridPG runs mode "hybrid" on the PostgreSQL store, mirroring
// internal/db.searchContentHybrid with the keyword-leg substitution PG uses
// (ILIKE candidates ordered by recency instead of BM25 rank). The vector and
// keyword rankings are each over-fetched to k, the vector leg filtered down to
// sessions passing the filter's metadata scope, keyword message hits resolved
// to their containing units, and the two rank-ordered leg lists fused at unit
// granularity with db.RRFMerge. Returned matches are enriched with
// session/message metadata via the same lookup semantic search uses, ordered
// by fused score descending, truncated to f.Limit. The caller (SearchContent)
// has already run db.ValidateSemanticFilter and confirmed a searcher is wired.
func (s *Store) searchContentHybridPG(
	ctx context.Context, f db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	searcher := s.getVectorSearcher()
	if searcher == nil {
		return db.ContentSearchPage{}, s.semanticUnavailableError()
	}

	k := max(f.Limit*4, db.SemanticOverfetchMin)
	vecLeg, err := s.hybridVectorLegPG(ctx, f, searcher, k)
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	kwLeg, err := s.hybridKeywordLegPG(ctx, f, searcher, k)
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	if len(vecLeg.ranked) == 0 && len(kwLeg.ranked) == 0 {
		return db.ContentSearchPage{}, nil
	}

	merged := db.RRFMerge([][]db.RankedUnit{vecLeg.ranked, kwLeg.ranked}, f.Limit)
	return s.enrichHybridMatchesPG(ctx, f, merged, vecLeg.display, kwLeg.display)
}

// hybridVectorLegPG over-fetches k semantic unit hits, drops any whose session
// fails the filter's metadata scope or whose subordinate flag falls outside
// f.Scope (via survivingVectorHitsPG, shared with the semantic mode), and
// returns the survivors as a rank-ordered fusion leg keyed by unit. Both
// filters run before the merge so reciprocal-rank fusion only ranks eligible
// units and a scoped search can still fill the limit.
func (s *Store) hybridVectorLegPG(
	ctx context.Context, f db.ContentSearchFilter, searcher db.VectorSearcher, k int,
) (pgHybridLeg, error) {
	leg := pgHybridLeg{display: make(map[string]pgHybridDisplay)}
	surviving, err := s.survivingVectorHitsPG(ctx, f, searcher, k)
	if err != nil {
		return pgHybridLeg{}, err
	}
	for _, h := range surviving {
		key := db.UnitFusionKey(h.SessionID, h.OrdinalStart)
		if _, seen := leg.display[key]; seen {
			continue
		}
		leg.ranked = append(leg.ranked, db.RankedUnit{Key: key, Subordinate: h.Subordinate})
		leg.display[key] = pgHybridDisplay{
			sessionID: h.SessionID, ordinal: h.Ordinal, snippet: h.Snippet,
			ordinalStart: h.OrdinalStart, ordinalEnd: h.OrdinalEnd,
			subordinate: h.Subordinate,
		}
	}
	return leg, nil
}

// maxHybridKeywordBatches caps how many k-row ILIKE batches the keyword leg
// fetches. It is the PG twin of internal/db.maxHybridFTSBatches (unexported
// there); defined locally so this diff stays self-contained. It bounds the
// worst-case work when discard dominates — many rows collapsing into one unit,
// or a narrow f.Scope dropping most rows — while letting the leg keep paging
// past discarded rows instead of under-filling after the first batch.
const maxHybridKeywordBatches = 4

// hybridKeywordLegPG runs a recency-ordered ILIKE query over the embedded
// universe (role user/assistant, is_system = FALSE, system-prefix excluded),
// scoped in SQL to sessions passing the child-exclusion-lifted filter (so both
// hybrid legs see the same universe), resolves each message hit to its
// containing unit, drops units outside f.Scope, and returns up to k hits as a
// rank-ordered fusion leg. A hit inside a unit adopts the unit's fusion key and
// subordinate flag while keeping its own message ordinal as the anchor and its
// keyword snippet for display; several hits in one unit collapse to the
// best-ranked one. A hit with no containing unit keeps a message-granularity
// key and survives fusion on its own, with its range and subordinate flag
// structurally derived before the scope filter and merge. Rows are fetched in
// recency-ordered batches of k with OFFSET continuation: collapse and scope
// filtering can discard most of a batch, so the leg keeps fetching until it
// holds k entries, the stream is exhausted, or maxHybridKeywordBatches is hit.
func (s *Store) hybridKeywordLegPG(
	ctx context.Context, f db.ContentSearchFilter, searcher db.VectorSearcher, k int,
) (pgHybridLeg, error) {
	leg := pgHybridLeg{display: make(map[string]pgHybridDisplay, k)}
	for batch := range maxHybridKeywordBatches {
		hits, err := s.fetchHybridKeywordBatchPG(ctx, f, k, batch*k)
		if err != nil {
			return pgHybridLeg{}, err
		}
		if err := s.appendHybridKeywordHitsPG(ctx, searcher, f, hits, &leg); err != nil {
			return pgHybridLeg{}, err
		}
		if len(hits) < k || len(leg.ranked) >= k {
			break
		}
	}
	return leg, nil
}

// fetchHybridKeywordBatchPG fetches one recency-ordered batch of at most k
// keyword message rows, starting at offset. The term-AND ILIKE predicate
// (pgContentSearchPredicate with Mode "fts") preserves SQLite's implicit-AND
// FTS-term semantics; recency ordering is the documented PG keyword ranking (no
// BM25), with (session_id, ordinal) as a deterministic tiebreak so OFFSET
// continuation is stable across batches. The display snippet is windowed in Go
// around the first case-insensitive pattern occurrence; it centers the redacted
// window only, since redaction (SemanticSnippet) always runs on full content.
func (s *Store) fetchHybridKeywordBatchPG(
	ctx context.Context, f db.ContentSearchFilter, k, offset int,
) ([]pgHybridDisplay, error) {
	scopeWhere, scopeArgs := buildPGSessionBaseFilter(semanticPGSessionFilter(f))
	pb := &paramBuilder{n: len(scopeArgs), args: append([]any{}, scopeArgs...)}

	kf := f
	kf.Mode = "fts"
	contentPred := pgContentSearchPredicate("m.content", kf, escapeLike(f.Pattern), pb)
	limitP := pb.add(k)
	offsetP := pb.add(offset)

	query := fmt.Sprintf(`
		SELECT m.session_id, m.ordinal, m.content
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
		  AND m.role IN ('user', 'assistant') AND m.is_system = FALSE
		  AND %s
		  AND m.session_id IN (SELECT id FROM sessions WHERE %s)
		ORDER BY COALESCE(s.ended_at, s.started_at, s.created_at) DESC NULLS LAST,
		         m.session_id ASC, m.ordinal ASC
		LIMIT %s OFFSET %s`,
		contentPred, db.PostgresSystemPrefixSQL("m.content", "m.role"),
		scopeWhere, limitP, offsetP)

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("pg hybrid keyword leg: %w", err)
	}
	defer rows.Close()

	var hits []pgHybridDisplay
	for rows.Next() {
		var hit pgHybridDisplay
		var content string
		if err := rows.Scan(&hit.sessionID, &hit.ordinal, &content); err != nil {
			return nil, fmt.Errorf("scan hybrid keyword hit: %w", err)
		}
		hit.snippet = pgKeywordApproxSnippet(content, f.Pattern)
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// pgKeywordApproxSnippet windows content around the pattern's term-aware span
// (db.FTSSnippetRange, the same helper the fts snippet path uses via
// pgSubstringSnippet), rune-snapped via pgSnippetBounds. Centering on the raw
// pattern would fall back to the message start for quoted phrases or multi-term
// queries whose pattern is not a literal substring; FTSSnippetRange instead
// locates the de-quoted phrase, then the first matched term, then the start. The
// result is the raw, unredacted centering text SemanticSnippet later locates in
// the full content; redaction always runs on the full content, not this window.
func pgKeywordApproxSnippet(content, pattern string) string {
	start, end := db.FTSSnippetRange(pattern, content)
	lo, hi := pgSnippetBounds(content, start, end)
	return content[lo:hi]
}

// appendHybridKeywordHitsPG resolves one batch of keyword message hits to their
// containing units, classifies unit-less hits structurally (range and
// subordinate flag, so the scope filter and fusion penalty treat them exactly
// like lexical mode), and accumulates the survivors into leg: hits outside
// scope are dropped, and a unit already seen (within or across batches) keeps
// its earlier, better-ranked entry.
func (s *Store) appendHybridKeywordHitsPG(
	ctx context.Context, searcher db.VectorSearcher, f db.ContentSearchFilter,
	hits []pgHybridDisplay, leg *pgHybridLeg,
) error {
	if len(hits) == 0 {
		return nil
	}
	refs := make([]db.MessageRef, len(hits))
	for i, hit := range hits {
		refs[i] = db.MessageRef{SessionID: hit.sessionID, Ordinal: hit.ordinal}
	}
	units, err := searcher.ResolveMessageUnits(ctx, refs)
	if err != nil {
		return fmt.Errorf("resolving keyword hits to units: %w", err)
	}
	if len(units) != len(refs) {
		return fmt.Errorf(
			"resolving keyword hits to units: got %d units for %d refs", len(units), len(refs))
	}

	keys := make([]string, len(hits))
	var unitless []int
	for i := range hits {
		hit := &hits[i]
		keys[i] = db.MessageFusionKey(hit.sessionID, hit.ordinal)
		hit.ordinalStart, hit.ordinalEnd = hit.ordinal, hit.ordinal
		if units[i].DocKey == "" {
			unitless = append(unitless, i)
			continue
		}
		keys[i] = db.UnitFusionKey(units[i].SessionID, units[i].OrdinalStart)
		hit.ordinalStart = units[i].OrdinalStart
		hit.ordinalEnd = units[i].OrdinalEnd
		hit.subordinate = units[i].Subordinate
	}
	if err := s.classifyUnitlessHybridHitsPG(ctx, hits, unitless); err != nil {
		return err
	}

	for i, hit := range hits {
		if db.ScopeExcludes(f.Scope, hit.subordinate) {
			continue
		}
		if _, seen := leg.display[keys[i]]; seen {
			continue
		}
		leg.ranked = append(leg.ranked, db.RankedUnit{Key: keys[i], Subordinate: hit.subordinate})
		leg.display[keys[i]] = hit
	}
	return nil
}

// classifyUnitlessHybridHitsPG assigns each unit-less keyword hit (the resolver
// returned no mirror unit; hits[i] for i in idxs) its structurally derived
// conversation-unit range and subordinate flag BEFORE scope filtering and
// fusion, so a unit-less sidechain (or subagent/fork-session) hit is excluded,
// included, and penalized exactly like lexical mode classifies the same anchor.
// It reuses fillAnchorMetaPG and db.DeriveUnitRanges + db.SubordinateSession,
// mirroring internal/db.classifyUnitlessHybridHits.
func (s *Store) classifyUnitlessHybridHitsPG(
	ctx context.Context, hits []pgHybridDisplay, idxs []int,
) error {
	if len(idxs) == 0 {
		return nil
	}
	matches := make([]db.ContentMatch, len(idxs))
	for k, i := range idxs {
		matches[k] = db.ContentMatch{SessionID: hits[i].sessionID, Ordinal: hits[i].ordinal}
	}
	metas, err := s.fillAnchorMetaPG(ctx, matches)
	if err != nil {
		return err
	}
	anchors := make([]db.UnitAnchor, len(idxs))
	for k := range idxs {
		meta := metas[k]
		anchors[k] = db.UnitAnchor{
			SessionID:  matches[k].SessionID,
			Ordinal:    matches[k].Ordinal,
			Role:       meta.role.String,
			Sidechain:  meta.sidechain.Valid && meta.sidechain.Bool,
			Embeddable: meta.embeddable.Valid && meta.embeddable.Bool,
			Missing:    meta.missing,
		}
	}
	ranges, err := db.DeriveUnitRanges(ctx, s, anchors)
	if err != nil {
		return fmt.Errorf("deriving hybrid unit-less ranges: %w", err)
	}
	for k, i := range idxs {
		hits[i].ordinalStart, hits[i].ordinalEnd = ranges[k][0], ranges[k][1]
		hits[i].subordinate = anchors[k].Sidechain ||
			db.SubordinateSession(metas[k].relationship, metas[k].parentSessionID)
	}
	return nil
}

// enrichHybridMatchesPG looks up session/message metadata for the fused units
// (reusing enrichSemanticHitsPG) and assembles the final page in fused-score
// order. When the keyword leg contributed a unit, its display wins: the match
// anchors on the keyword-matched message's ordinal and centers on the keyword
// snippet (the vector leg's chunk anchor may be a different run member). Either
// way the returned snippet is built (and redacted) from the anchor message's
// full content via SemanticSnippet, the guarantee mode "semantic" gives.
// Unit-less keyword rows arrive with their derived range and subordinate flag
// already assigned pre-merge, so no derivation runs here.
func (s *Store) enrichHybridMatchesPG(
	ctx context.Context, f db.ContentSearchFilter, merged []db.FusedUnit,
	vecDisplay, kwDisplay map[string]pgHybridDisplay,
) (db.ContentSearchPage, error) {
	displays := make([]pgHybridDisplay, len(merged))
	asHits := make([]db.VectorHit, len(merged))
	for i, m := range merged {
		d, ok := kwDisplay[m.Unit.Key]
		if !ok {
			d = vecDisplay[m.Unit.Key]
		}
		displays[i] = d
		asHits[i] = db.VectorHit{SessionID: d.sessionID, Ordinal: d.ordinal}
	}
	meta, err := s.enrichSemanticHitsPG(ctx, asHits)
	if err != nil {
		return db.ContentSearchPage{}, err
	}

	out := make([]db.ContentMatch, 0, len(merged))
	for i, m := range merged {
		d := displays[i]
		info, ok := meta[db.MessageRef{SessionID: d.sessionID, Ordinal: d.ordinal}]
		if !ok {
			continue
		}
		score := m.Score
		out = append(out, db.ContentMatch{
			SessionID:       d.sessionID,
			Project:         info.project,
			Agent:           info.agent,
			Location:        "message",
			Role:            info.role,
			Ordinal:         d.ordinal,
			OrdinalRange:    [2]int{d.ordinalStart, d.ordinalEnd},
			Subordinate:     d.subordinate,
			Relationship:    info.relationshipType,
			ParentSessionID: info.parentSessionID,
			Sidechain:       info.isSidechain,
			Timestamp:       info.timestamp,
			Snippet:         f.SemanticSnippet(info.content, d.snippet),
			Score:           &score,
		})
	}
	return db.ContentSearchPage{Matches: out}, nil
}
