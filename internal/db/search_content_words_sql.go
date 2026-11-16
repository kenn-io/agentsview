package db

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// WordCandidateSQL selects candidates holding prefixes of all or any query words.
func WordCandidateSQL(b *QueryBuilder, column string, q ContentWords, all bool) string {
	parts := make([]string, len(q.Terms))
	for i, forms := range q.Terms {
		parts[i] = b.ContainsPredicate(column, wordCandidatePrefix(forms))
	}
	op := " OR "
	if all {
		op = " AND "
	}
	return "(" + strings.Join(parts, op) + ")"
}

// WordRegexSQL requires every phrase and, when all is true, every word.
func WordRegexSQL(b *QueryBuilder, raw string, q ContentWords, all bool) string {
	return wordRegexSQL(b, raw, q, all, false)
}

// WordASCIIRegexSQL admits Unicode boundary false positives for later validation.
func WordASCIIRegexSQL(b *QueryBuilder, raw string, q ContentWords) string {
	return wordRegexSQL(b, raw, q, true, true)
}

func wordRegexSQL(b *QueryBuilder, raw string, q ContentWords, all, ascii bool) string {
	parts := []string{"1=1"}
	add := func(pattern string) {
		if ascii {
			pattern = strings.ReplaceAll(pattern, `[^\p{L}\p{N}]`, `[^a-zA-Z0-9]`)
			parts = append(parts, b.RegexPredicate(raw, pattern))
		} else {
			parts = append(parts, wordPredicate(b, raw, pattern))
		}
	}
	if all {
		for _, forms := range q.Terms {
			add(boundedPattern(b, compactForms(b, forms)))
		}
	}
	for _, phrase := range q.Phrases {
		words := strings.FieldsFunc(phrase, isWordSeparator)
		for i := range words {
			words[i] = regexp.QuoteMeta(words[i])
		}
		if len(words) > 0 {
			add(boundedPattern(b, words...))
		}
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// WordCountSQL counts query words held as whole words.
func WordCountSQL(b *QueryBuilder, raw string, q ContentWords) string {
	parts := make([]string, len(q.Terms))
	for i, forms := range q.Terms {
		parts[i] = "CASE WHEN " + b.ContainsPredicate(raw, wordCandidatePrefix(forms)) +
			" AND " + wordPredicate(b, raw, boundedPattern(b, compactForms(b, forms))) + " THEN 1 ELSE 0 END"
	}
	return "(" + strings.Join(parts, " + ") + ")"
}

func wordPredicate(b *QueryBuilder, raw, pattern string) string {
	if b.dialect.name == "postgres" {
		return b.RegexPredicate(raw, pattern)
	}
	ascii := strings.ReplaceAll(pattern, `[^\p{L}\p{N}]`, `[^a-zA-Z0-9]`)
	isASCII := "octet_length(encode(" + raw + ")) = length(" + raw + ")"
	if b.dialect.name == "clickhouse" {
		isASCII = "length(" + raw + ") = lengthUTF8(" + raw + ")"
	}
	return "(CASE WHEN " + isASCII + " THEN " + b.RegexPredicate(raw, ascii) + " ELSE " + b.RegexPredicate(raw, pattern) + " END)"
}

// WordFilterSQL selects candidate words and requires every phrase.
func WordFilterSQL(b *QueryBuilder, column string, f ContentSearchFilter, q ContentWords, all bool) string {
	exact := ""
	if all && f.Mode != "fts" {
		exact = b.ContainsPredicate(column, f.Pattern) + " OR "
	}
	return "(" + exact + "(" + WordCandidateSQL(b, column, q, all) + " AND " + WordRegexSQL(b, column, q, all) + "))"
}

// WordRankedSQL orders branches, DuckDB or ClickHouse content candidates, by
// tier, which puts the exact text first outside fts mode, then by n, the query
// words a row holds.
func WordRankedSQL(b *QueryBuilder, branches []string, f ContentSearchFilter, q ContentWords, all bool) string {
	n, tier := strconv.Itoa(len(q.Terms)), "1"
	if !all && f.Mode != "fts" {
		// The exact text counts every word. Binds follow the SQL text.
		n = "CASE WHEN " + b.ContainsPredicate("body", f.Pattern) + " THEN " + n + " ELSE " +
			WordCountSQL(b, "body", q) + " END"
	} else if !all {
		n = WordCountSQL(b, "body", q)
	}
	if f.Mode != "fts" {
		tier = "CASE WHEN " + b.ContainsPredicate("body", f.Pattern) + " THEN 0 ELSE 1 END"
	}
	materialized, matched := "", ""
	if !all {
		materialized, matched = " MATERIALIZED", " WHERE n >= 1"
	}
	return `WITH candidates AS (` + strings.Join(branches, " UNION ALL ") + `),
			ranked AS` + materialized + ` (
				SELECT candidates.*, ` + n + " AS n, " + tier + " AS tier" + ` FROM candidates
			)
			SELECT session_id, project, agent, location, role, tool_name,
				ordinal, ts, body, n
			FROM ranked` + matched + `
			ORDER BY tier, n DESC, sort_ts DESC, session_id ASC, ordinal ASC,
				src ASC, row_id ASC, call_index ASC, event_index ASC, body ASC
			LIMIT ? OFFSET ?`
}

// boundedPattern matches words separated by non-letter, non-digit runs, with
// a boundary on each side. PostgreSQL's ~* ignores case; RE2 takes (?i),
// which ClickHouse's regexPredicate repeats harmlessly.
func boundedPattern(b *QueryBuilder, words ...string) string {
	gap := wordGap(b)
	pattern := "(^|" + gap + ")" + strings.Join(words, gap+"+") + "($|" + gap + ")"
	if b.dialect.name == "postgres" {
		return pattern
	}
	return "(?i)" + pattern
}

// wordGap is the character class between words.
func wordGap(b *QueryBuilder) string {
	if b.dialect.name == "postgres" {
		return "[^[:alnum:]]"
	}
	return `[^\p{L}\p{N}]`
}

// compactForms folds forms to their common prefix and an alternation of
// suffixes, which roughly halves PostgreSQL's regex cost.
func compactForms(b *QueryBuilder, forms []string) string {
	if slices.ContainsFunc(forms, func(form string) bool {
		return strings.IndexFunc(form, isWordSeparator) >= 0
	}) {
		patterns := make([]string, len(forms))
		for i, form := range forms {
			patterns[i] = wordFormRegex(b, form)
		}
		return "(" + strings.Join(patterns, "|") + ")"
	}
	prefix := wordPrefix(forms)
	suffixes := make([]string, len(forms))
	for i, form := range forms {
		suffixes[i] = wordFormRegex(b, form[len(prefix):])
	}
	slices.Sort(suffixes)
	return wordFormRegex(b, prefix) + "(" + strings.Join(slices.Compact(suffixes), "|") + ")"
}

func wordFormRegex(b *QueryBuilder, form string) string {
	parts := strings.FieldsFunc(form, isWordSeparator)
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return strings.Join(parts, wordGap(b)+"+")
}

func wordCandidatePrefix(forms []string) string {
	prefix := wordPrefix(forms)
	if i := strings.IndexFunc(prefix, isWordSeparator); i >= 0 {
		return prefix[:i]
	}
	return prefix
}

// wordPrefix returns the common prefix of forms, which always hold the word.
func wordPrefix(forms []string) string {
	prefix := forms[0]
	for _, form := range forms[1:] {
		for !strings.HasPrefix(form, prefix) {
			_, size := utf8.DecodeLastRuneInString(prefix)
			prefix = prefix[:len(prefix)-size]
		}
	}
	return prefix
}
