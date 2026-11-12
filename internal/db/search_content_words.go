package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// contentWords is a natural-language query: the words a message can hold,
// counted per message, and the quoted phrases it must contain.
type contentWords struct {
	terms   []string // one FTS expression per word, covering its regular forms
	phrases []string // quoted phrases checked against the message's own words
	match   string   // FTS expression requiring word-bearing phrases
}

// contentWordsQuery prepares pattern for ranked word search. It reports false
// when the pattern has no word FTS can count, or when it needs the CJK index,
// whose segmentation the word count does not model.
func contentWordsQuery(pattern string) (contentWords, bool) {
	if containsCJK(pattern) {
		return contentWords{}, false
	}
	natural, err := literalFTSAnalyzer.PrepareNatural(pattern)
	var q contentWords
	if err != nil || len(natural.CountTerms) == 0 {
		return contentWords{}, false
	}
	if len(natural.Phrases) > 0 {
		q.match = natural.Match
		q.phrases = append(q.phrases, natural.Phrases...)
	}
	for _, term := range natural.CountTerms {
		q.terms = append(q.terms, term.Match)
	}
	return q, true
}

// snippetTerm is an FTS expression whose operands locate any query word.
func (q contentWords) snippetTerm() string { return strings.Join(q.terms, " OR ") }

// snippet windows body on a query word as spelled, or else on the FTS
// fragment, which holds a stemmed match such as "connecting" for "connection".
func (q contentWords) snippet(f ContentSearchFilter, body, fragment string) string {
	start, end := FTSSnippetRange(q.snippetTerm(), body)
	if start == end {
		start, end, _ = approxSnippetSpan(body, fragment)
	}
	return f.BuildSnippet(body, start, end)
}

// phraseMatch reports whether body holds the words of phrase adjacent and in
// order, ignoring case, with only punctuation or space between them.
func phraseMatch(body, phrase string) bool {
	isSeparator := func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }
	want := strings.FieldsFunc(phrase, isSeparator)
	have := strings.FieldsFunc(body, isSeparator)
	for i := 0; i+len(want) <= len(have); i++ {
		if slices.EqualFunc(have[i:i+len(want)], want, strings.EqualFold) {
			return true
		}
	}
	return false
}

// unquoteContentSearch returns the text inside a single fully quoted query.
func unquoteContentSearch(pattern string) (string, bool) {
	runes := []rune(strings.TrimSpace(pattern))
	if len(runes) < 2 {
		return pattern, false
	}
	inner := string(runes[1 : len(runes)-1])
	ascii := runes[0] == '"' && runes[len(runes)-1] == '"' && !strings.ContainsRune(inner, '"')
	curly := (runes[0] == '“' || runes[0] == '„') && (runes[len(runes)-1] == '”' || runes[len(runes)-1] == '“') && !strings.ContainsAny(inner, `“”„`)
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
// a query word and the number of words it holds.
func buildContentWordHits(ctx context.Context, tx *sql.Tx, q contentWords) error {
	terms, err := json.Marshal(q.terms)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE content_word_hits (
		id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("collecting word matches: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO content_word_hits
		SELECT f.rowid, count(*) FROM json_each(?) t
		CROSS JOIN messages_fts f WHERE f.content MATCH t.value GROUP BY f.rowid`, string(terms)); err != nil {
		return classifyFTSError(fmt.Errorf("collecting word matches: %w", err))
	}
	return nil
}

// contentWordRows reads need message matches from content_word_hits, after
// skipping offset. where filters the message m and its session s.
func contentWordRows(
	ctx context.Context, tx *sql.Tx, f ContentSearchFilter, q contentWords,
	where string, args []any, offset, need, floor int, full bool,
) ([]ContentMatch, error) {
	args = append([]any(nil), args...)
	var phraseWhere strings.Builder
	for _, phrase := range q.phrases {
		phraseWhere.WriteString(` AND agentsview_phrase_match(COALESCE(m.dialogue_text, m.content), ?)`)
		args = append(args, phrase)
	}
	where += phraseWhere.String()
	from := "messages_fts CROSS JOIN content_word_hits h ON h.id = messages_fts.rowid"
	count := "h.n"
	sortKey := "julianday(COALESCE(s.ended_at, s.started_at, ''))"
	order := "sort_key DESC, m.session_id ASC, m.ordinal ASC, m.id ASC"
	if full {
		from = "messages_fts"
		count = fmt.Sprint(len(q.terms))
		where += " AND messages_fts.content MATCH ?"
		match := strings.Join(q.terms, " AND ")
		if q.match != "" {
			match = "(" + q.match + ") AND " + match
		}
		args = append(args, match)
	} else {
		where += " AND h.n >= ? AND messages_fts.content MATCH ?"
		match := q.snippetTerm()
		if q.match != "" {
			match = q.match
		}
		args = append(args, floor, match)
	}
	if f.Mode == "fts" || f.Mode == "hybrid" {
		sortKey = "messages_fts.rank"
		order = "sort_key ASC, m.id ASC"
		if f.Mode == "fts" {
			order = "sort_key ASC, m.ordinal ASC, m.id ASC"
		}
	}
	id := "h.id"
	if full {
		id = "messages_fts.rowid"
	}
	query := `SELECT m.session_id, s.project, s.agent, s.transcript_revision, 'message', m.role, '',
			m.ordinal, COALESCE(m.timestamp,''), COALESCE(m.dialogue_text, m.content), snippet(messages_fts, 0, '', '', '...', 32), p.n
		FROM (SELECT m.id, ` + count + ` AS n, ` + sortKey + ` AS sort_key
			FROM ` + from + ` CROSS JOIN messages m ON m.id = ` + id + ` CROSS JOIN sessions s ON s.id = m.session_id
			WHERE ` + where + ` ORDER BY n DESC, ` + order + ` LIMIT ? OFFSET ?) p
		CROSS JOIN messages m ON m.id = p.id CROSS JOIN sessions s ON s.id = m.session_id CROSS JOIN messages_fts ON messages_fts.rowid = p.id AND messages_fts.content MATCH ?
		ORDER BY p.n DESC, ` + order
	args = append(args, need, offset, q.snippetTerm())
	return scanContentRows(ctx, tx, query, args, func(body, fragment string) string {
		return q.snippet(f, body, fragment)
	})
}

// selectContentWordPage avoids collecting partial hits when all-word matches
// already fill the requested page. Counts are unfiltered, so a short filtered
// page retries with all tiers and the original offset.
func selectContentWordPage(ctx context.Context, tx *sql.Tx, f ContentSearchFilter, q contentWords, where string, args []any, offset, need int) ([]ContentMatch, error) {
	full, err := contentWordRows(ctx, tx, f, q, where, args, offset, need, 0, true)
	if err != nil || len(full) == need {
		return full, err
	}
	if err := buildContentWordHits(ctx, tx, q); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT n, count(*) FROM content_word_hits GROUP BY n ORDER BY n DESC`)
	if err != nil {
		return nil, err
	}
	floor, total := 1, 0
	for rows.Next() {
		var n, count int
		if err := rows.Scan(&n, &count); err != nil {
			rows.Close()
			return nil, err
		}
		total += count
		if total >= offset+need {
			floor = n
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	page, err := contentWordRows(ctx, tx, f, q, where, args, offset, need, floor, false)
	if err == nil && len(page) < need && floor > 1 {
		return contentWordRows(ctx, tx, f, q, where, args, offset, need, 1, false)
	}
	return page, err
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
		words, err := selectContentWordPage(ctx, snapshot.tx, f, q, where, args,
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
