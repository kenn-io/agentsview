package clickhouse

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// searchContentSemantic runs mode "semantic" on the ClickHouse store,
// mirroring the PostgreSQL store: it over-fetches ranked hits from the
// installed searcher, keeps hits whose session passes the filter's metadata
// scope (f.Scope governs subordinate-unit visibility instead of the
// sidebar-child exclusion), routes the survivors through the same
// subordinate penalty hybrid uses, enriches the anchor messages in one
// query, and returns them in rank order truncated to f.Limit. The caller has
// already run db.ValidateSemanticFilter and confirmed a searcher is wired.
func (s *Store) searchContentSemantic(
	ctx context.Context, f db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	searcher := s.getVectorSearcher()
	if searcher == nil {
		return db.ContentSearchPage{}, s.semanticUnavailableError()
	}
	k := max(f.Limit*4, db.SemanticOverfetchMin)
	surviving, err := s.survivingVectorHits(ctx, f, searcher, k)
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	if len(surviving) == 0 {
		return db.ContentSearchPage{}, nil
	}
	surviving = db.ApplySubordinatePenalty(surviving)

	meta, err := s.enrichSemanticHits(ctx, surviving)
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

// survivingVectorHits over-fetches k ranked hits and keeps only those whose
// session passes the filter's metadata scope and whose subordinate flag
// falls inside f.Scope, preserving rank order. Shared by the semantic mode
// and the hybrid vector leg.
func (s *Store) survivingVectorHits(
	ctx context.Context, f db.ContentSearchFilter, searcher db.VectorSearcher, k int,
) ([]db.VectorHit, error) {
	hits, err := searcher.SemanticSearch(ctx, f.Pattern, k)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.SessionID)
	}
	allowed, err := s.semanticAllowedSessionIDs(ctx, f, uniqueIDs(ids))
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

// semanticSessionFilter is the content-search session scope for the
// semantic and hybrid modes: the shared db.ContentSessionFilter mapping plus
// the child one-shot exemption, so child sessions are not dropped by the
// one-shot gate while top-level one-shots keep their exclusion. Both modes
// build their SQL with db.BuildSessionBaseFilterSQL, never the sidebar
// filter: unit visibility for child sessions is governed by f.Scope, which
// supersedes IncludeChildren, matching the SQLite and PostgreSQL paths.
func semanticSessionFilter(f db.ContentSearchFilter) db.SessionFilter {
	sf := db.ContentSessionFilter(f)
	sf.ChildExemptOneShot = true
	return sf
}

// semanticAllowedSessionIDs returns the subset of ids whose session passes
// the filter's metadata scope, reusing the shared session filter SQL so this
// path cannot drift from the substring and regex scope.
func (s *Store) semanticAllowedSessionIDs(
	ctx context.Context, f db.ContentSearchFilter, ids []string,
) (map[string]bool, error) {
	allowed := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return allowed, nil
	}
	scopeWhere, scopeArgs := db.BuildSessionBaseFilterSQL(semanticSessionFilter(f), db.ClickHouseQueryDialect())
	scopeWhere, scopeArgs = db.AppendExcludeSessionIDs(scopeWhere, scopeArgs, "id", f.ExcludeSessionIDs)
	for batch := range idBatches(ids) {
		placeholders, args := inArgs(batch)
		if err := func() error {
			rows, err := s.queryContext(ctx,
				"SELECT id FROM sessions WHERE "+scopeWhere+" AND id IN ("+placeholders+")",
				append(append([]any{}, scopeArgs...), args...)...)
			if err != nil {
				return fmt.Errorf("clickhouse semantic search session scope: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return fmt.Errorf("scanning clickhouse semantic session id: %w", err)
				}
				allowed[id] = true
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
	}
	return allowed, nil
}

// semanticHitInfo is the session and message metadata attached to a hit.
// content is the anchor message's full content: snippets are built from it
// so secret redaction sees the same whole-body context the lexical paths do.
type semanticHitInfo struct {
	project, agent, role, timestamp, content string
	relationshipType, parentSessionID        string
	isSidechain                              bool
}

// enrichSemanticHits looks up session and message metadata for the hits'
// anchor (session_id, ordinal) pairs. A hit whose anchor message is missing
// from the mirror is absent from the map and dropped by the caller.
func (s *Store) enrichSemanticHits(
	ctx context.Context, hits []db.VectorHit,
) (map[db.MessageRef]semanticHitInfo, error) {
	out := make(map[db.MessageRef]semanticHitInfo, len(hits))
	seen := make(map[db.MessageRef]struct{}, len(hits))
	refs := make([]db.MessageRef, 0, len(hits))
	for _, h := range hits {
		ref := db.MessageRef{SessionID: h.SessionID, Ordinal: h.Ordinal}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	for start := 0; start < len(refs); start += vectorLookupChunk {
		chunk := refs[start:min(start+vectorLookupChunk, len(refs))]
		if err := s.enrichSemanticHitChunk(ctx, chunk, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) enrichSemanticHitChunk(
	ctx context.Context, refs []db.MessageRef, out map[db.MessageRef]semanticHitInfo,
) error {
	parts := make([]string, len(refs))
	args := make([]any, 0, len(refs)*2)
	for i, r := range refs {
		parts[i] = "SELECT ? AS session_id, toInt64(?) AS ordinal"
		args = append(args, r.SessionID, r.Ordinal)
	}
	rows, err := s.queryContext(ctx, `
		WITH refs AS (`+strings.Join(parts, " UNION ALL ")+`)
		SELECT m.session_id, s.project, s.agent, m.role, m.ordinal,
			m.timestamp, m.content,
			COALESCE(s.relationship_type, ''), COALESCE(s.parent_session_id, ''),
			m.is_sidechain
		FROM refs r
		JOIN messages m ON m.session_id = r.session_id AND m.ordinal = r.ordinal
		JOIN sessions s ON s.id = m.session_id`, args...)
	if err != nil {
		return fmt.Errorf("clickhouse semantic search enrich: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref db.MessageRef
		var info semanticHitInfo
		var ordinal int64
		var ts any
		if err := rows.Scan(&ref.SessionID, &info.project, &info.agent,
			&info.role, &ordinal, &ts, &info.content,
			&info.relationshipType, &info.parentSessionID,
			&info.isSidechain); err != nil {
			return fmt.Errorf("scanning clickhouse semantic hit: %w", err)
		}
		ref.Ordinal = int(ordinal)
		info.timestamp = formatDBTime(ts)
		out[ref] = info
	}
	return rows.Err()
}
