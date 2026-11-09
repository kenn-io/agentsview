package duckdb

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/signals"
)

// duckASCIISpacePattern matches a run of the ASCII runes unicode.IsSpace
// accepts.
const duckASCIISpacePattern = `[\t\n\v\f\r ]+`

// duckFrustrationPromptsSQL reads the prompts of the sessions in the list
// it takes as its third parameter. A plain prompt, made of ASCII and
// signals.PlainPromptSymbols, is decided in DuckDB: it comes back only when
// it is a marker, with empty content. Any other prompt comes back with its
// content for signals.IsFrustrationMarker. Prompts are the rows the signals
// scan reads: user rows that are neither system rows nor tool results.
// DuckDB stores only valid UTF-8, so plainness depends on the runes alone;
// strlen equals length only for ASCII text, which skips the symbol check.
//
// The marker expression decides what signals.IsFrustrationMarker decides,
// in the same order, over the text left after code fences become a space:
//
//  1. The normalized text is lowercased, has each whitespace run collapsed
//     to one space, and is trimmed; it must be FrustrationMinBytes long.
//     strlen counts bytes, as Go's len does. The first 64 characters are
//     normalized first. Their normalized form is a prefix of the whole
//     prompt's, so when it is long enough the whole prompt is too, and a
//     long prompt skips collapsing all of its whitespace.
//  2. FrustrationPhrasePattern matches the stripped text with each space
//     widened to a whitespace run. Over a plain prompt that is the same
//     match as over the normalized text: (?i) covers lowercasing, and
//     collapsing whitespace moves no word boundary.
//  3. Words are the ASCII letter runs, which in a plain prompt are exactly
//     Go's unicode.IsLetter runs. regexp_extract_all lists the words of two
//     or more letters, and a lambda counts them and the capitalized ones
//     among them. Go's minimum on all words is implied: every long word is
//     a word.
//
// Patterns are parameters because the Quack read path renders parameters
// into the SQL text by replacing each question mark, and the patterns
// contain question marks. ORDER BY ends the pipeline that evaluates the
// marker expression; without it the streamed result runs that pipeline on
// one DuckDB thread as the reader pulls rows.
var duckFrustrationPromptsSQL = func() string {
	normalizedLength := func(text string) string {
		return `strlen(regexp_replace(regexp_replace(lower(` + text +
			`), ?, ' ', 'g'), '^ | $', '', 'g'))`
	}
	return fmt.Sprintf(`
	SELECT session_id, plain, CASE WHEN plain THEN '' ELSE content END
	FROM (
		SELECT session_id, content, plain,
			CASE WHEN plain AND contains(content, '%[1]s')
				THEN regexp_replace(content, ?, ' ', 'g') ELSE content END AS stripped
		FROM (
			SELECT session_id, content,
				content IS NOT NULL AND (strlen(content) = length(content)
					OR NOT regexp_matches(content, ?)) AS plain
			FROM messages
			WHERE role = 'user' AND COALESCE(source_subtype, '') <> 'tool_result'
				AND NOT is_system AND session_id IN (SELECT unnest(?))
		)
	)
	WHERE NOT plain OR CASE
		WHEN strlen(stripped) < %[2]d THEN false
		WHEN %[3]s < %[2]d AND %[4]s < %[2]d THEN false
		WHEN regexp_matches(stripped, ?) THEN true
		ELSE list_transform([regexp_extract_all(stripped, '[A-Za-z]{2,}')], lambda long:
			len(long) >= %[5]d
			AND len(list_filter(long, lambda w: w = upper(w))) / len(long) >= CAST(%[6]v AS DOUBLE)
		)[1]
		END
	ORDER BY session_id`,
		"```", signals.FrustrationMinBytes,
		normalizedLength("left(stripped, 64)"), normalizedLength("stripped"),
		signals.FrustrationCapsMinWords, signals.FrustrationCapsRatio)
}()

// duckFrustrationPromptsArgs returns the parameters of
// duckFrustrationPromptsSQL for the session IDs.
func duckFrustrationPromptsArgs(ids []string) []any {
	return []any{
		signals.CodeFencePattern,
		signals.NonPlainRunePattern(),
		ids,
		duckASCIISpacePattern,
		duckASCIISpacePattern,
		strings.ReplaceAll(signals.FrustrationPhrasePattern, " ", duckASCIISpacePattern),
	}
}
