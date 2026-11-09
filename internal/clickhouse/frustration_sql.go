package clickhouse

import (
	"fmt"
	"strings"
	"unicode"

	"go.kenn.io/agentsview/internal/signals"
)

// chPlainPromptSymbols are the non-ASCII runes a prompt may contain and
// still have its frustration marker decided in ClickHouse. Each is a
// symbol, punctuation, format, or private-use rune: not a letter, not
// whitespace, and unchanged by lowercasing. Over such a prompt the regular
// expressions ClickHouse runs (RE2, UTF-8, ASCII \b) see the same words,
// spaces, and boundaries signals.IsFrustrationMarker sees in Go. Prompts
// with any other non-ASCII rune go to Go. The set covers the dashes,
// quotes, arrows, check marks, box drawing, and emoji common in prompts.
var chPlainPromptSymbols = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00A1, Hi: 0x00A9, Stride: 1},
		{Lo: 0x00AB, Hi: 0x00B4, Stride: 1},
		{Lo: 0x00B6, Hi: 0x00B9, Stride: 1},
		{Lo: 0x00BB, Hi: 0x00BF, Stride: 1},
		{Lo: 0x00D7, Hi: 0x00F7, Stride: 0x20},
		{Lo: 0x200B, Hi: 0x2027, Stride: 1},
		{Lo: 0x202A, Hi: 0x202E, Stride: 1},
		{Lo: 0x2030, Hi: 0x205E, Stride: 1},
		{Lo: 0x2060, Hi: 0x206F, Stride: 1},
		{Lo: 0x2190, Hi: 0x23FF, Stride: 1},
		{Lo: 0x2500, Hi: 0x27BF, Stride: 1},
		{Lo: 0x2B00, Hi: 0x2BFF, Stride: 1},
		{Lo: 0xE000, Hi: 0xF8FF, Stride: 1},
		{Lo: 0xFE00, Hi: 0xFE0F, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x1F000, Hi: 0x1FAFF, Stride: 1},
	},
}

// chFrustrationPromptsSQL reads the prompts of the sessions in the set its
// %s names. A plain prompt, valid UTF-8 made of ASCII and
// chPlainPromptSymbols, is decided in ClickHouse: it comes back only when
// it is a marker, with empty content. Any other prompt comes back with its
// content for signals.IsFrustrationMarker. Prompts are the rows the
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
	chOtherPromptRunePattern(),
	signals.FrustrationMinBytes,
	signals.FrustrationPhrasePattern,
	signals.FrustrationCapsMinWords,
	signals.FrustrationCapsMinWords,
	signals.FrustrationCapsRatio,
}

// chOtherPromptRunePattern matches a rune that is neither ASCII nor in
// chPlainPromptSymbols.
func chOtherPromptRunePattern() string {
	var b strings.Builder
	b.WriteString(`[^\x00-\x7F`)
	add := func(lo, hi, stride uint32) {
		if stride == 1 {
			fmt.Fprintf(&b, `\x{%X}-\x{%X}`, lo, hi)
			return
		}
		for r := lo; r <= hi; r += stride {
			fmt.Fprintf(&b, `\x{%X}`, r)
		}
	}
	for _, r := range chPlainPromptSymbols.R16 {
		add(uint32(r.Lo), uint32(r.Hi), uint32(r.Stride))
	}
	for _, r := range chPlainPromptSymbols.R32 {
		add(r.Lo, r.Hi, r.Stride)
	}
	b.WriteString(`]`)
	return b.String()
}
