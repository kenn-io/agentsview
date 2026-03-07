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

	whereClauses := []string{"messages_fts MATCH ?", "s.deleted_at IS NULL"}
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
