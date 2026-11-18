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
	terms   []string   // one FTS expression per word, covering its regular forms
	phrases []string   // quoted phrases checked against the message's own words
	match   string     // FTS expression requiring word-bearing phrases
	forms   [][]string // each word's regular forms, for the other backends
}

// ContentWords is a natural query for backends without FTS5: each word's
// regular forms and the quoted phrases a match must hold.
type ContentWords struct {
	Terms   [][][]string
	Phrases []string
}

// ContentWordsMatch reports whether body contains every word and phrase.
func ContentWordsMatch(body string, words ContentWords) bool {
	for _, term := range words.Terms {
		if !wordFormsMatch(body, term) {
			return false
		}
	}
	return !slices.ContainsFunc(words.Phrases, func(phrase string) bool { return !phraseMatch(body, phrase) })
}

func wordFormsMatch(body string, term [][]string) bool {
	have := strings.FieldsFunc(body, isSeparator)
	for i := 0; i+len(term) <= len(have); i++ {
		matched := true
		for j, forms := range term {
			if !slices.ContainsFunc(forms, func(form string) bool { return strings.EqualFold(have[i+j], form) }) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// WordSnippetRange finds the exact pattern or the first whole word matching a
// query form, falling back to the start of body.
func WordSnippetRange(body, pattern string, words ContentWords) (int, int) {
	if start, end, ok := CaseInsensitiveSpan(body, pattern); ok {
		return start, end
	}
	offset := 0
	for word := range strings.FieldsFuncSeq(body, isSeparator) {
		offset += strings.Index(body[offset:], word)
		for _, term := range words.Terms {
			// A form spanning punctuation centers on its first word, as
			// ftsTermSpan does for phrases matched across punctuation.
			if slices.ContainsFunc(term[0], func(form string) bool {
				parts := strings.FieldsFunc(form, isSeparator)
				return len(parts) > 0 && strings.EqualFold(word, parts[0])
			}) {
				return offset, offset + len(word)
			}
		}
		offset += len(word)
	}
	return 0, 0
}

// RouteContentWords applies SQLite's gates for word matching to f, a
// substring or fts search. It returns f with a fully quoted pattern unquoted.
func RouteContentWords(f ContentSearchFilter) (ContentSearchFilter, ContentWords, bool) {
	if f.Mode == "fts" && (hasFTSPrefix(f.Pattern) || strings.HasPrefix(strings.TrimSpace(f.Pattern), `NOT "`)) {
		return f, ContentWords{}, false
	}
	f, q, ok := routeContentWords(f)
	return f, ContentWords{Terms: mirrorWordForms(q.forms), Phrases: q.phrases}, ok
}

func mirrorWordForms(terms [][]string) [][][]string {
	result := make([][][]string, 0, len(terms))
	for _, forms := range terms {
		parts := strings.FieldsFunc(forms[0], isSeparator)
		if len(parts) < 2 {
			result = append(result, [][]string{forms})
			continue
		}
		var term [][]string
		for _, part := range parts {
			natural, err := literalFTSAnalyzer.PrepareNatural(part)
			if err != nil || len(natural.CountTerms) == 0 {
				term = append(term, []string{part})
			} else {
				term = append(term, natural.CountTerms[0].Forms)
			}
		}
		result = append(result, term)
	}
	return result
}

func hasFTSPrefix(pattern string) bool {
	quoted := false
	for _, r := range pattern {
		if r == '"' {
			quoted = !quoted
		} else if r == '*' && !quoted {
			return true
		}
	}
	return false
}

func routeContentWords(f ContentSearchFilter) (ContentSearchFilter, contentWords, bool) {
	words := f.Mode == "fts" && !explicitFTSExpression(f.Pattern)
	if f.Mode != "fts" && hasSource(f, "messages") {
		var quoted bool
		f.Pattern, quoted = unquoteContentSearch(f.Pattern)
		words = !quoted && len(strings.Fields(f.Pattern)) > 1
	}
	if !words {
		return f, contentWords{}, false
	}
	q, ok := contentWordsQuery(f.Pattern)
	return f, q, ok
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
		q.forms = append(q.forms, term.Forms)
	}
	return q, true
}

// snippetTerm is an FTS expression whose operands locate any query word.
func (q contentWords) snippetTerm() string { return strings.Join(q.terms, " OR ") }

// phraseMatch reports whether body holds the words of phrase adjacent and in
// order, ignoring case, with only punctuation or space between them.
func phraseMatch(body, phrase string) bool {
	want := strings.FieldsFunc(phrase, isSeparator)
	have := strings.FieldsFunc(body, isSeparator)
	for i := 0; i+len(want) <= len(have); i++ {
		if slices.EqualFunc(have[i:i+len(want)], want, strings.EqualFold) {
			return true
		}
	}
	return false
}

func isSeparator(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }

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

// explicitFTSExpression reports whether a leading-quote pattern uses FTS5 syntax.
func explicitFTSExpression(pattern string) bool {
	if !strings.HasPrefix(strings.TrimSpace(pattern), `"`) {
		return false
	}
	tokens, err := ftsTokens(pattern)
	if err != nil {
		return true // FTS5 reports malformed expressions
	}
	natural := false
	for _, token := range tokens {
		if token.kind != "term" {
			return true
		}
		if strings.HasPrefix(token.raw, `"`) {
			if strings.HasSuffix(token.raw, "*") {
				return true
			}
		} else if token.text == "NEAR" || strings.ContainsAny(token.raw, "*+:^") {
			return true
		} else {
			natural = true
		}
	}
	return !natural
}

// buildContentWordHits fills a temporary table with every message that holds
// a query word and the number of words it holds. A query with quoted phrases
// keeps only the messages FTS finds holding every phrase.
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
		return fmt.Errorf("collecting word matches: %w", err)
	}
	if q.match == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM content_word_hits WHERE id NOT IN (
		SELECT rowid FROM messages_fts WHERE messages_fts.content MATCH ?)`, q.match); err != nil {
		return fmt.Errorf("collecting phrase matches: %w", err)
	}
	return nil
}

// contentWordFilter limits word ranking. scope is a predicate on m.session_id
// alone; where filters the message m and its session s, and may read message
// content.
type contentWordFilter struct {
	scope, where    string
	scopeArgs, args []any
}

// selectContentWordPage reads need messages after offset, ordered by how many
// query words they hold. It applies the session scope to the word matches
// first, then the rest of the filter one word count at a time, stopping once
// the page can fill. SQLite runs the scope's subquery after a row's other
// checks, so one combined filter would read the content of every match in
// the archive.
func selectContentWordPage(ctx context.Context, tx *sql.Tx, f ContentSearchFilter, q contentWords, filter contentWordFilter, offset, need int) ([]ContentMatch, error) {
	// Untyped columns keep each rank and sort key exactly as computed.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE content_word_scope (
		n INTEGER NOT NULL, id INTEGER NOT NULL, rank, PRIMARY KEY (n, id)) WITHOUT ROWID`); err != nil {
		return nil, fmt.Errorf("scoping word matches: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE content_word_rows (
		id INTEGER PRIMARY KEY, n, sort_key, session_id, ordinal)`); err != nil {
		return nil, fmt.Errorf("filtering word matches: %w", err)
	}
	ranked := f.Mode == "fts" || f.Mode == "hybrid"
	// FTS finds the messages holding every word directly, so a page they
	// fill needs no per-word counts. Phrase and word expressions rank
	// differently, so only a query without phrases takes its rank here.
	all := len(q.terms)
	match := strings.Join(q.terms, " AND ")
	rank := "NULL"
	if q.match != "" {
		match = "(" + q.match + ") AND " + match
	} else if ranked {
		rank = "f.rank"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO content_word_scope
		SELECT ?, f.rowid, `+rank+` FROM messages_fts f CROSS JOIN messages m ON m.id = f.rowid
		WHERE f.content MATCH ? AND m.`+filter.scope, append([]any{all, match}, filter.scopeArgs...)...); err != nil {
		return nil, fmt.Errorf("scoping word matches: %w", err)
	}
	where := filter.where
	args := append([]any(nil), filter.args...)
	var whereSb291 strings.Builder
	for _, phrase := range q.phrases {
		whereSb291.WriteString(` AND agentsview_phrase_match(COALESCE(m.dialogue_text, m.content), ?)`)
		args = append(args, phrase)
	}
	where += whereSb291.String()
	sortKey := "julianday(COALESCE(s.ended_at, s.started_at, ''))"
	if ranked {
		sortKey = "c.rank"
	}
	// Every match holding more words than the page's last row comes before
	// it, so the counts below that row's need no filtering.
	floor, total := all, 0
	for {
		if floor == all-1 {
			if err := buildContentWordHits(ctx, tx, q); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO content_word_scope
				SELECT h.n, h.id, NULL FROM content_word_hits h CROSS JOIN messages m ON m.id = h.id
				WHERE h.n < ? AND m.`+filter.scope, append([]any{all}, filter.scopeArgs...)...); err != nil {
				return nil, fmt.Errorf("scoping word matches: %w", err)
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO content_word_rows
			SELECT m.id, c.n, `+sortKey+`, m.session_id, m.ordinal
			FROM content_word_scope c CROSS JOIN messages m ON m.id = c.id CROSS JOIN sessions s ON s.id = m.session_id
			WHERE c.n = ? AND `+where, append([]any{floor}, args...)...)
		if err != nil {
			return nil, fmt.Errorf("filtering word matches: %w", err)
		}
		added, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if total += int(added); floor == 1 || total >= offset+need {
			break
		}
		floor--
	}
	order := []string{"n DESC", "sort_key DESC", "session_id ASC", "ordinal ASC", "id ASC"}
	switch f.Mode {
	case "fts":
		order = []string{"n DESC", "sort_key ASC", "ordinal ASC", "id ASC"}
	case "hybrid":
		order = []string{"n DESC", "sort_key ASC", "id ASC"}
	}
	if !ranked || (q.match == "" && floor == all) {
		page := `SELECT id, n, sort_key, session_id, ordinal FROM content_word_rows
			ORDER BY ` + strings.Join(order, ", ") + ` LIMIT ? OFFSET ?`
		return contentWordPageRows(ctx, tx, f, q, page, order, []any{need, offset})
	}
	if total == 0 {
		return nil, nil
	}
	// The any-word expression ranks a message holding every word as the
	// all-words expression does: bm25 sums the same phrases. FTS scores only
	// the filtered matches, within the range of their ids.
	match = q.snippetTerm()
	if q.match != "" {
		match = q.match
	}
	var low, high int64
	if err := tx.QueryRowContext(ctx, `SELECT min(id), max(id) FROM content_word_rows`).Scan(&low, &high); err != nil {
		return nil, err
	}
	page := `SELECT * FROM (SELECT r.id, r.n, f.rank AS sort_key, r.session_id, r.ordinal
		FROM messages_fts f CROSS JOIN content_word_rows r ON r.id = f.rowid
		WHERE f.content MATCH ? AND f.rowid BETWEEN ? AND ?)
		ORDER BY ` + strings.Join(order, ", ") + ` LIMIT ? OFFSET ?`
	return contentWordPageRows(ctx, tx, f, q, page, order, []any{match, low, high, need, offset})
}

// contentWordPageRows reads the messages page lists, with their snippets.
// page selects id, n, sort_key, session_id, and ordinal, in order.
func contentWordPageRows(
	ctx context.Context, tx *sql.Tx, f ContentSearchFilter, q contentWords,
	page string, order []string, args []any,
) ([]ContentMatch, error) {
	query := `SELECT m.session_id, s.project, s.agent, s.transcript_revision, 'message', m.role, '',
			m.ordinal, COALESCE(m.timestamp,''), COALESCE(m.dialogue_text, m.content), snippet(messages_fts, 0, '', '', '...', 32), p.n
		FROM (` + page + `) p
		CROSS JOIN messages m ON m.id = p.id CROSS JOIN sessions s ON s.id = m.session_id CROSS JOIN messages_fts ON messages_fts.rowid = p.id AND messages_fts.content MATCH ?
		ORDER BY p.` + strings.Join(order, ", p.")
	args = append(args, q.snippetTerm())
	return scanContentRows(ctx, tx, query, args, func(body, fragment string) string {
		return f.SemanticSnippet(body, fragment)
	})
}

// lexicalMessageFilter is the scope, system, and anchor filter the substring
// search applies to messages.
func lexicalMessageFilter(f ContentSearchFilter) contentWordFilter {
	scope, args := sessionScopeSubquery(f)
	where := LexicalAnchorScopeSQL(f, "s", "m", SQLiteQueryDialect())
	if f.ExcludeSystem {
		where += " AND m.is_system = 0 AND " + SystemPrefixSQL("m.content", "m.role")
	}
	return contentWordFilter{scope: scope, scopeArgs: args, where: where}
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
		filter := lexicalMessageFilter(f)
		if exact {
			if matches, total, err = exactContentRows(ctx, snapshot.tx, f, candidates, len(q.terms)); err != nil {
				return nil, err
			}
			filter.where += ` AND COALESCE(m.dialogue_text, m.content) NOT LIKE ? ESCAPE '\'`
			filter.args = append(filter.args, "%"+escapeLike(f.Pattern)+"%")
		}
		if len(matches) > f.Limit {
			return matches, nil
		}
		words, err := selectContentWordPage(ctx, snapshot.tx, f, q, filter,
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
