package postgres

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// searchContentSemanticPG runs mode "semantic" on the PostgreSQL store,
// mirroring internal/db.searchContentSemantic exactly with PG idioms: it
// over-fetches ranked hits from the wired VectorSearcher, keeps hits whose
// session passes the filter's metadata scope (the sidebar-child exclusion is
// lifted -- f.Scope governs subordinate-unit visibility instead, dropping hits
// db.ScopeExcludes rules out), routes the survivors through the same RRF merge
// hybrid uses as a one-leg fusion (db.ApplySubordinatePenalty, so subordinate
// units are penalized identically while matches keep the searcher's own
// scores), enriches surviving anchor (session_id, ordinal) pairs with
// session/message metadata in one query, and returns them in the fused order,
// truncated to f.Limit. The caller (SearchContent) has already run
// db.ValidateSemanticFilter and confirmed a searcher is wired.
func (s *Store) searchContentSemanticPG(
	ctx context.Context, f db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	searcher := s.getVectorSearcher()
	if searcher == nil {
		return db.ContentSearchPage{}, s.semanticUnavailableError()
	}

	k := max(f.Limit*4, db.SemanticOverfetchMin)
	surviving, err := s.survivingVectorHitsPG(ctx, f, searcher, k)
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	if len(surviving) == 0 {
		return db.ContentSearchPage{}, nil
	}
	surviving = db.ApplySubordinatePenalty(surviving)

	meta, err := s.enrichSemanticHitsPG(ctx, surviving)
	if err != nil {
		return db.ContentSearchPage{}, err
	}

	out := make([]db.ContentMatch, 0, min(len(surviving), f.Limit))
	for _, h := range surviving {
		info, ok := meta[db.MessageRef{SessionID: h.SessionID, Ordinal: h.Ordinal}]
		if !ok {
			continue
		}
		score := float64(h.Score)
		out = append(out, db.ContentMatch{
			SessionID:       h.SessionID,
			Project:         info.project,
			Agent:           info.agent,
			Location:        "message",
			Role:            info.role,
			Ordinal:         h.Ordinal,
			OrdinalRange:    [2]int{h.OrdinalStart, h.OrdinalEnd},
			Subordinate:     h.Subordinate,
			Relationship:    info.relationshipType,
			ParentSessionID: info.parentSessionID,
			Sidechain:       info.isSidechain,
			Timestamp:       info.timestamp,
			Snippet:         f.SemanticSnippet(info.content, h.Snippet),
			Score:           &score,
		})
		if len(out) >= f.Limit {
			break
		}
	}
	return db.ContentSearchPage{Matches: out}, nil
}

// survivingVectorHitsPG over-fetches k ranked hits from the searcher and keeps
// only those whose session passes the filter's metadata scope (the
// child-exclusion-lifted lookup) and whose subordinate flag falls inside
// f.Scope, preserving the searcher's rank order. Shared by the semantic mode
// and the hybrid vector leg so both filter the vector candidates identically.
func (s *Store) survivingVectorHitsPG(
	ctx context.Context, f db.ContentSearchFilter, searcher db.VectorSearcher, k int,
) ([]db.VectorHit, error) {
	hits, err := searcher.SemanticSearch(ctx, f.Pattern, k)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	allowed, err := s.semanticAllowedSessionIDsPG(ctx, f, pgUniqueSessionIDs(hits))
	if err != nil {
		return nil, err
	}
	surviving := make([]db.VectorHit, 0, len(hits))
	for _, h := range hits {
		if allowed[h.SessionID] && !db.ScopeExcludes(f.Scope, h.Subordinate) {
			surviving = append(surviving, h)
		}
	}
	return surviving, nil
}

// pgUniqueSessionIDs returns the distinct session IDs referenced by hits.
// Order is irrelevant: the result only feeds an ANY(...) array bind.
func pgUniqueSessionIDs(hits []db.VectorHit) []string {
	seen := make(map[string]bool, len(hits))
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if !seen[h.SessionID] {
			seen[h.SessionID] = true
			ids = append(ids, h.SessionID)
		}
	}
	return ids
}

// semanticPGSessionFilter maps a ContentSearchFilter for the semantic/hybrid
// session scope: the shared pgSessionFilter mapping plus the child one-shot
// exemption (SessionFilter.ChildExemptOneShot) -- child sessions must not be
// dropped by the one-shot gate in these modes, while top-level one-shots keep
// today's exclusion. It mirrors internal/db.semanticContentSessionFilter.
func semanticPGSessionFilter(f db.ContentSearchFilter) db.SessionFilter {
	sf := pgSessionFilter(f)
	sf.ChildExemptOneShot = true
	return sf
}

// semanticAllowedSessionIDsPG returns the subset of ids whose session passes
// the ContentSearchFilter's metadata scope (project, agent, date range,
// one-shot/automated, ...), reusing buildPGSessionBaseFilter so this path
// cannot drift from the substring/regex scope subquery. Like SQLite's
// semanticAllowedSessionIDs it omits the sidebar-child exclusion and exempts
// child sessions from the one-shot gate (semanticPGSessionFilter): in
// semantic/hybrid modes Scope supersedes IncludeChildren, so subordinate units
// stay visible to the vector leg. The whole id set binds as one array
// parameter (pgx expands ANY natively), so no IN chunking is needed.
func (s *Store) semanticAllowedSessionIDsPG(
	ctx context.Context, f db.ContentSearchFilter, ids []string,
) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	where, args := buildPGSessionBaseFilter(semanticPGSessionFilter(f))
	query := fmt.Sprintf(
		"SELECT id FROM sessions WHERE %s AND id = ANY($%d)", where, len(args)+1)
	args = append(args, ids)

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("pg semantic search session scope: %w", err)
	}
	defer func() { _ = rows.Close() }()

	allowed := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pg semantic session id: %w", err)
		}
		allowed[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return allowed, nil
}

// pgSemanticHitInfo is the session/message metadata enrichSemanticHitsPG
// attaches to a surviving hit. content is the message's full, un-truncated
// content: semantic/hybrid snippets are built from it (SemanticSnippet) rather
// than the searcher's pre-truncated chunk text, so secret redaction sees the
// same whole-body context the lexical paths give it. relationshipType,
// parentSessionID, and isSidechain carry the hit's lineage joined from the
// sessions/messages rows; isSidechain is the ANCHOR ordinal's message flag.
type pgSemanticHitInfo struct {
	project, agent, role, timestamp, content string
	relationshipType, parentSessionID        string
	isSidechain                              bool
}

// enrichSemanticHitsPG looks up session/message metadata for hits' anchor
// (session_id, ordinal) pairs via parallel unnest arrays joined to
// messages/sessions, keyed by db.MessageRef{SessionID, Ordinal}. A hit whose
// anchor message is missing from PG is absent from the map and dropped by the
// caller, matching SQLite's enrichSemanticHits. The whole batch binds as two
// array parameters (session_ids text[], ordinals int[]), so any hit count
// stays well under PG's bind limit.
func (s *Store) enrichSemanticHitsPG(
	ctx context.Context, hits []db.VectorHit,
) (map[db.MessageRef]pgSemanticHitInfo, error) {
	out := make(map[db.MessageRef]pgSemanticHitInfo, len(hits))
	if len(hits) == 0 {
		return out, nil
	}
	sessionIDs := make([]string, len(hits))
	ordinals := make([]int32, len(hits))
	for i, h := range hits {
		sessionIDs[i] = h.SessionID
		ordinals[i] = int32(h.Ordinal)
	}

	const query = `
SELECT m.session_id, s.project, s.agent, m.role, m.ordinal,
       COALESCE(m.timestamp::text, ''), m.content,
       COALESCE(s.relationship_type, ''), COALESCE(s.parent_session_id, ''),
       m.is_sidechain
  FROM (SELECT unnest($1::text[]) AS session_id,
               unnest($2::int[]) AS ordinal) h
  JOIN messages m ON m.session_id = h.session_id AND m.ordinal = h.ordinal
  JOIN sessions s ON s.id = m.session_id`

	rows, err := s.pg.QueryContext(ctx, query, sessionIDs, ordinals)
	if err != nil {
		return nil, fmt.Errorf("pg semantic search enrich: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var ref db.MessageRef
		var info pgSemanticHitInfo
		if err := rows.Scan(&ref.SessionID, &info.project, &info.agent,
			&info.role, &ref.Ordinal, &info.timestamp, &info.content,
			&info.relationshipType, &info.parentSessionID,
			&info.isSidechain); err != nil {
			return nil, fmt.Errorf("scan pg semantic hit: %w", err)
		}
		out[ref] = info
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
