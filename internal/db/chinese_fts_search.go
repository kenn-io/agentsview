package db

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type messageFTSQuery struct {
	table string
	match string
	plain string
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
	if prepared == "" || !containsCJK(trimmed) || !db.HasChineseFTS() {
		return query, nil
	}

	query.table = "messages_chinese_fts"
	if strings.HasPrefix(trimmed, `"`) {
		// A leading quote is the established opt-in for an explicit FTS5
		// expression. Preserve phrases, operators, and grouping verbatim.
		query.match = prepared
		return query, nil
	}

	conn, err := db.getReader().Conn(ctx)
	if err != nil {
		return messageFTSQuery{}, fmt.Errorf(
			"acquiring Chinese FTS query connection: %w", err,
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
			"preparing Chinese FTS query: %w", err,
		)
	}
	if strings.TrimSpace(query.match) == "" {
		return messageFTSQuery{}, &SearchInputError{
			Msg: "search: Chinese FTS query is empty after tokenization",
		}
	}
	return query, nil
}

func containsCJK(text string) bool {
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		if r == utf8.RuneError && size == 1 {
			text = text[1:]
			continue
		}
		if unicode.In(r,
			unicode.Han,
			unicode.Hangul,
			unicode.Hiragana,
			unicode.Katakana,
		) {
			return true
		}
		text = text[size:]
	}
	return false
}
