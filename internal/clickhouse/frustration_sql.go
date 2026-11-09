package clickhouse

import "go.kenn.io/agentsview/internal/signals"

// chFrustrationPromptsSQL reads the prompts of the sessions in the set its
// %s names. A plain prompt, valid UTF-8 made of ASCII and
// signals.PlainPromptSymbols, is decided in ClickHouse: it comes back only
// when it is a marker, with empty content. Any other prompt comes back with
// its content for signals.IsFrustrationMarker. Prompts are the rows the
// signals scan reads: user rows that are neither system rows nor tool
// results.
//
// The marker expression follows signals.IsFrustrationMarker step by step.
// Code fences become a space. The normalized prompt lowercases that text,
// collapses each run of the whitespace Go's unicode.IsSpace accepts in
// ASCII, and trims the ends. Words are the ASCII letter runs, which in a
// plain prompt are exactly Go's unicode.IsLetter runs; caps words are the
// runs left after every run with a lowercase letter is blanked.
//
// Store connections read with final = 1, and FINAL over messages merges
// the content of every message in the sessions, assistant replies
// included, before any filter runs. So the query sets final = 0, and only
// the subquery sets final = 1 again, over key and filter columns, to find
// the newest version of each prompt. The outer read takes content for just
// those versions; a version is one push's row, so LIMIT 1 BY drops a block
// a retried insert wrote twice.
// It runs last, over the few rows left, because it joins the parallel
// streams into one. The explicit PREWHERE keeps ClickHouse from evaluating
// the marker expression ahead of it over every message.
const chFrustrationPromptsSQL = `
	WITH
		if(position(content, '` + "```" + `') > 0, replaceRegexpAll(content, ?, ' '), content) AS stripped,
		replaceRegexpAll(replaceRegexpAll(lower(stripped), ?, ' '), '^ | $', '') AS normalized,
		countMatches(stripped, '[A-Za-z]+') AS words,
		countMatches(stripped, '[A-Za-z]{2,}') AS long_words,
		countMatches(replaceRegexpAll(stripped, '[A-Za-z]*[a-z][A-Za-z]*', ' '), '[A-Z]{2,}') AS caps_words
	SELECT session_id, plain, if(plain, '', content)
	FROM (
		SELECT session_id, ordinal, content,
			isValidUTF8(content) AND NOT match(content, ?) AS plain
		FROM messages
		PREWHERE (session_id, ordinal, push_version) IN (
			SELECT session_id, ordinal, push_version
			FROM messages
			WHERE role = 'user' AND COALESCE(source_subtype, '') <> 'tool_result'
				AND NOT is_system AND session_id IN %[1]s
			SETTINGS final = 1)
		WHERE session_id IN %[1]s
	)
	WHERE NOT plain OR (length(normalized) >= ? AND (
		match(normalized, ?) OR (
			words >= ? AND long_words >= ? AND caps_words / long_words >= ?)))
	LIMIT 1 BY session_id, ordinal
	SETTINGS final = 0`

// chFrustrationPromptsArgs are the arguments of chFrustrationPromptsSQL.
var chFrustrationPromptsArgs = []any{
	signals.CodeFencePattern,
	// The ASCII runes unicode.IsSpace accepts.
	`[\t\n\v\f\r ]+`,
	signals.NonPlainRunePattern(),
	signals.FrustrationMinBytes,
	signals.FrustrationPhrasePattern,
	signals.FrustrationCapsMinWords,
	signals.FrustrationCapsMinWords,
	signals.FrustrationCapsRatio,
}
