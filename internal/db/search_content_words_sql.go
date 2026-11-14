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
// candidates before whole-word regexes run. With fold, it applies SQLite's
// fixed Unicode61 mapping; without it, PostgreSQL's trigram index serves the gate.
func WordCandidateSQL(b *QueryBuilder, column string, q ContentWords, all, fold bool) string {
	parts := make([]string, len(q.Terms))
	for i, forms := range q.Terms {
		prefix := wordPrefix(forms)
		if fold {
			parts[i] = wordContainsSQL(b, column, prefix, true, false)
			continue
		}
		parts[i] = wordContainsSQL(b, column, prefix, false, false)
		foldedPrefix := foldUnicode61(prefix)
		if foldedPrefix != prefix {
			parts[i] = "(" + parts[i] + " OR " + wordContainsSQL(b, column, foldedPrefix, false, false) + ")"
		}
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
			words[i] = wordCaseFoldRegex(words[i])
		}
		// As phraseMatch does, only non-letter, non-digit runs may separate them.
		if len(words) > 0 {
			parts = append(parts, b.RegexPredicate(raw, boundedPattern(b, true, words...)))
		}
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// WordCountSQL counts the query words raw holds as whole words, with folded
// as in WordRegexSQL.
func WordCountSQL(b *QueryBuilder, raw, folded string, q ContentWords) string {
	parts := make([]string, len(q.Terms))
	for i, forms := range q.Terms {
		parts[i] = "CASE WHEN " + wordContainsSQL(b, folded, wordPrefix(forms), true, true) +
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
	raw := WordCandidateSQL(b, column, q, all, false)
	rawExclude := WordCandidateSQL(b, column, q, all, false)
	folded := WordCandidateSQL(b, column, q, all, true)
	candidate := "(" + raw + " OR (" + WordNonASCIISQL(b.dialect, column) +
		" AND NOT " + rawExclude + " AND " + folded + "))"
	return "(" + exact + "(" + candidate + " AND " +
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

// WordFoldSQL applies the pinned SQLite Unicode61 fold to expression.
func WordFoldSQL(d QueryDialect, expression string) string {
	from := "'" + unicode61FoldFrom + "'"
	to := "'" + unicode61FoldTo + "'"
	switch d.name {
	case "postgres", "cockroach":
		return "translate(" + expression + ", " + from + ", " + to + ")"
	case "duckdb":
		return "translate(" + expression + ", " + from + ", " + to + ")"
	case "clickhouse":
		return "translateUTF8(" + expression + ", " + from + ", " + to + ")"
	default:
		return expression
	}
}

// WordNonASCIISQL identifies rows that may need the Unicode61 branch.
func WordNonASCIISQL(d QueryDialect, expression string) string {
	switch d.name {
	case "postgres", "cockroach":
		return "octet_length(" + expression + ") > char_length(" + expression + ")"
	case "duckdb":
		return "strlen(" + expression + ") > length(" + expression + ")"
	case "clickhouse":
		return "length(" + expression + ") > lengthUTF8(" + expression + ")"
	default:
		return "1=0"
	}
}

func wordContainsSQL(b *QueryBuilder, column, pattern string, fold, columnFolded bool) string {
	if fold {
		pattern = foldUnicode61(pattern)
	}
	if fold && !columnFolded {
		column = WordFoldSQL(b.dialect, column)
	}
	pattern = "%" + EscapeLikePattern(pattern) + "%"
	return "(" + column + " " + b.dialect.caseInsensitiveLike + " " + b.Add(pattern) + " " +
		b.dialect.caseInsensitiveLikeEsc + ")"
}

// wordTermSQL matches any form as a whole word using SQLite's fixed fold.
// RE2's Unicode classes are slower, so non-PostgreSQL dialects use ASCII
// classes only when the raw body is ASCII.
func wordTermSQL(b *QueryBuilder, raw, folded string, forms []string) string {
	foldedForms := make([]string, len(forms))
	for i, form := range forms {
		foldedForms[i] = foldUnicode61(form)
	}
	compact := compactForms(foldedForms)
	if b.dialect.name == "postgres" {
		return b.dialect.regexPredicate(folded, b.Add(boundedPattern(b, true, compact)))
	}
	asciiPattern := b.Add(boundedPattern(b, false, compact))
	unicodePattern := b.Add(boundedPattern(b, true, compact))
	ascii := "NOT " + WordNonASCIISQL(b.dialect, raw)
	return "CASE WHEN " + ascii + " THEN " + b.dialect.regexPredicate(folded, asciiPattern) +
		" ELSE " + b.dialect.regexPredicate(folded, unicodePattern) + " END"
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
		suffixes[i] = wordCaseFoldRegex(form[len(prefix):])
	}
	slices.Sort(suffixes)
	return wordCaseFoldRegex(prefix) + "(" + strings.Join(slices.Compact(suffixes), "|") + ")"
}

func wordCaseFoldRegex(word string) string {
	var pattern strings.Builder
	for _, r := range word {
		folds := append([]rune{r}, wordCaseFoldAliases(r)...)
		if len(folds) == 1 {
			pattern.WriteString(regexp.QuoteMeta(string(r)))
			continue
		}
		pattern.WriteByte('[')
		for _, folded := range folds {
			pattern.WriteRune(folded)
		}
		pattern.WriteByte(']')
	}
	return pattern.String()
}

func wordCaseFoldAliases(r rune) []rune {
	if r > unicode.MaxASCII || !unicode.IsLetter(r) {
		return nil
	}
	var aliases []rune
	for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
		if folded > unicode.MaxASCII {
			aliases = append(aliases, folded)
		}
	}
	return aliases
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
