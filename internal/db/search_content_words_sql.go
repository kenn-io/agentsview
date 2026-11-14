package db

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// WordCandidateSQL matches rows in column holding the common prefix of every
// query word's forms, or with all false of any word. The substring tests let
// PostgreSQL use its trigram index before the whole-word regexes run.
func WordCandidateSQL(b *QueryBuilder, column string, q ContentWords, all bool) string {
	parts := make([]string, len(q.Terms))
	for i, forms := range q.Terms {
		parts[i] = b.ContainsPredicate(column, wordPrefix(forms))
	}
	op := " OR "
	if all {
		op = " AND "
	}
	return "(" + strings.Join(parts, op) + ")"
}

// WordRegexSQL requires every quoted phrase in column and, when all is true,
// every query word as a whole word.
func WordRegexSQL(b *QueryBuilder, column string, q ContentWords, all bool) string {
	parts := []string{"1=1"}
	if all {
		for _, forms := range q.Terms {
			parts = append(parts, b.RegexPredicate(column, wordPattern(b, forms)))
		}
	}
	for _, phrase := range q.Phrases {
		if pattern := phrasePattern(b, phrase); pattern != "" {
			parts = append(parts, b.RegexPredicate(column, pattern))
		}
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// WordPrefixSQL proves an all-word match when a plain query starts the body.
func WordPrefixSQL(b *QueryBuilder, column string, f ContentSearchFilter, q ContentWords) string {
	fields := strings.Fields(f.Pattern)
	if len(q.Phrases) != 0 || len(fields) != len(q.Terms) || strings.Join(fields, " ") != f.Pattern {
		return ""
	}
	for i, field := range fields {
		if strings.ContainsFunc(field, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) ||
			!slices.ContainsFunc(q.Terms[i], func(form string) bool { return strings.EqualFold(field, form) }) {
			return ""
		}
	}
	prefix := b.Add(f.Pattern)
	length := b.Add(f.Pattern)
	return "(" + column + " ILIKE " + prefix + " || '%' AND substring(" + column + " from char_length(" + length + ")+1 for 1) !~ '[[:alnum:]]')"
}

// WordCountSQL counts the query words column holds as whole words.
func WordCountSQL(b *QueryBuilder, column string, q ContentWords) string {
	parts := make([]string, len(q.Terms))
	for i, forms := range q.Terms {
		parts[i] = "CASE WHEN " + b.ContainsPredicate(column, wordPrefix(forms)) +
			" AND " + b.RegexPredicate(column, wordPattern(b, forms)) + " THEN 1 ELSE 0 END"
	}
	return "(" + strings.Join(parts, " + ") + ")"
}

// WordFilterSQL matches rows in column holding every query word and phrase,
// or with all false any word and every phrase. Outside fts mode it also
// matches the exact text.
func WordFilterSQL(b *QueryBuilder, column string, f ContentSearchFilter, q ContentWords, all bool) string {
	exact := ""
	if f.Mode != "fts" {
		exact = b.ContainsPredicate(column, f.Pattern) + " OR "
	}
	match := WordCandidateSQL(b, column, q, all)
	if !all {
		match += " AND " + WordCountSQL(b, column, q) + " >= 1"
	}
	return "(" + exact + "(" + match + " AND " + WordRegexSQL(b, column, q, all) + "))"
}

// WordRankSQL selects n, the query words a row in column holds, and tier,
// which puts the exact text first outside fts mode.
func WordRankSQL(b *QueryBuilder, column string, f ContentSearchFilter, q ContentWords, all bool) string {
	n, tier := strconv.Itoa(len(q.Terms)), "1"
	if !all && f.Mode != "fts" {
		// The exact text counts every word. Binds follow the SQL text.
		n = "CASE WHEN " + b.ContainsPredicate(column, f.Pattern) + " THEN " + n + " ELSE " + WordCountSQL(b, column, q) + " END"
	} else if !all {
		n = WordCountSQL(b, column, q)
	}
	if f.Mode != "fts" {
		tier = "CASE WHEN " + b.ContainsPredicate(column, f.Pattern) + " THEN 0 ELSE 1 END"
	}
	return n + " AS n, " + tier + " AS tier"
}

// wordPattern matches any of forms as a whole word, case-insensitively.
// PostgreSQL's ~* ignores case; RE2 takes (?i), which ClickHouse's
// regexPredicate repeats harmlessly. ASCII classes serve ASCII words.
func wordPattern(b *QueryBuilder, forms []string) string {
	pattern := compactForms(forms)
	switch {
	case b.dialect.name == "postgres":
		return `(^|[^[:alnum:]])` + pattern + `($|[^[:alnum:]])`
	case !strings.ContainsFunc(strings.Join(forms, ""), func(r rune) bool { return r > unicode.MaxASCII }):
		return `(?i)(^|[^[:alnum:]])` + pattern + `($|[^[:alnum:]])`
	default:
		return `(?i)(^|[^\p{L}\p{N}])` + pattern + `($|[^\p{L}\p{N}])`
	}
}

// phrasePattern matches phrase's words in order with only non-letter,
// non-digit runs between them, as phraseMatch does.
func phrasePattern(b *QueryBuilder, phrase string) string {
	words := strings.FieldsFunc(phrase, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if len(words) == 0 {
		return ""
	}
	for i := range words {
		words[i] = regexp.QuoteMeta(words[i])
	}
	if b.dialect.name == "postgres" {
		return `(^|[^[:alnum:]])` + strings.Join(words, `[^[:alnum:]]+`) + `($|[^[:alnum:]])`
	}
	return `(?i)(^|[^\p{L}\p{N}])` + strings.Join(words, `[^\p{L}\p{N}]+`) + `($|[^\p{L}\p{N}])`
}

// compactForms folds forms to their common prefix and an alternation of
// suffixes, which roughly halves PostgreSQL's regex cost.
func compactForms(forms []string) string {
	prefix := wordPrefix(forms)
	suffixes := make([]string, len(forms))
	for i, form := range forms {
		suffixes[i] = regexp.QuoteMeta(form[len(prefix):])
	}
	slices.Sort(suffixes)
	return regexp.QuoteMeta(prefix) + "(" + strings.Join(slices.Compact(suffixes), "|") + ")"
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
