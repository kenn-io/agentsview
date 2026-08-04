package postgres

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
)

const (
	pgSnippetRadius = 60 // chars of context on each side of a match
)

// SearchContent implements content search for the PostgreSQL read-only store.
// The "fts" mode falls back to ILIKE over message content, with one predicate
// per prepared FTS term to preserve SQLite's implicit-AND semantics.
func (s *Store) SearchContent(
	ctx context.Context, f db.ContentSearchFilter,
) (db.ContentSearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxContentSearchLimit {
		f.Limit = db.DefaultContentSearchLimit
	}
	if f.Pattern == "" {
		return db.ContentSearchPage{}, nil
	}

	// Semantic and hybrid validate Sources themselves (messages only) ahead
	// of the substring/regex/fts source-set default just below, which fills
	// in tool_input/tool_result that neither mode supports -- mirroring
	// internal/db's SearchContent so an empty Sources field is not defaulted
	// out from under ValidateSemanticFilter's empty-or-messages-only check.
	if f.Mode == "semantic" || f.Mode == "hybrid" {
		// Validate input the same way SQLite's semantic/hybrid paths do
		// before reporting the capability gate: an invalid request (bad
		// cursor, non-messages source) must return the same 400
		// SearchInputError on every backend rather than a 501 here and a
		// 400 on SQLite (backend parity, see AGENTS.md).
		if err := db.ValidateSemanticFilter(f); err != nil {
			return db.ContentSearchPage{}, err
		}
		if s.getVectorSearcher() == nil {
			return db.ContentSearchPage{}, s.semanticUnavailableError()
		}
		if f.Mode == "semantic" {
			return s.searchContentSemanticPG(ctx, f)
		}
		return s.searchContentHybridPG(ctx, f)
	}
	return s.BunStore.SearchContent(ctx, f)
}

// pgSessionFilter builds a db.SessionFilter from a ContentSearchFilter.
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

// pgMessagesBranch builds the messages source branch SQL.
// Placeholders continue from pb's current position.
func pgMessagesBranch(
	f db.ContentSearchFilter, escapedPat string, pb *paramBuilder,
) string {
	contentPred := pgContentSearchPredicate(
		"m.content", f, escapedPat, pb,
	)

	sysPred := "TRUE"
	if f.ExcludeSystem {
		sysPred = "m.is_system = FALSE AND " +
			db.PostgresSystemPrefixSQL("m.content", "m.role")
	}

	// Select the full content; the snippet is windowed and redacted in Go.
	return fmt.Sprintf(`
		SELECT m.session_id, s.project, s.agent, 'message' AS location,
			m.role AS role, '' AS tool_name, m.ordinal,
			m.timestamp AS ts,
			m.content AS snippet, 0 AS src, 0::bigint AS row_id,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		JOIN scoped sc ON sc.id = m.session_id
		WHERE %s
		  AND %s`,
		contentPred, sysPred)
}

func pgContentSearchPredicate(
	column string, f db.ContentSearchFilter, escapedPat string,
	pb *paramBuilder,
) string {
	if f.Mode != "fts" {
		ilikePat := "%" + escapedPat + "%"
		ilikeParam := pb.add(ilikePat)
		return fmt.Sprintf(
			"%s ILIKE '%%'||%s||'%%' ESCAPE E'\\\\'",
			column, ilikeParam,
		)
	}

	terms := db.FTSTerms(db.PrepareFTSQuery(f.Pattern))
	clauses := make([]string, 0, len(terms))
	for _, term := range terms {
		if term == "" {
			continue
		}
		termParam := pb.add(escapeLike(term))
		clauses = append(clauses, fmt.Sprintf(
			"%s ILIKE '%%'||%s||'%%' ESCAPE E'\\\\'",
			column, termParam,
		))
	}
	if len(clauses) == 0 {
		return "FALSE"
	}
	return strings.Join(clauses, " AND ")
}

// pgSnippetBounds returns the rune-snapped byte window around [start,end),
// mirroring snippetBounds in internal/db/search_content.go.
func pgSnippetBounds(text string, start, end int) (int, int) {
	lo := max(start-pgSnippetRadius, 0)
	hi := min(end+pgSnippetRadius, len(text))
	for lo < start && !utf8.RuneStart(text[lo]) {
		lo++
	}
	for hi > end && hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi--
	}
	return lo, hi
}

// pgBuildSnippet windows body around [start,end) and, unless reveal is set,
// masks secrets overlapping the window (including straddling ones) via
// secrets.RedactWindow, so a pre-truncated snippet can never leak a fragment.
func pgBuildSnippet(f db.ContentSearchFilter, body string, start, end int) string {
	lo, hi := pgSnippetBounds(body, start, end)
	if f.RevealSecrets {
		return body[lo:hi]
	}
	return secrets.RedactWindow(body, lo, hi)
}

// pgSubstringSnippet builds a substring-match snippet: it locates the
// case-insensitive pattern (the ILIKE already matched, so it is present; fall
// back to the start) and windows it. It uses db.CaseInsensitiveIndex so the
// offset indexes body directly even when lowercasing would change byte length.
func pgSubstringSnippet(f db.ContentSearchFilter, body string) string {
	if f.Mode == "fts" {
		start, end := db.FTSSnippetRange(f.Pattern, body)
		return pgBuildSnippet(f, body, start, end)
	}
	off := max(db.CaseInsensitiveIndex(body, f.Pattern), 0)
	return pgBuildSnippet(f, body, off, min(off+len(f.Pattern), len(body)))
}
