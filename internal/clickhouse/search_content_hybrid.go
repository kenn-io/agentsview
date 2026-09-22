package clickhouse

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// hybridDisplay carries what one fused unit needs for presentation: the
// anchor (session, ordinal) the match reports and enriches by, the unit's
// ordinal span and subordinate flag, plus the leg's raw approximate snippet
// text used only to center the redacted window.
type hybridDisplay struct {
	sessionID    string
	ordinal      int
	ordinalStart int
	ordinalEnd   int
	subordinate  bool
	snippet      string
}

// hybridLeg is one rank-ordered fusion leg: entries for db.RRFMerge plus
// each key's display info.
type hybridLeg struct {
	ranked  []db.RankedUnit
	display map[string]hybridDisplay
}

// maxHybridKeywordBatches caps how many k-row keyword batches the keyword
// leg fetches when collapse and scope filtering discard most of a batch.
const maxHybridKeywordBatches = 4

// searchContentHybrid runs mode "hybrid" on the ClickHouse store, mirroring
// the PostgreSQL store: the vector and keyword rankings are each over-fetched
// to k, the vector leg filtered to sessions passing the metadata scope,
// keyword message hits resolved to their containing units, and the two legs
// fused at unit granularity with db.RRFMerge. The keyword leg is the mirror's
// recency-ordered term-AND ILIKE ranking, the same substitution the
// PostgreSQL store makes for BM25. The caller has already validated the
// filter and confirmed a searcher is wired.
func (s *Store) searchContentHybrid(
	ctx context.Context, f db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	searcher := s.getVectorSearcher()
	if searcher == nil {
		return db.ContentSearchPage{}, s.semanticUnavailableError()
	}
	k := max(f.Limit*4, db.SemanticOverfetchMin)
	vecLeg, err := s.hybridVectorLeg(ctx, f, searcher, k)
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	kwLeg, err := s.hybridKeywordLeg(ctx, f, searcher, k)
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	if len(vecLeg.ranked) == 0 && len(kwLeg.ranked) == 0 {
		return db.ContentSearchPage{}, nil
	}
	merged := db.RRFMerge([][]db.RankedUnit{vecLeg.ranked, kwLeg.ranked}, f.Limit)
	return s.enrichHybridMatches(ctx, f, merged, vecLeg.display, kwLeg.display)
}

// hybridVectorLeg over-fetches k semantic unit hits, drops any outside the
// filter's scope (via survivingVectorHits, shared with the semantic mode),
// and returns the survivors as a rank-ordered fusion leg keyed by unit.
func (s *Store) hybridVectorLeg(
	ctx context.Context, f db.ContentSearchFilter, searcher db.VectorSearcher, k int,
) (hybridLeg, error) {
	leg := hybridLeg{display: make(map[string]hybridDisplay)}
	surviving, err := s.survivingVectorHits(ctx, f, searcher, k)
	if err != nil {
		return hybridLeg{}, err
	}
	for _, h := range surviving {
		key := db.UnitFusionKey(h.SessionID, h.OrdinalStart)
		if _, seen := leg.display[key]; seen {
			continue
		}
		leg.ranked = append(leg.ranked, db.RankedUnit{Key: key, Subordinate: h.Subordinate})
		leg.display[key] = hybridDisplay{
			sessionID: h.SessionID, ordinal: h.Ordinal, snippet: h.Snippet,
			ordinalStart: h.OrdinalStart, ordinalEnd: h.OrdinalEnd,
			subordinate: h.Subordinate,
		}
	}
	return leg, nil
}

// hybridKeywordLeg runs a recency-ordered term-AND ILIKE query over the
// embeddable message universe scoped to sessions passing the filter,
// resolves each hit to its containing unit, drops units outside f.Scope,
// and returns up to k hits as a fusion leg. Rows are fetched in batches of
// k with OFFSET continuation until the leg holds k entries, the stream is
// exhausted, or maxHybridKeywordBatches is reached.
func (s *Store) hybridKeywordLeg(
	ctx context.Context, f db.ContentSearchFilter, searcher db.VectorSearcher, k int,
) (hybridLeg, error) {
	leg := hybridLeg{display: make(map[string]hybridDisplay, k)}
	for batch := range maxHybridKeywordBatches {
		hits, err := s.fetchHybridKeywordBatch(ctx, f, k, batch*k)
		if err != nil {
			return hybridLeg{}, err
		}
		if err := s.appendHybridKeywordHits(ctx, searcher, f, hits, &leg); err != nil {
			return hybridLeg{}, err
		}
		if len(hits) < k || len(leg.ranked) >= k {
			break
		}
	}
	return leg, nil
}

// fetchHybridKeywordBatch fetches one recency-ordered batch of at most k
// keyword message rows starting at offset. (session_id, ordinal) is the
// deterministic tiebreak so OFFSET continuation is stable across batches.
func (s *Store) fetchHybridKeywordBatch(
	ctx context.Context, f db.ContentSearchFilter, k, offset int,
) ([]hybridDisplay, error) {
	scopeWhere, scopeArgs := db.BuildSessionBaseFilterSQL(semanticSessionFilter(f), db.ClickHouseQueryDialect())
	scopeWhere, scopeArgs = db.AppendExcludeSessionIDs(scopeWhere, scopeArgs, "id", f.ExcludeSessionIDs)
	kf := f
	kf.Mode = "fts"
	var args []any
	contentPred := contentSearchPredicate("m.content", kf, &args)
	args = append(args, scopeArgs...)
	args = append(args, k, offset)
	rows, err := s.queryContext(ctx, `
		SELECT m.session_id, m.ordinal, m.content
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE `+contentPred+`
			AND m.role IN ('user', 'assistant')
			AND `+embeddableMessagePredicate("m")+`
			AND m.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)
		ORDER BY COALESCE(s.ended_at, s.started_at, s.created_at) DESC NULLS LAST,
			m.session_id ASC, m.ordinal ASC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse hybrid keyword leg: %w", err)
	}
	defer rows.Close()
	var hits []hybridDisplay
	for rows.Next() {
		var hit hybridDisplay
		var ordinal int64
		var content string
		if err := rows.Scan(&hit.sessionID, &ordinal, &content); err != nil {
			return nil, fmt.Errorf("scanning clickhouse hybrid keyword hit: %w", err)
		}
		hit.ordinal = int(ordinal)
		start, end := db.FTSSnippetRange(f.Pattern, content)
		lo, hi := snippetBounds(content, start, end, 60)
		hit.snippet = content[lo:hi]
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// appendHybridKeywordHits resolves one batch of keyword hits to their
// containing units, classifies unit-less hits structurally so the scope
// filter and fusion penalty treat them like lexical mode, and accumulates
// the survivors into leg; a unit already seen keeps its earlier entry.
func (s *Store) appendHybridKeywordHits(
	ctx context.Context, searcher db.VectorSearcher, f db.ContentSearchFilter,
	hits []hybridDisplay, leg *hybridLeg,
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
	if err := s.classifyUnitlessHybridHits(ctx, hits, unitless); err != nil {
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

// classifyUnitlessHybridHits assigns each unit-less keyword hit its
// structurally derived conversation-unit range and subordinate flag before
// scope filtering and fusion, reusing the lexical anchor metadata lookup and
// db.DeriveUnitRanges.
func (s *Store) classifyUnitlessHybridHits(
	ctx context.Context, hits []hybridDisplay, idxs []int,
) error {
	if len(idxs) == 0 {
		return nil
	}
	matches := make([]db.ContentMatch, len(idxs))
	for k, i := range idxs {
		matches[k] = db.ContentMatch{SessionID: hits[i].sessionID, Ordinal: hits[i].ordinal}
	}
	metas, err := s.fillAnchorMeta(ctx, matches)
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

// enrichHybridMatches looks up metadata for the fused units and assembles
// the page in fused-score order. When the keyword leg contributed a unit its
// display wins: the match anchors on the keyword-matched message and centers
// on the keyword snippet. Either way the snippet is built and redacted from
// the anchor message's full content.
func (s *Store) enrichHybridMatches(
	ctx context.Context, f db.ContentSearchFilter, merged []db.FusedUnit,
	vecDisplay, kwDisplay map[string]hybridDisplay,
) (db.ContentSearchPage, error) {
	displays := make([]hybridDisplay, len(merged))
	asHits := make([]db.VectorHit, len(merged))
	for i, m := range merged {
		d, ok := kwDisplay[m.Unit.Key]
		if !ok {
			d = vecDisplay[m.Unit.Key]
		}
		displays[i] = d
		asHits[i] = db.VectorHit{SessionID: d.sessionID, Ordinal: d.ordinal}
	}
	meta, err := s.enrichSemanticHits(ctx, asHits)
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
