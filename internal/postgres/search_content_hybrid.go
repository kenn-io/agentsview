package postgres

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/uptrace/bun"

	"go.kenn.io/agentsview/internal/db"
)

func pgSessionFilter(f db.ContentSearchFilter) db.SessionFilter {
	return db.SessionFilter{
		Project: f.Project, ExcludeProject: f.ExcludeProject,
		Machine: f.Machine, GitBranch: f.GitBranch, Agent: f.Agent,
		Date: f.Date, DateFrom: f.DateFrom, DateTo: f.DateTo,
		Timezone:         f.Timezone,
		ActiveSince:      f.ActiveSince,
		ExcludeOneShot:   !f.IncludeOneShot,
		ExcludeAutomated: !f.IncludeAutomated,
		IncludeChildren:  f.IncludeChildren,
	}
}

func pgSnippetBounds(text string, start, end int) (int, int) {
	const radius = 60
	lo := max(start-radius, 0)
	hi := min(end+radius, len(text))
	for lo < start && !utf8.RuneStart(text[lo]) {
		lo++
	}
	for hi > end && hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi--
	}
	return lo, hi
}

func semanticPGSessionFilter(f db.ContentSearchFilter) db.SessionFilter {
	sf := pgSessionFilter(f)
	sf.ChildExemptOneShot = true
	return sf
}

func (postgresFullTextCapability) SearchHybridContent(
	ctx context.Context, store bun.IDB, f db.ContentSearchFilter,
) ([]db.ContentSearchHit, error) {
	terms := db.FTSTerms(db.PrepareFTSQuery(f.Pattern))
	if len(terms) == 0 {
		return nil, nil
	}
	scopeWhere, scopeArgs := db.BunSessionBaseFilter(semanticPGSessionFilter(f))
	predicates := make([]string, len(terms))
	args := make([]any, 0, len(terms)+len(scopeArgs)+2)
	for i, term := range terms {
		predicates[i] = `m.content ILIKE ? ESCAPE '\'`
		args = append(args, "%"+db.EscapeLikePattern(term)+"%")
	}
	args = append(args, scopeArgs...)
	args = append(args, f.Limit, f.Cursor)
	query := fmt.Sprintf(`
		SELECT m.session_id, m.ordinal, m.content
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
		  AND m.role IN ('user', 'assistant') AND m.is_system = FALSE
		  AND %s
		  AND m.session_id IN (SELECT id FROM sessions AS session WHERE %s)
		ORDER BY COALESCE(s.ended_at, s.started_at, s.created_at) DESC NULLS LAST,
		         m.session_id ASC, m.ordinal ASC
		LIMIT ? OFFSET ?`,
		strings.Join(predicates, " AND "),
		db.PostgresSystemPrefixSQL("m.content", "m.role"), scopeWhere)
	var rows []struct {
		SessionID string `bun:"session_id"`
		Ordinal   int    `bun:"ordinal"`
		Content   string `bun:"content"`
	}
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("querying PostgreSQL hybrid keyword candidates: %w", err)
	}
	hits := make([]db.ContentSearchHit, len(rows))
	for i, row := range rows {
		hits[i] = db.ContentSearchHit{
			SessionID: row.SessionID, Ordinal: row.Ordinal,
			Location: "message", Snippet: pgKeywordApproxSnippet(row.Content, f.Pattern),
		}
	}
	return hits, nil
}

func pgKeywordApproxSnippet(content, pattern string) string {
	start, end := db.FTSSnippetRange(pattern, content)
	lo, hi := pgSnippetBounds(content, start, end)
	return content[lo:hi]
}
