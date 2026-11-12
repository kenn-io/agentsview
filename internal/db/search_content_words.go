package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"unicode"
)

// contentWords is a natural-language query: the words a message can hold,
// counted per message, and the quoted phrases it must contain as typed.
type contentWords struct {
	terms   []string // one FTS expression per word, covering its regular forms
	phrases []string
}

// contentWordsQuery prepares pattern for ranked word search. It reports false
// when the pattern has no word FTS can count, or when it needs the CJK index,
// whose segmentation the word count does not model.
func contentWordsQuery(pattern string) (contentWords, bool) {
	if containsCJK(pattern) {
		return contentWords{}, false
	}
	segments := strings.Split(strings.NewReplacer("“", `"`, "”", `"`, "„", `"`).Replace(pattern), `"`)
	var q contentWords
	analyzed := make([]string, 0, len(segments))
	for i, segment := range segments {
		if i%2 == 1 && i < len(segments)-1 {
			q.phrases = append(q.phrases, segment)
			// FTS has no token for a punctuation-only phrase, so only the
			// literal check below requires it.
			if !strings.ContainsFunc(segment, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
				continue
			}
			segment = `"` + segment + `"`
		}
		analyzed = append(analyzed, segment)
	}
	natural, err := literalFTSAnalyzer.PrepareNatural(strings.Join(analyzed, " "))
	if err != nil || len(natural.CountTerms) == 0 {
		return contentWords{}, false
	}
	for _, term := range natural.CountTerms {
		q.terms = append(q.terms, term.Match)
	}
	return q, true
}

// snippetTerm is an FTS expression whose operands locate any query word.
func (q contentWords) snippetTerm() string { return strings.Join(q.terms, " OR ") }

// unquoteContentSearch returns the text inside a single fully quoted query.
func unquoteContentSearch(pattern string) (string, bool) {
	runes := []rune(strings.TrimSpace(pattern))
	if len(runes) < 2 {
		return pattern, false
	}
	inner := string(runes[1 : len(runes)-1])
	ascii := runes[0] == '"' && runes[len(runes)-1] == '"' && !strings.ContainsRune(inner, '"')
	curly := (runes[0] == '“' || runes[0] == '„') && (runes[len(runes)-1] == '”' || runes[len(runes)-1] == '“') &&
		!strings.ContainsAny(inner, `“”„`)
	if (ascii || curly) && strings.TrimSpace(inner) != "" {
		return inner, true
	}
	return pattern, false
}

// explicitFTSExpression reports whether an --fts pattern is FTS5 syntax: a
// leading quote with only quoted phrases, or with operators outside them. A
// quoted phrase mixed with ordinary words, and a phrase with no word in it,
// stay natural text.
func explicitFTSExpression(pattern string) bool {
	trimmed := strings.TrimSpace(pattern)
	if !strings.HasPrefix(trimmed, `"`) {
		return false
	}
	parts := strings.Split(trimmed, `"`)
	if len(parts)%2 == 0 {
		return true // unbalanced; FTS5 reports the error
	}
	var outside, inside strings.Builder
	for i, part := range parts {
		if i%2 == 0 {
			outside.WriteString(part + " ")
		} else {
			inside.WriteString(part)
		}
	}
	if !strings.ContainsFunc(inside.String(), func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
		return false
	}
	words := strings.Fields(outside.String())
	if len(words) == 0 || strings.ContainsAny(outside.String(), "*+():^") {
		return true
	}
	for _, word := range words {
		if word == "AND" || word == "OR" || word == "NOT" || word == "NEAR" {
			return true
		}
	}
	return false
}

// buildContentWordHits fills a temporary table with every message that holds
// a query word and the number of words it holds. The key orders hits by that
// count, then newest message first, so a page reads only the hits before it
// rather than sorting them all.
func buildContentWordHits(ctx context.Context, tx *sql.Tx, q contentWords) error {
	terms, err := json.Marshal(q.terms)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE content_word_hits (
		n INTEGER NOT NULL, id INTEGER NOT NULL, PRIMARY KEY (n DESC, id DESC)) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("collecting word matches: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO content_word_hits
		SELECT count(*), f.rowid FROM json_each(?) t
		CROSS JOIN messages_fts f WHERE f.content MATCH t.value GROUP BY f.rowid`, string(terms)); err != nil {
		return classifyFTSError(fmt.Errorf("collecting word matches: %w", err))
	}
	return nil
}

// contentWordRows reads need message matches from content_word_hits, after
// skipping offset. where filters the message m and its session s; each
// quoted phrase must also appear as typed.
func contentWordRows(
	ctx context.Context, tx *sql.Tx, f ContentSearchFilter, q contentWords,
	where string, args []any, offset, need int,
) ([]ContentMatch, error) {
	args = append([]any(nil), args...)
	var whereSb135 strings.Builder
	for _, phrase := range q.phrases {
		whereSb135.WriteString(` AND COALESCE(m.dialogue_text, m.content) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(phrase)+"%")
	}
	where += whereSb135.String()
	query := `SELECT m.session_id, s.project, s.agent, s.transcript_revision, 'message', m.role, '',
			m.ordinal, COALESCE(m.timestamp,''), COALESCE(m.dialogue_text, m.content), '', h.n
		FROM content_word_hits h CROSS JOIN messages m ON m.id = h.id CROSS JOIN sessions s ON s.id = m.session_id
		WHERE ` + where + ` ORDER BY h.n DESC, h.id DESC LIMIT ? OFFSET ?`
	args = append(args, need, offset)
	return scanContentRows(ctx, tx, query, args, func(body, _ string) string {
		start, end := FTSSnippetRange(q.snippetTerm(), body)
		return f.BuildSnippet(body, start, end)
	})
}

// lexicalMessageWhere is the system, scope, and anchor filter the substring
// search applies to messages.
func lexicalMessageWhere(f ContentSearchFilter) (string, []any) {
	scope, args := sessionScopeSubquery(f)
	where := "m." + scope + " AND " + LexicalAnchorScopeSQL(f, "s", "m", SQLiteQueryDialect())
	if f.ExcludeSystem {
		where += " AND m.is_system = 0 AND " + SystemPrefixSQL("m.content", "m.role")
	}
	return where, args
}

// searchContentWords lists messages by how many query words they hold. With
// exact set, matches holding the whole pattern come first, from every
// selected source, in substring order.
func (db *DB) searchContentWords(
	ctx context.Context, f ContentSearchFilter, q contentWords, exact bool,
) (ContentSearchPage, error) {
	// Only the exact scan reads bodies across the archive; word matching
	// reads the index, as FTS search always has.
	if exact && f.SessionID == "" {
		release, err := db.acquireBulkRead(ctx)
		if err != nil {
			return ContentSearchPage{}, err
		}
		defer release()
	}
	search := func(candidates substringCandidateFilters) ([]ContentMatch, error) {
		// One snapshot keeps exact and word matches consistent with the
		// exact total that offsets the word matches.
		snapshot, err := db.BeginMessageSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = snapshot.Close() }()
		var matches []ContentMatch
		total := 0
		where, args := lexicalMessageWhere(f)
		if exact {
			if matches, total, err = exactContentRows(ctx, snapshot.tx, f, candidates, len(q.terms)); err != nil {
				return nil, err
			}
			where += ` AND COALESCE(m.dialogue_text, m.content) NOT LIKE ? ESCAPE '\'`
			args = append(args, "%"+escapeLike(f.Pattern)+"%")
		}
		if len(matches) > f.Limit {
			return matches, nil
		}
		if err := buildContentWordHits(ctx, snapshot.tx, q); err != nil {
			return nil, err
		}
		words, err := contentWordRows(ctx, snapshot.tx, f, q, where, args,
			max(0, f.Cursor-total), f.Limit+1-len(matches))
		return append(matches, words...), err
	}
	var matches []ContentMatch
	var err error
	if exact {
		matches, err = searchWithSubstringCandidates(ctx, db, f, f.Pattern, search)
	} else {
		matches, err = search(substringCandidateFilters{})
	}
	if err != nil {
		return ContentSearchPage{}, err
	}
	return db.contentMatchPage(ctx, matches, f.Limit, f.Cursor)
}

// exactContentRows reads up to Limit+1 substring matches after Cursor and the
// total number of substring matches, from one scan. The statement also
// returns the last match when the page holds none, so the total survives an
// offset past every match.
func exactContentRows(
	ctx context.Context, tx *sql.Tx, f ContentSearchFilter,
	candidates substringCandidateFilters, words int,
) ([]ContentMatch, int, error) {
	union, args := substringUnion(f, candidates)
	if union == "" {
		return nil, 0, nil
	}
	query := `SELECT session_id, project, agent, transcript_revision, location, role, tool_name,
			ordinal, ts, snippet, '', ?, total FROM (
			SELECT u.*, row_number() OVER (ORDER BY ` + substringOrder + `) AS rn, count(*) OVER () AS total
			FROM (` + union + `) u)
		WHERE rn > ? AND rn <= ? OR rn = total ORDER BY rn`
	args = append([]any{words}, args...)
	args = append(args, f.Cursor, f.Cursor+f.Limit+1)
	total := 0
	rows, err := scanContentRows(ctx, tx, query, args, func(body, _ string) string {
		return f.substringSnippet(body)
	}, &total)
	if err != nil {
		return nil, 0, err
	}
	if len(rows) > 0 && (total <= f.Cursor || total > f.Cursor+f.Limit+1) {
		rows = rows[:len(rows)-1]
	}
	return rows, total, nil
}
