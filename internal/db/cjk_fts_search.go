package db

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"go.kenn.io/kit/search/lexical"
)

type messageFTSQuery struct {
	table       string
	match       string
	plain       string
	snippetTerm string
}

func (db *DB) prepareMessageFTSQuery(
	ctx context.Context, raw string,
) (messageFTSQuery, error) {
	trimmed := strings.TrimSpace(raw)
	prepared := PrepareFTSQuery(trimmed)
	query := messageFTSQuery{
		table: "messages_fts",
		match: prepared,
		plain: StripFTSQuotes(prepared),
	}
	if prepared == "" || !containsCJK(trimmed) || !db.HasCJKFTS(ctx) {
		return query, nil
	}

	query.table = "messages_cjk_fts"
	if strings.HasPrefix(trimmed, `"`) {
		// A leading quote is the established opt-in for an explicit FTS5
		// expression. Preserve phrases, operators, and grouping verbatim.
		query.match = prepared
		return query, nil
	}
	if strings.ContainsFunc(trimmed, func(r rune) bool {
		return unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
	}) {
		// Jieba turns kana and Hangul queries into independent character
		// matches. Keep each whitespace-delimited term as a phrase in the
		// character index instead. Han-only queries remain Chinese-segmented;
		// callers can quote Japanese kanji phrases to preserve their order.
		return query, nil
	}

	conn, err := db.getReader().Conn(ctx)
	if err != nil {
		return messageFTSQuery{}, fmt.Errorf(
			"acquiring CJK FTS query connection: %w", err,
		)
	}
	defer conn.Close()

	simpleFTSJiebaMu.Lock()
	err = conn.QueryRowContext(
		ctx, "SELECT jieba_query(?, 0)", trimmed,
	).Scan(&query.match)
	simpleFTSJiebaMu.Unlock()
	if err != nil {
		return messageFTSQuery{}, fmt.Errorf(
			"preparing CJK FTS query: %w", err,
		)
	}
	if strings.TrimSpace(query.match) == "" {
		return messageFTSQuery{}, &SearchInputError{
			Msg: "search: CJK FTS query is empty after tokenization",
		}
	}
	// jieba_query joins terms with AND and may add an unquoted prefix '*'.
	// Keep the first literal term to locate snippets when the raw query splits.
	firstTerm, _, _ := strings.Cut(query.match, " AND ")
	query.snippetTerm = StripFTSQuotes(strings.TrimSuffix(firstTerm, "*"))
	return query, nil
}

// cjkScripts routes text containing Han, Hangul, Hiragana, or Katakana to the
// CJK index. Only its script detection is used here; query preparation stays
// with the simple tokenizer's jieba_query.
var cjkScripts = lexical.EnableCharacterPhrase()

func containsCJK(text string) bool {
	return cjkScripts.IndexFor(text) == lexical.IndexCJK
}
