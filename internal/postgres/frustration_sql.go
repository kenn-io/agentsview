package postgres

import (
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/signals"
)

// pgFrustrationPromptsSQL reads the transcript version of each session in
// the array $1 and the prompts of those sessions, in one statement so both
// share a snapshot. The %s names the session version column. Prompts are the
// rows the signals scan reads: user rows that are neither system rows nor
// tool results.
//
// A plain prompt, valid UTF-8 made of ASCII and signals.PlainPromptSymbols,
// is judged in SQL when that is cheap: it comes back only when it is a
// marker, with empty content. Any other prompt, and a plain prompt SQL
// cannot judge cheaply, comes back with its content for
// signals.IsFrustrationMarker. Plain judging needs a UTF8 server encoding;
// other databases read pgFrustrationGoPromptsSQL instead.
//
// The verdict follows signals.IsFrustrationMarker in order, over the prompt
// with its code fences replaced by a space:
//
//  1. Length. The normalized prompt has at least FrustrationMinBytes bytes
//     when the prompt has that many non-space characters; otherwise SQL
//     collapses the whitespace runs and trims the ends to measure it.
//  2. Phrase. The phrase pattern matches the stripped prompt with each
//     letter as an ASCII case pair and each space as a whitespace run, which
//     is how it matches the normalized prompt.
//  3. Caps, ruled out. Words are ASCII letter runs, which in a plain prompt
//     are exactly Go's unicode.IsLetter runs. All-capital words are at most
//     the [A-Z]{2,} runs, and words of two or more letters are at least 32
//     per match of a run of 32 such words, and at least
//     FrustrationCapsMinWords when the rule can fire. When that bound on
//     the ratio is under FrustrationCapsRatio, the exact ratio is too.
//     Try the caps-run count alone first. For long prompts, try the
//     32-word bound next.
//  4. Caps, exactly, for an unresolved prompt of at most pgExactCapsMaxBytes,
//     where counting every word costs little. A longer prompt goes to Go.
//
// Every pattern names its characters explicitly, so neither the server
// locale nor the regular expression flavor (POSIX ARE in PostgreSQL, RE2 in
// CockroachDB) changes which characters are letters, spaces, or word
// characters. Match counts use regexp_split_to_array, which both engines
// have. Each OFFSET 0 keeps the planner from inlining a subquery and
// computing a column once per reference. The first level copies the content
// once, so later steps do not each decompress the stored value.
const pgFrustrationPromptsSQL = `
	SELECT id, %s::text, false, ''
	FROM sessions
	WHERE id = ANY($1)
	UNION ALL
	SELECT session_id, NULL, verdict = 1, CASE WHEN verdict = 2 THEN content ELSE '' END
	FROM (
		SELECT session_id, content, CASE
			WHEN NOT plain THEN 2
			WHEN NOT (stripped ~ $5
				OR octet_length(btrim(regexp_replace(stripped, $3, ' ', 'g'), ' ')) >= $6::int8) THEN 0
			WHEN stripped ~ $7 THEN 1
			WHEN (
				SELECT CASE WHEN caps_runs::float8 / $10::float8 < $11::float8 THEN true
					WHEN octet_length(stripped) > $12::int8 THEN caps_runs::float8 / greatest($9::int8 * (cardinality(regexp_split_to_array(stripped, $8)) - 1), $10::int8)::float8 < $11::float8
					ELSE false END
				FROM (SELECT cardinality(regexp_split_to_array(stripped, '[A-Z]{2,}')) - 1 AS caps_runs OFFSET 0) c
			) THEN 0
			WHEN octet_length(stripped) > $12::int8 THEN 2
			WHEN (
				SELECT CASE WHEN long_words >= $10::int8 THEN caps_words::float8 / long_words::float8 >= $11::float8 ELSE false END
				FROM (
					SELECT
						cardinality(regexp_split_to_array(stripped, '[A-Za-z]{2,}')) - 1 AS long_words,
						cardinality(regexp_split_to_array(
							regexp_replace(stripped, '[A-Za-z]*[a-z][A-Za-z]*', ' ', 'g'), '[A-Z]{2,}')) - 1 AS caps_words
				) c
			) THEN 1
			ELSE 0
		END AS verdict
		FROM (
			SELECT session_id, content, plain,
				CASE WHEN plain AND strpos(content, '` + "```" + `') > 0
					THEN regexp_replace(content, $2, ' ', 'g') ELSE content END AS stripped
			FROM (
				SELECT session_id, content || '' AS content,
					content !~ $4 AS plain
				FROM messages
				WHERE role = 'user'
				  AND source_subtype <> 'tool_result'
				  AND NOT is_system
				  AND session_id = ANY($1)
				OFFSET 0
			) m
			OFFSET 0
		) s
		OFFSET 0
	) v
	WHERE verdict > 0`

const (
	// pgLongWordRun is how many consecutive words of two or more letters
	// one match of the run pattern spans.
	pgLongWordRun = 32
	// pgExactCapsMaxBytes is the longest stripped prompt whose caps ratio
	// SQL counts exactly. Typed prompts fit; long pastes the bound cannot
	// rule out go to Go.
	pgExactCapsMaxBytes = 16 << 10

	// The ASCII runes unicode.IsSpace accepts, written as literal runes.
	pgSpace    = "[\t\n\v\f\r ]"
	pgNonSpace = "[^\t\n\v\f\r ]"
	pgSpaceRun = pgSpace + "+"
)

// pgFrustrationPromptsArgs are the arguments of pgFrustrationPromptsSQL
// after the session IDs.
var pgFrustrationPromptsArgs = []any{
	signals.CodeFencePattern, // $2
	pgSpaceRun,               // $3
	pgOtherPromptRunePattern(),
	"(" + pgNonSpace + pgSpace + "*){" + strconv.Itoa(signals.FrustrationMinBytes) + "}",
	signals.FrustrationMinBytes,
	pgFrustrationPhrasePattern(), // $7
	"(?:[A-Za-z]{2,}[^A-Za-z]+){" + strconv.Itoa(pgLongWordRun) + "}",
	pgLongWordRun,
	signals.FrustrationCapsMinWords, // $10
	signals.FrustrationCapsRatio,
	pgExactCapsMaxBytes,
}

// pgOtherPromptRunePattern matches a rune that is neither ASCII nor in
// signals.PlainPromptSymbols. It writes the range ends as literal runes,
// which POSIX ARE and RE2 read alike; their escapes for non-ASCII runes
// differ. NUL cannot be in PostgreSQL text, and the class leaves it out, so
// a CockroachDB string with NUL goes to Go.
func pgOtherPromptRunePattern() string {
	var b strings.Builder
	b.WriteString("[^\x01-\x7f")
	add := func(lo, hi, stride rune) {
		if stride == 1 {
			b.WriteRune(lo)
			b.WriteByte('-')
			b.WriteRune(hi)
			return
		}
		for r := lo; r <= hi; r += stride {
			b.WriteRune(r)
		}
	}
	for _, r := range signals.PlainPromptSymbols.R16 {
		add(rune(r.Lo), rune(r.Hi), rune(r.Stride))
	}
	for _, r := range signals.PlainPromptSymbols.R32 {
		add(rune(r.Lo), rune(r.Hi), rune(r.Stride))
	}
	b.WriteByte(']')
	return b.String()
}

// pgFrustrationPhrasePattern translates signals.FrustrationPhrasePattern to
// match a plain prompt with its code fences stripped, before lowercasing
// and whitespace collapsing. PostgreSQL's ARE has no \b (it means
// backspace) and its word escapes follow the server locale, so each Go
// ASCII word boundary becomes an explicit non-word character or a string
// end; every phrase starts and ends with a letter, so that is the same
// test. Each phrase letter becomes its ASCII case pair, which is all (?i)
// can match in a plain prompt, and each space becomes a whitespace run,
// which normalizing collapses to that space.
func pgFrustrationPhrasePattern() string {
	const nonWord = `[^0-9A-Za-z_]`
	shapeChanged := "signals.FrustrationPhrasePattern changed shape; update pgFrustrationPhrasePattern"
	p, ok := strings.CutPrefix(signals.FrustrationPhrasePattern, "(?i)")
	prefix, rest, ok1 := strings.Cut(p, `\b(?:`)
	phrases, suffix, ok2 := strings.Cut(rest, `)\b`)
	if !ok || !ok1 || !ok2 || strings.ContainsFunc(prefix+suffix, isASCIILetter) {
		panic(shapeChanged)
	}
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(`(^|` + nonWord + `)(?:`)
	for _, r := range phrases {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteString("[" + string(r-'a'+'A') + string(r) + "]")
		case r == ' ':
			b.WriteString(pgSpaceRun)
		case r == '\'' || r == '|':
			b.WriteRune(r)
		default:
			panic(shapeChanged)
		}
	}
	b.WriteString(`)($|` + nonWord + `)`)
	b.WriteString(suffix)
	return b.String()
}

func isASCIILetter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

// pgFrustrationGoPromptsSQL is pgFrustrationPromptsSQL for a database whose
// server encoding is not UTF8: every prompt comes back with its content for
// signals.IsFrustrationMarker. It binds only the session IDs. PostgreSQL
// converts each parameter from the client encoding to the server encoding
// before the statement runs, so binding the plain-alphabet pattern, which
// holds runes such an encoding may lack, fails even when every prompt is
// ASCII.
const pgFrustrationGoPromptsSQL = `
	SELECT id, %s::text, false, ''
	FROM sessions
	WHERE id = ANY($1)
	UNION ALL
	SELECT session_id, NULL, false, content
	FROM messages
	WHERE role = 'user'
	  AND source_subtype <> 'tool_result'
	  AND NOT is_system
	  AND session_id = ANY($1)`

// pgFrustrationPrompts formats pgFrustrationPromptsSQL for a version column.
func pgFrustrationPrompts(versionColumn string) string {
	return fmt.Sprintf(pgFrustrationPromptsSQL, versionColumn)
}

// pgFrustrationQuery returns the prompt read for the sessions in ids and its
// arguments, for a database with the given server encoding.
func pgFrustrationQuery(serverEncoding, versionColumn string, ids []string) (string, []any) {
	if serverEncoding != "UTF8" {
		return fmt.Sprintf(pgFrustrationGoPromptsSQL, versionColumn), []any{ids}
	}
	return pgFrustrationPrompts(versionColumn), append([]any{ids}, pgFrustrationPromptsArgs...)
}
