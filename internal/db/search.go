package db

import (
	"context"
	"fmt"
	"strings"
)

const (
	DefaultSearchLimit = 50
	MaxSearchLimit     = 500
	snippetTokenLength = 32
)

// SearchResult holds a message match with session context.
type SearchResult struct {
	SessionID string  `json:"session_id"`
	Project   string  `json:"project"`
	Ordinal   int     `json:"ordinal"`
	Role      string  `json:"role"`
	Timestamp string  `json:"timestamp"`
	Snippet   string  `json:"snippet"`
	Rank      float64 `json:"rank"`
}

// SearchFilter specifies search parameters.
type SearchFilter struct {
	Query   string
	Project string
	Cursor  int // offset for pagination
	Limit   int
}

// SearchPage holds paginated search results.
type SearchPage struct {
	Results    []SearchResult `json:"results"`
	NextCursor int            `json:"next_cursor,omitempty"`
}

// Search performs FTS5 full-text search across messages.
func (db *DB) Search(
	ctx context.Context, f SearchFilter,
) (SearchPage, error) {
	if f.Limit <= 0 || f.Limit > MaxSearchLimit {
		f.Limit = DefaultSearchLimit
	}

	whereClauses := []string{
		"messages_fts MATCH ?",
		"s.deleted_at IS NULL",
		"m.is_system = 0",
	}
	args := []any{f.Query}

	if f.Project != "" {
		whereClauses = append(whereClauses, "s.project = ?")
		args = append(args, f.Project)
	}

	query := fmt.Sprintf(`
		SELECT m.session_id, s.project, m.ordinal, m.role,
			m.timestamp,
			snippet(messages_fts, 0, '<mark>', '</mark>',
				'...', %d) as snippet,
			rank
		FROM messages_fts
		JOIN messages m ON messages_fts.rowid = m.id
		JOIN sessions s ON m.session_id = s.id
		WHERE %s
		ORDER BY rank
		LIMIT ? OFFSET ?`,
		snippetTokenLength,
		strings.Join(whereClauses, " AND "),
	)
	args = append(args, f.Limit+1, f.Cursor)

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return SearchPage{}, fmt.Errorf("searching: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(
			&r.SessionID, &r.Project, &r.Ordinal, &r.Role,
			&r.Timestamp, &r.Snippet, &r.Rank,
		); err != nil {
			return SearchPage{},
				fmt.Errorf("scanning result: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return SearchPage{}, err
	}

	page := SearchPage{Results: results}
	if len(results) > f.Limit {
		page.Results = results[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

// SearchSession performs a case-insensitive substring search within a single
// session's messages, returning matching ordinals in document order.
// This is used by the in-session find bar (analogous to browser Cmd+F).
// Both message content and tool-call result_content are searched so that
// matches inside tool output blocks are reachable. Only fields that the
// frontend renders and highlights are included to avoid phantom matches.
func (db *DB) SearchSession(
	ctx context.Context, sessionID, query string,
) ([]int, error) {
	if query == "" {
		return nil, nil
	}
	// Use LIKE for substring semantics consistent with browser find-bar UX.
	// SQLite LIKE is case-insensitive for ASCII by default.
	// LEFT JOIN tool_calls so that a hit in result_content also surfaces
	// the parent message ordinal; DISTINCT collapses multiple tool calls
	// on the same message into a single result.
	like := "%" + escapeLike(query) + "%"
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT DISTINCT m.ordinal
		 FROM messages m
		 LEFT JOIN tool_calls tc ON tc.message_id = m.id
		 WHERE m.session_id = ?
		   AND m.is_system = 0
		   AND (m.content LIKE ? ESCAPE '\'
		        OR tc.result_content LIKE ? ESCAPE '\')
		 ORDER BY m.ordinal ASC`,
		sessionID, like, like,
	)
	if err != nil {
		return nil, fmt.Errorf("session search: %w", err)
	}
	defer rows.Close()

	var ordinals []int
	for rows.Next() {
		var ord int
		if err := rows.Scan(&ord); err != nil {
			return nil, fmt.Errorf("scanning ordinal: %w", err)
		}
		ordinals = append(ordinals, ord)
	}
	return ordinals, rows.Err()
}
