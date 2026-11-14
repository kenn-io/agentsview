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
// query word's forms, or with all false of any word. The LIKE gate selects
// candidates before the whole-word regexes run. With fold, Latin words compare
// without diacritics; without it, PostgreSQL's trigram index serves the gate.
func WordCandidateSQL(b *QueryBuilder, column string, q ContentWords, all, fold bool) string {
	parts := make([]string, len(q.Terms))
	for i, forms := range q.Terms {
		foldTerm := fold && wordUsesLatinDiacritics(forms)
		parts[i] = wordContainsSQL(b, column, wordPrefix(forms), foldTerm, false)
	}
	op := " OR "
	if all {
		op = " AND "
	}
	return "(" + strings.Join(parts, op) + ")"
}

// WordRegexSQL requires every quoted phrase in raw and, when all is true,
// every query word as a whole word. folded is raw as WordFoldSQL renders it,
// or a column already holding that.
func WordRegexSQL(b *QueryBuilder, raw, folded string, q ContentWords, all bool) string {
	parts := []string{"1=1"}
	if all {
		for _, forms := range q.Terms {
			parts = append(parts, wordTermSQL(b, raw, folded, forms))
		}
	}
	for _, phrase := range q.Phrases {
		words := strings.FieldsFunc(phrase, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		for i := range words {
			words[i] = regexp.QuoteMeta(words[i])
		}
		// As phraseMatch does, only non-letter, non-digit runs may separate them.
		if len(words) > 0 {
			parts = append(parts, b.RegexPredicate(raw, boundedPattern(b, true, words...)))
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

// WordCountSQL counts the query words raw holds as whole words, with folded
// as in WordRegexSQL.
func WordCountSQL(b *QueryBuilder, raw, folded string, q ContentWords) string {
	parts := make([]string, len(q.Terms))
	for i, forms := range q.Terms {
		foldTerm := wordUsesLatinDiacritics(forms)
		candidateColumn := raw
		if foldTerm {
			candidateColumn = folded
		}
		parts[i] = "CASE WHEN " + wordContainsSQL(b, candidateColumn, wordPrefix(forms), foldTerm, foldTerm) +
			" AND " + wordTermSQL(b, raw, folded, forms) + " THEN 1 ELSE 0 END"
	}
	return "(" + strings.Join(parts, " + ") + ")"
}

// WordFilterSQL matches rows in column holding every query word and phrase,
// or with all false any word's prefix and every phrase, leaving the count to
// the caller. With all, outside fts mode, it also matches the exact text.
func WordFilterSQL(b *QueryBuilder, column string, f ContentSearchFilter, q ContentWords, all bool) string {
	exact := ""
	if all && f.Mode != "fts" {
		exact = b.ContainsPredicate(column, f.Pattern) + " OR "
	}
	return "(" + exact + "(" + WordCandidateSQL(b, column, q, all, true) + " AND " +
		WordRegexSQL(b, column, WordFoldSQL(b.dialect, column), q, all) + "))"
}

// WordRankSQL selects n, the query words a row in column holds, and tier,
// which puts the exact text first outside fts mode.
func WordRankSQL(b *QueryBuilder, column string, f ContentSearchFilter, q ContentWords, all bool) string {
	n, tier := strconv.Itoa(len(q.Terms)), "1"
	if !all && f.Mode != "fts" {
		// The exact text counts every word. Binds follow the SQL text.
		n = "CASE WHEN " + b.ContainsPredicate(column, f.Pattern) + " THEN " + n + " ELSE " +
			WordCountSQL(b, column, WordFoldSQL(b.dialect, column), q) + " END"
	} else if !all {
		n = WordCountSQL(b, column, WordFoldSQL(b.dialect, column), q)
	}
	if f.Mode != "fts" {
		tier = "CASE WHEN " + b.ContainsPredicate(column, f.Pattern) + " THEN 0 ELSE 1 END"
	}
	return n + " AS n, " + tier + " AS tier"
}

// WordFoldSQL removes diacritics from expression for natural word
// predicates, leaving ASCII text as it is.
func WordFoldSQL(d QueryDialect, expression string) string {
	switch d.name {
	case "postgres":
		folded := `regexp_replace(normalize(` + expression +
			`, NFD), E'([A-Za-z])[\\u0300-\\u036F]+', E'\\1', 'g')`
		return "CASE WHEN octet_length(" + expression + ") = char_length(" + expression +
			") THEN " + expression + " ELSE " + folded + " END"
	case "duckdb":
		return "CASE WHEN strlen(" + expression + ") = length(" + expression +
			") THEN " + expression + " ELSE strip_accents(" + expression + ") END"
	case "clickhouse":
		return "if(length(" + expression + ") = lengthUTF8(" + expression +
			"), " + expression + ", removeDiacriticsUTF8(" + expression + "))"
	default:
		return expression
	}
}

// wordFoldPatternSQL binds pattern without diacritics. A placeholder appears
// once, since DuckDB and ClickHouse bind by position.
func wordFoldPatternSQL(b *QueryBuilder, pattern string) string {
	ph := b.Add(pattern)
	switch b.dialect.name {
	case "duckdb":
		return "strip_accents(" + ph + ")"
	case "clickhouse":
		return "removeDiacriticsUTF8(" + ph + ")"
	default:
		return WordFoldSQL(b.dialect, ph)
	}
}

func wordContainsSQL(b *QueryBuilder, column, pattern string, fold, columnFolded bool) string {
	pattern = "%" + EscapeLikePattern(pattern) + "%"
	var patternSQL string
	if fold {
		if !columnFolded {
			column = WordFoldSQL(b.dialect, column)
		}
		patternSQL = wordFoldPatternSQL(b, pattern)
	} else {
		patternSQL = b.Add(pattern)
	}
	return column + " " + b.dialect.caseInsensitiveLike + " " + patternSQL + " " + b.dialect.caseInsensitiveLikeEsc
}

// wordTermSQL matches any of forms as a whole word, case-insensitively.
// PostgreSQL's classes already follow Unicode. RE2's Unicode classes are
// slow, so other dialects test ASCII rows with ASCII classes and keep
// Unicode classes for the rest, where folding a diacritic must not turn an
// adjacent non-Latin letter into a boundary.
func wordTermSQL(b *QueryBuilder, raw, folded string, forms []string) string {
	compact := compactForms(forms)
	if !wordUsesLatinDiacritics(forms) {
		return b.dialect.regexPredicate(raw, b.Add(boundedPattern(b, true, compact)))
	}
	if b.dialect.name == "postgres" {
		return b.dialect.regexPredicate(folded, wordFoldPatternSQL(b, boundedPattern(b, true, compact)))
	}
	asciiPattern := wordFoldPatternSQL(b, boundedPattern(b, false, compact))
	unicodePattern := wordFoldPatternSQL(b, boundedPattern(b, true, compact))
	ascii := "strlen(" + raw + ") = length(" + raw + ")"
	if b.dialect.name == "clickhouse" {
		ascii = "length(" + raw + ") = lengthUTF8(" + raw + ")"
	}
	return "CASE WHEN " + ascii + " THEN " + b.dialect.regexPredicate(raw, asciiPattern) +
		" ELSE " + b.dialect.regexPredicate(folded, unicodePattern) + " END"
}

func wordUsesLatinDiacritics(forms []string) bool {
	hasLatin := false
	for _, form := range forms {
		for _, r := range form {
			if !unicode.IsLetter(r) {
				continue
			}
			if !unicode.In(r, unicode.Latin) {
				return false
			}
			hasLatin = true
		}
	}
	return hasLatin
}

// boundedPattern matches words separated by non-letter, non-digit runs, with
// a boundary on each side. PostgreSQL's ~* ignores case; RE2 takes (?i),
// which ClickHouse's regexPredicate repeats harmlessly.
func boundedPattern(b *QueryBuilder, unicodeClasses bool, words ...string) string {
	flags, gap := "(?i)", `[^\p{L}\p{N}]`
	if b.dialect.name == "postgres" {
		flags, gap = "", "[^[:alnum:]]"
	} else if !unicodeClasses {
		gap = "[^[:alnum:]]"
	}
	return flags + "(^|" + gap + ")" + strings.Join(words, gap+"+") + "($|" + gap + ")"
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
