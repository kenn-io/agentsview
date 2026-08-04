package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
)

type postgresFullTextCapability struct{}

type postgresSearchHitProjection struct {
	SessionID string  `bun:"session_id"`
	Ordinal   int     `bun:"ordinal"`
	Snippet   string  `bun:"snippet"`
	Rank      float64 `bun:"rank"`
	MatchPos  int     `bun:"match_pos"`
}

// SearchSession performs ILIKE substring search within a single
// session's messages, returning matching ordinals.
func (postgresFullTextCapability) SearchSession(
	ctx context.Context, store bun.IDB, sessionID, query string,
) ([]int, error) {
	if query == "" {
		return nil, nil
	}
	like := "%" + escapeLike(query) + "%"
	var rows []struct {
		Ordinal int `bun:"ordinal"`
	}
	err := store.NewRaw(`
		SELECT DISTINCT m.ordinal
		FROM messages m
		LEFT JOIN tool_calls tc
			ON tc.session_id = m.session_id
			AND tc.message_ordinal = m.ordinal
		WHERE m.session_id = ?0
			AND m.is_system = FALSE
			AND `+db.PostgresSystemPrefixSQL("m.content", "m.role")+`
			AND (m.content ILIKE ?1
				OR tc.result_content ILIKE ?1)
		ORDER BY m.ordinal ASC`,
		sessionID, like,
	).Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf(
			"searching session: %w", err,
		)
	}
	ordinals := make([]int, len(rows))
	for i, row := range rows {
		ordinals[i] = row.Ordinal
	}
	return ordinals, nil
}

// Available reports that PostgreSQL ILIKE search is available.
func (postgresFullTextCapability) Available() bool { return true }

// HasSemantic reports whether a PG vector searcher was wired at startup
// (pg serve found a generation matching its embeddings fingerprint). When
// false, SearchContent rejects "semantic"/"hybrid" modes up front with
// db.ErrSemanticUnavailable.
func (s *Store) HasSemantic() bool { return s.getVectorSearcher() != nil }

// escapeLike escapes SQL LIKE metacharacters so the bind
// parameter is treated as a literal substring.
func escapeLike(v string) string {
	return db.EscapeLikePattern(v)
}

// Search performs ILIKE-based full-text search across messages,
// grouped to one result per session via DISTINCT ON, UNION'd with a
// session name (display_name / first_message) branch.
func (postgresFullTextCapability) Search(
	ctx context.Context, store bun.IDB, f db.SearchFilter,
) ([]db.SearchHit, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSearchLimit {
		f.Limit = db.DefaultSearchLimit
	}

	// plainTerm is the de-quoted query joined back into one string. It feeds
	// the name-branch ILIKE (matching the typed text against the short session
	// name) and centers the message snippet, mirroring SQLite's plainQuery.
	// terms is the per-term decomposition: every term must appear in the
	// message content (AND), matching SQLite FTS5's implicit AND so the same
	// user query behaves identically across backends. An explicit exact phrase
	// (user-supplied leading quote) collapses to a single term, preserving the
	// exact-phrase opt-in.
	plainTerm := db.StripFTSQuotes(f.Query)
	terms := db.FTSTerms(f.Query)
	if plainTerm == "" || len(terms) == 0 {
		return nil, nil
	}
	// firstTerm anchors POSITION-based ordering and snippet centering.
	firstTerm := terms[0]

	// Validate Sort before interpolating into ORDER BY.
	// session_id ASC is a deterministic tie-breaker for both modes,
	// preventing pagination instability when sort keys are equal.
	// NULLS LAST ensures sessions with NULL timestamps sort after
	// sessions with real timestamps under DESC ordering.
	// match_priority: 1 = message content match, 2 = name-only match.
	// This ensures content matches always rank above name-only fallbacks
	// regardless of match_pos (name-only rows have match_pos=0 which would
	// otherwise sort them before content matches under match_pos ASC alone).
	// match_priority: 1 = message content match, 2 = name-only match.
	// Only applied in relevance mode so content matches rank above name-only
	// fallbacks. Recency mode orders purely by time so the newest session
	// wins regardless of match type.
	outerOrderBy := "match_priority ASC, match_pos ASC, session_ended_at DESC NULLS LAST, session_id ASC"
	if f.Sort == "recency" {
		outerOrderBy = "session_ended_at DESC NULLS LAST, session_id ASC"
	}

	// ?0 = escaped ILIKE pattern for the name branch (full plain term)
	// ?1 = raw first term (for POSITION — case folded in expression)
	args := []any{escapeLike(plainTerm), firstTerm}
	argIdx := 3

	// Message branch matches every term (AND). Each term gets its own escaped
	// ILIKE placeholder so a multi-word query requires all terms to be present
	// without demanding they be contiguous, exactly like SQLite FTS5.
	termClauses := make([]string, len(terms))
	for i, t := range terms {
		termClauses[i] = fmt.Sprintf(
			"m.content ILIKE '%%' || ?%d || '%%' ESCAPE E'\\\\'", argIdx-1)
		args = append(args, escapeLike(t))
		argIdx++
	}
	msgTermPredicate := strings.Join(termClauses, "\n\t\t\t\t\tAND ")

	msgProjectClause := ""
	nameProjectClause := ""
	if f.Project != "" {
		msgProjectClause = fmt.Sprintf("AND s.project = ?%d", argIdx-1)
		nameProjectClause = fmt.Sprintf("AND s.project = ?%d", argIdx-1)
		args = append(args, f.Project)
		argIdx++
	}

	query := fmt.Sprintf(`
		WITH msg_matches AS (
			SELECT DISTINCT ON (m.session_id)
				m.session_id,
				s.project,
				s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at) AS session_ended_at,
				m.ordinal,
				POSITION(LOWER(?1) IN LOWER(m.content)) AS match_pos,
				CASE
					WHEN POSITION(LOWER(?1) IN LOWER(m.content)) > 100
						THEN '...' || SUBSTRING(m.content
							FROM GREATEST(1, POSITION(
								LOWER(?1) IN LOWER(m.content)
							) - 50) FOR 200) || '...'
					ELSE SUBSTRING(m.content FROM 1 FOR 200)
						|| CASE WHEN LENGTH(m.content) > 200
							THEN '...' ELSE '' END
				END AS snippet
			FROM messages m
			JOIN sessions s ON m.session_id = s.id
			WHERE %s
				AND s.deleted_at IS NULL
				AND m.is_system = FALSE
				AND `+db.PostgresSystemPrefixSQL("m.content", "m.role")+`
				%s
			ORDER BY m.session_id,
				POSITION(LOWER(?1) IN LOWER(m.content)) ASC,
				m.ordinal ASC
		),
		name_matches AS (
			SELECT
				s.id AS session_id,
				s.project,
				s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at) AS session_ended_at,
				-1 AS ordinal,
				0 AS match_pos,
				CASE
					WHEN COALESCE(s.display_name, s.session_name) ILIKE '%%' || ?0 || '%%' ESCAPE E'\\'
						THEN COALESCE(s.display_name, s.session_name, '')
					WHEN s.first_message ILIKE '%%' || ?0 || '%%' ESCAPE E'\\'
						THEN COALESCE(s.first_message, '')
					ELSE COALESCE(s.display_name, s.session_name, s.first_message, '')
				END AS snippet
			FROM sessions s
			WHERE (COALESCE(s.display_name, s.session_name) ILIKE '%%' || ?0 || '%%' ESCAPE E'\\'
				OR s.first_message ILIKE '%%' || ?0 || '%%' ESCAPE E'\\')
				AND s.deleted_at IS NULL
				AND EXISTS (
					SELECT 1 FROM messages mx
					WHERE mx.session_id = s.id
					  AND mx.is_system = FALSE
					  AND `+db.PostgresSystemPrefixSQL("mx.content", "mx.role")+`
				)
				AND s.id NOT IN (SELECT session_id FROM msg_matches)
				%s
		)
		-- rank is a constant 1.0 because PostgreSQL ILIKE has no
	-- relevance scoring engine (unlike SQLite FTS5). Ordering
	-- uses match_pos and session_ended_at instead.
	SELECT session_id, ordinal,
			snippet, 1.0::double precision AS rank, match_pos
		FROM (
			SELECT *, 1 AS match_priority FROM msg_matches
			UNION ALL
			SELECT *, 2 AS match_priority FROM name_matches
		) combined
		ORDER BY %s
		LIMIT ?%d OFFSET ?%d`,
		msgTermPredicate,
		msgProjectClause,
		nameProjectClause,
		outerOrderBy,
		argIdx-1, argIdx,
	)
	args = append(args, f.Limit, f.Cursor)

	var rows []postgresSearchHitProjection
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil,
			fmt.Errorf("searching: %w", err)
	}
	hits := make([]db.SearchHit, len(rows))
	for i, row := range rows {
		hits[i] = db.SearchHit{
			SessionID: row.SessionID, Ordinal: row.Ordinal,
			Snippet: row.Snippet, Rank: row.Rank,
		}
	}
	return hits, nil
}
