package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	stale, err := s.staleVectorHitsPG(ctx, surviving)
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
			SessionID:          h.SessionID,
			Project:            info.project,
			Agent:              info.agent,
			TranscriptRevision: pgBoundRevision(stale, h, info),
			Location:           "message",
			Role:               info.role,
			Ordinal:            h.Ordinal,
			OrdinalRange:       [2]int{h.OrdinalStart, h.OrdinalEnd},
			Subordinate:        h.Subordinate,
			Relationship:       info.relationshipType,
			ParentSessionID:    info.parentSessionID,
			Sidechain:          info.isSidechain,
			Timestamp:          info.timestamp,
			Snippet:            f.SemanticSnippet(info.content, h.Snippet),
			Score:              &score,
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
// session scope: the shared db.ContentSessionFilter mapping plus the child one-shot
// exemption (SessionFilter.ChildExemptOneShot) -- child sessions must not be
// dropped by the one-shot gate in these modes, while top-level one-shots keep
// today's exclusion. It mirrors internal/db.semanticContentSessionFilter.
func semanticPGSessionFilter(f db.ContentSearchFilter) db.SessionFilter {
	sf := db.ContentSessionFilter(f)
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
	where, args = appendExcludeSessionIDsPG(where, args, "id", f.ExcludeSessionIDs)
	query := fmt.Sprintf(
		"SELECT id FROM sessions WHERE %s AND id = ANY($%d)", where, len(args)+1)
	args = append(args, ids)

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("pg semantic search session scope: %w", err)
	}
	defer rows.Close()
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

// staleVectorHitsPG is the PostgreSQL twin of internal/db's staleVectorHits:
// it reports which hits were ranked from content that no longer matches the
// archive. Each hit carries ContentHash, the vector_documents content_hash
// for the document its embedding was computed from; the current document
// content is rebuilt from the archive with the exact membership the embedding
// build used -- every embeddable (non-system, non-system-prefixed
// user/assistant) message between the unit's ordinal bounds, joined with
// "\n\n" for runs -- and hashed with db.UnitContentHash. The unit's end
// boundary is verified too: an embeddable assistant row now occupying
// ordinal_end+1 with the run's sidechain means the run grew past the recorded
// span, so the old hash can never prove currency. A hit whose recorded hash
// is empty or differs (or whose span has no embeddable members left) is
// stale: its score or snippet may describe older content, so the caller must
// not report it as revision-bound. The result is keyed by (session_id, unit
// start ordinal).
func (s *Store) staleVectorHitsPG(
	ctx context.Context, hits []db.VectorHit,
) (map[db.MessageRef]bool, error) {
	stale := make(map[db.MessageRef]bool, len(hits))
	type span struct {
		hit              db.VectorHit
		parts            []string
		isRun            bool
		runSide          *bool
		firstRole        string
		structureChanged bool
		extended         bool
	}
	byKey := make(map[db.MessageRef]*span, len(hits))
	sessionIDs := make([]string, 0, len(hits))
	los := make([]int32, 0, len(hits))
	his := make([]int32, 0, len(hits))
	for _, h := range hits {
		key := db.MessageRef{SessionID: h.SessionID, Ordinal: h.OrdinalStart}
		if _, seen := byKey[key]; seen {
			continue // two anchors on one unit: verify it once
		}
		if h.ContentHash == "" {
			// Without the mirror's recorded hash there is nothing to compare
			// against; fail closed.
			stale[key] = true
			continue
		}
		sp := &span{hit: h}
		byKey[key] = sp
		sessionIDs = append(sessionIDs, h.SessionID)
		los = append(los, int32(h.OrdinalStart))
		his = append(his, int32(h.OrdinalEnd))
	}
	if len(byKey) == 0 {
		return stale, nil
	}

	// hi+1 is the would-be extension row: a run grows only when an
	// embeddable assistant message with the run's sidechain lands there.
	// The probe is the first embeddable row after the recorded end (ignored
	// rows never split an embedding unit, so the physical next ordinal may
	// hide the real extension candidate).
	query := "SELECT sp.session_id, sp.lo, m.ordinal, m.content, m.role, m.is_sidechain " +
		"FROM (SELECT unnest($1::text[]) AS session_id, " +
		"unnest($2::int[]) AS lo, unnest($3::int[]) AS hi) sp " +
		"JOIN messages m ON m.session_id = sp.session_id " +
		"AND m.ordinal BETWEEN sp.lo AND sp.hi " +
		"WHERE m.role IN ('user','assistant') AND m.is_system = FALSE AND " +
		db.PostgresSystemPrefixSQL("m.content", "m.role") + " " +
		"UNION ALL " +
		"SELECT p.session_id, p.lo, m.ordinal, m.content, m.role, m.is_sidechain " +
		"FROM (SELECT sp.session_id, sp.lo, MIN(m.ordinal) AS next_ordinal " +
		"FROM (SELECT unnest($1::text[]) AS session_id, " +
		"unnest($2::int[]) AS lo, unnest($3::int[]) AS hi) sp " +
		"JOIN messages m ON m.session_id = sp.session_id " +
		"AND m.ordinal > sp.hi " +
		"WHERE m.role IN ('user','assistant') AND m.is_system = FALSE AND " +
		db.PostgresSystemPrefixSQL("m.content", "m.role") + " " +
		"GROUP BY sp.session_id, sp.lo) p " +
		"JOIN messages m ON m.session_id = p.session_id " +
		"AND m.ordinal = p.next_ordinal " +
		"ORDER BY 1, 2, 3"
	rows, err := s.pg.QueryContext(ctx, query, sessionIDs, los, his)
	if err != nil {
		return nil, fmt.Errorf("pg verify vector hit currency: %w", err)
	}
	defer rows.Close()
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sessionID, role string
		var lo, ordinal int
		var content string
		var sidechain bool
		if err := rows.Scan(&sessionID, &lo, &ordinal, &content,
			&role, &sidechain); err != nil {
			return nil, fmt.Errorf("scan pg vector hit verification row: %w", err)
		}
		sp, ok := byKey[db.MessageRef{SessionID: sessionID, Ordinal: lo}]
		if !ok {
			continue
		}
		if ordinal > sp.hit.OrdinalEnd {
			// Candidate extension row past the recorded span. A run grows
			// only when an embeddable assistant message with the run's
			// sidechain lands there; a user document never extends, and
			// anything else closes the unit at the recorded end.
			if sp.isRun && role == "assistant" && sidechain == *sp.runSide {
				sp.extended = true
			}
			continue
		}
		if sp.runSide == nil {
			sp.isRun = role == "assistant"
			sp.firstRole = role
			runSide := sidechain
			sp.runSide = &runSide
		} else if role != sp.firstRole || sidechain != *sp.runSide {
			// Units are homogeneous: a user row never sits inside an
			// assistant run, and a sidechain flip splits the run. An
			// in-span row that changed role or sidechain means the current
			// structure differs from the indexed unit even when every
			// content byte is unchanged.
			sp.structureChanged = true
		}
		sp.parts = append(sp.parts, content)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pg vector hit verification rows: %w", err)
	}
	for key, sp := range byKey {
		if sp.extended || sp.structureChanged ||
			db.UnitContentHash(strings.Join(sp.parts, "\n\n")) != sp.hit.ContentHash {
			stale[key] = true
		}
	}
	return stale, nil
}

// pgBoundRevision returns the match's transcript revision, or "" when the
// hit was ranked from stale content: an empty revision marks the match
// unbound so the page's revision_bound flag stays honest about what the
// ranking evidence was computed from.
func pgBoundRevision(
	stale map[db.MessageRef]bool, h db.VectorHit, info pgSemanticHitInfo,
) string {
	if stale[db.MessageRef{SessionID: h.SessionID, Ordinal: h.OrdinalStart}] {
		return ""
	}
	return info.transcriptRevision
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
	transcriptRevision                       string
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
       m.timestamp, m.content,
       COALESCE(s.transcript_revision, ''),
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
	defer rows.Close()
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var ref db.MessageRef
		var info pgSemanticHitInfo
		var ts *time.Time
		if err := rows.Scan(&ref.SessionID, &info.project, &info.agent,
			&info.role, &ref.Ordinal, &ts, &info.content,
			&info.transcriptRevision,
			&info.relationshipType, &info.parentSessionID,
			&info.isSidechain); err != nil {
			return nil, fmt.Errorf("scan pg semantic hit: %w", err)
		}
		if ts != nil {
			info.timestamp = FormatISO8601(*ts)
		}
		out[ref] = info
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
