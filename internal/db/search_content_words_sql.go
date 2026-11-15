package db

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
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

// wordRegexSQL is WordRegexSQL with ascii set to bound words by the ASCII gap,
// a superset of the Unicode result that ContentWords.HoldsWords narrows.
func wordRegexSQL(b *QueryBuilder, raw string, q ContentWords, all, ascii bool) string {
	parts := []string{"1=1"}
	if all {
		for _, forms := range q.Terms {
			parts = append(parts, b.RegexPredicate(raw, boundedPattern(b, ascii, compactForms(b, ascii, forms))))
		}
	}
	for _, phrase := range q.Phrases {
		words := strings.FieldsFunc(phrase, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		for i := range words {
			words[i] = regexp.QuoteMeta(words[i])
		}
		if len(words) > 0 {
			parts = append(parts, b.RegexPredicate(raw, boundedPattern(b, ascii, words...)))
		}
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// WordCountSQL counts query words held as whole words.
func WordCountSQL(b *QueryBuilder, raw string, q ContentWords) string {
	parts := make([]string, len(q.Terms))
	for i, forms := range q.Terms {
		parts[i] = "CASE WHEN " + b.ContainsPredicate(raw, wordCandidatePrefix(forms)) +
			" AND " + b.RegexPredicate(raw, boundedPattern(b, false, compactForms(b, false, forms))) + " THEN 1 ELSE 0 END"
	}
	return "(" + strings.Join(parts, " + ") + ")"
}

// WordFilterSQL selects candidate words and requires every phrase. The all-word
// pass bounds words by ASCII characters, which DuckDB and ClickHouse plan far
// faster than Unicode classes. Its rows hold every word or are a superset
// whose non-ASCII bodies ContentWords.HoldsWords must confirm.
func WordFilterSQL(b *QueryBuilder, column string, f ContentSearchFilter, q ContentWords, all bool) string {
	exact := ""
	if all && f.Mode != "fts" {
		exact = b.ContainsPredicate(column, f.Pattern) + " OR "
	}
	return "(" + exact + "(" + WordCandidateSQL(b, column, q, all) + " AND " + wordRegexSQL(b, column, q, all, all) + "))"
}

// WordRankedSQL orders branches, DuckDB or ClickHouse content candidates, by
// tier, which puts the exact text first outside fts mode, then by n, the query
// words a row holds. The all-word pass is unpaged and adds a final column, 1
// for the exact text, because its caller pages after ContentWords.HoldsWords.
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
	materialized, matched, exactText, page := "", "", "", "\n\t\t\tLIMIT ? OFFSET ?"
	if all {
		exactText, page = ", CASE WHEN tier = 0 THEN 1 ELSE 0 END AS exact_text", ""
	} else {
		materialized, matched = " MATERIALIZED", " WHERE n >= 1"
	}
	return `WITH candidates AS (` + strings.Join(branches, " UNION ALL ") + `),
			ranked AS` + materialized + ` (
				SELECT candidates.*, ` + n + " AS n, " + tier + " AS tier" + ` FROM candidates
			)
			SELECT session_id, project, agent, location, role, tool_name,
				ordinal, ts, body, n` + exactText + `
			FROM ranked` + matched + `
			ORDER BY tier, n DESC, sort_ts DESC, session_id ASC, ordinal ASC,
				src ASC, row_id ASC, call_index ASC, event_index ASC, body ASC` + page
}

// boundedPattern matches words separated by non-letter, non-digit runs, with
// a boundary on each side. PostgreSQL's ~* ignores case; RE2 takes (?i),
// which ClickHouse's regexPredicate repeats harmlessly.
func boundedPattern(b *QueryBuilder, ascii bool, words ...string) string {
	gap := wordGap(b, ascii)
	pattern := "(^|" + gap + ")" + strings.Join(words, gap+"+") + "($|" + gap + ")"
	if b.dialect.name == "postgres" {
		return pattern
	}
	return "(?i)" + pattern
}

// wordGap is the character class between words. With ascii set, RE2 dialects
// treat every non-ASCII rune as a boundary; the class only widens matches.
func wordGap(b *QueryBuilder, ascii bool) string {
	switch {
	case b.dialect.name == "postgres":
		return "[^" + postgresWordCharacters + "]"
	case ascii:
		return "[^a-zA-Z0-9]"
	}
	return `[^\p{L}\p{N}]`
}

// PostgreSQL character classes depend on LC_CTYPE. Build the same letter and
// number class used by the other dialects from Go's Unicode tables instead.
var postgresWordCharacters = func() string {
	var chars strings.Builder
	for r := rune(0); r <= unicode.MaxRune; {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			r++
			continue
		}
		start := r
		for r < unicode.MaxRune && (unicode.IsLetter(r+1) || unicode.IsNumber(r+1)) {
			r++
		}
		chars.WriteRune(start)
		if r > start {
			chars.WriteByte('-')
			chars.WriteRune(r)
		}
		r++
	}
	return chars.String()
}()

// compactForms folds forms to their common prefix and an alternation of
// suffixes, which roughly halves PostgreSQL's regex cost.
func compactForms(b *QueryBuilder, ascii bool, forms []string) string {
	if slices.ContainsFunc(forms, func(form string) bool {
		return strings.IndexFunc(form, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) >= 0
	}) {
		patterns := make([]string, len(forms))
		for i, form := range forms {
			patterns[i] = wordFormRegex(b, ascii, form)
		}
		return "(" + strings.Join(patterns, "|") + ")"
	}
	prefix := wordPrefix(forms)
	suffixes := make([]string, len(forms))
	for i, form := range forms {
		suffixes[i] = wordFormRegex(b, ascii, form[len(prefix):])
	}
	slices.Sort(suffixes)
	return wordFormRegex(b, ascii, prefix) + "(" + strings.Join(slices.Compact(suffixes), "|") + ")"
}

func wordFormRegex(b *QueryBuilder, ascii bool, form string) string {
	parts := strings.FieldsFunc(form, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return strings.Join(parts, wordGap(b, ascii)+"+")
}

func wordCandidatePrefix(forms []string) string {
	prefix := wordPrefix(forms)
	if i := strings.IndexFunc(prefix, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }); i >= 0 {
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
