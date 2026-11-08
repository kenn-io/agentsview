package db

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type substringHit struct {
	Location string
	Ordinal  int
}

func substringHits(t *testing.T, d *DB, f ContentSearchFilter) []substringHit {
	t.Helper()
	page, err := d.SearchContent(t.Context(), f)
	require.NoError(t, err, "%s %q", f.Mode, f.Pattern)
	hits := []substringHit{}
	for _, m := range page.Matches {
		hits = append(hits, substringHit{m.Location, m.Ordinal})
	}
	return hits
}

// countMessagesForSubstringIndex sets a session's message count high enough
// that searches over it take the index for patterns that list a few
// messages.
func countMessagesForSubstringIndex(t *testing.T, d *DB, id string) {
	t.Helper()
	_, err := d.getWriter().Exec(t.Context(), `UPDATE sessions SET message_count = 100 WHERE id = ?`, id)
	require.NoError(t, err)
}

func requireSubstringIndexIntact(t *testing.T, d *DB) {
	t.Helper()
	for _, table := range []string{"message_trigram_fts", "thinking_trigram_fts"} {
		_, err := d.getWriter().Exec(t.Context(),
			fmt.Sprintf("INSERT INTO %[1]s(%[1]s) VALUES('integrity-check')", table))
		require.NoError(t, err, table)
	}
}

// The index only narrows the rows LIKE checks, so every search must return
// what a full scan returns: ASCII case folding, non-ASCII case kept
// distinct, LIKE wildcards and quotes taken literally, patterns too short
// for the index, patterns longer than the trigram cap, and thinking text.
func TestSearchContentSubstringIndexMatchesScan(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "Please check the Deploy-Window settings"},
		{"assistant", `Done: 100% complete_now and "ship it" twice`},
		{"user", "\u00dcber alles"},
		{"assistant", "\u00fcber alles"},
		{"user", "ab cd"},
	})
	insertMessages(t, d, Message{
		SessionID: "s1", Ordinal: 5, Role: "assistant", Content: "answer",
		ThinkingText: "private plan mentions zebra-crossing",
		Timestamp:    "2026-05-20T12:00:05Z",
	})
	countMessagesForSubstringIndex(t, d, "s1")
	for _, pattern := range []string{"deploy-window", "zebra-crossing"} {
		candidates, err := d.substringCandidates(t.Context(), ContentSearchFilter{Pattern: pattern}, pattern)
		require.NoError(t, err)
		require.NotEmpty(t, candidates.message.sql, pattern)
		require.NotEmpty(t, candidates.thinking.sql, pattern)
	}

	patterns := map[string][]substringHit{
		"deploy-window":  {{"message", 0}},
		"100% complete_": {{"message", 1}},
		`"ship it"`:      {{"message", 1}},
		"\u00fcber":      {{"message", 3}},
		"\u00dcBER":      {{"message", 2}},
		"ab":             {{"message", 4}},
		"zebra-crossing": {{"thinking", 5}},
		`Done: 100% complete_now and "ship it" twice`: {{"message", 1}},
		"nothing-here": {},
	}
	indexed := map[string][]substringHit{}
	for pattern, want := range patterns {
		got := substringHits(t, d, ContentSearchFilter{Pattern: pattern, Limit: 50})
		assert.Equal(t, want, got, "substring %q", pattern)
		indexed["substring "+pattern] = got
		indexed["regex "+pattern] = substringHits(t, d,
			ContentSearchFilter{Pattern: regexQuoteForTest(pattern), Mode: "regex", Limit: 50})
	}

	for _, stmt := range dropSubstringIndexSQL {
		_, err := d.getWriter().Exec(t.Context(), stmt)
		require.NoError(t, err, stmt)
	}
	require.False(t, d.hasSubstringIndex(t.Context()))
	for pattern := range patterns {
		assert.Equal(t, indexed["substring "+pattern],
			substringHits(t, d, ContentSearchFilter{Pattern: pattern, Limit: 50}), "substring %q", pattern)
		assert.Equal(t, indexed["regex "+pattern],
			substringHits(t, d, ContentSearchFilter{Pattern: regexQuoteForTest(pattern), Mode: "regex", Limit: 50}),
			"regex %q", pattern)
	}
}

func regexQuoteForTest(pattern string) string {
	out := make([]rune, 0, len(pattern))
	for _, r := range pattern {
		switch r {
		case '\\', '.', '+', '*', '?', '(', ')', '|', '[', ']', '{', '}', '^', '$':
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}

// Inserts, edits to message, dialogue, and thinking text, transcript
// replacement, and session deletion must each keep the index equal to the
// stored text; a missing entry would hide a real match.
func TestSubstringIndexFollowsMessageWrites(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "original request"},
		{"assistant", "original answer"},
	})
	countMessagesForSubstringIndex(t, d, "s1")
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "original", Limit: 50}), 2)

	for _, stmt := range []string{
		`UPDATE messages SET content = 'rewritten request' WHERE session_id = 's1' AND ordinal = 0`,
		`UPDATE messages SET dialogue_text = 'projected answer' WHERE session_id = 's1' AND ordinal = 1`,
		`UPDATE messages SET thinking_text = 'quiet reasoning' WHERE session_id = 's1' AND ordinal = 1`,
	} {
		_, err := d.getWriter().Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	requireSubstringIndexIntact(t, d)
	assert.Equal(t, []substringHit{{"message", 0}},
		substringHits(t, d, ContentSearchFilter{Pattern: "rewritten", Limit: 50}))
	assert.Equal(t, []substringHit{{"message", 1}},
		substringHits(t, d, ContentSearchFilter{Pattern: "projected", Limit: 50}))
	assert.Equal(t, []substringHit{{"thinking", 1}},
		substringHits(t, d, ContentSearchFilter{Pattern: "quiet reasoning", Limit: 50}))
	assert.Empty(t, substringHits(t, d, ContentSearchFilter{Pattern: "original", Limit: 50}))

	require.NoError(t, d.ReplaceSessionMessages(ctx, "s1", []Message{
		userMsg("s1", 0, "replacement prompt"),
		{SessionID: "s1", Ordinal: 1, Role: "assistant", Content: "replacement reply", ThinkingText: "fresh reasoning"},
	}))
	requireSubstringIndexIntact(t, d)
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "replacement", Limit: 50}), 2)
	assert.Equal(t, []substringHit{{"thinking", 1}},
		substringHits(t, d, ContentSearchFilter{Pattern: "fresh reasoning", Limit: 50}))

	require.NoError(t, d.DeleteSession(ctx, "s1"))
	requireSubstringIndexIntact(t, d)
	var rows int
	require.NoError(t, d.getReader().QueryRow(ctx,
		`SELECT count(*) FROM message_trigram_fts WHERE message_trigram_fts MATCH '"rep" AND "lac"'`).Scan(&rows))
	assert.Zero(t, rows)
}

// An archive from before the index gets it on its next writable open, and a
// bulk import that drops the search indexes falls back to scanning until it
// rebuilds them.
func TestSubstringIndexBuiltForExistingArchiveAndBulkImport(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := Open(ctx, path)
	require.NoError(t, err)
	for _, stmt := range dropSubstringIndexSQL {
		_, err := d.getWriter().Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "written before the upgrade"},
		{"assistant", "kept answer"},
	})
	require.NoError(t, d.Close())

	d, err = Open(ctx, path)
	require.NoError(t, err)
	defer d.Close()
	require.True(t, d.hasSubstringIndex(ctx))
	countMessagesForSubstringIndex(t, d, "s1")
	requireSubstringIndexIntact(t, d)
	assert.Equal(t, []substringHit{{"message", 0}},
		substringHits(t, d, ContentSearchFilter{Pattern: "before the upgrade", Limit: 50}))

	require.NoError(t, d.DropFTS(ctx))
	require.False(t, d.hasSubstringIndex(ctx))
	insertMessages(t, d, asstMsg("s1", 2, "imported while dropped"))
	assert.Equal(t, []substringHit{{"message", 2}},
		substringHits(t, d, ContentSearchFilter{Pattern: "while dropped", Limit: 50}))

	require.NoError(t, d.RebuildFTS(ctx))
	require.True(t, d.hasSubstringIndex(ctx))
	requireSubstringIndexIntact(t, d)
	assert.Equal(t, []substringHit{{"message", 2}},
		substringHits(t, d, ContentSearchFilter{Pattern: "while dropped", Limit: 50}))
}

// A pattern the index lists for few messages drives the search from the
// index; one it lists for more than a quarter of the scope's messages, and
// patterns or scopes the index cannot help, keep the scan.
func TestSubstringCandidatesOnlyForSelectivePatterns(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.MessageCount = 8
		s.UserMessageCount = 4
	})
	for i := range 8 {
		content := "common words here"
		if i == 3 {
			content = "rare-token here"
		}
		insertMessages(t, d, asstMsg("s1", i, content))
	}
	candidates := func(f ContentSearchFilter) substringCandidateFilters {
		t.Helper()
		got, err := d.substringCandidates(ctx, f, f.Pattern)
		require.NoError(t, err)
		return got
	}

	rare := candidates(ContentSearchFilter{Pattern: "rare-token"})
	require.NotEmpty(t, rare.message.sql)
	rows, err := d.getReader().Query(ctx,
		"EXPLAIN QUERY PLAN SELECT m.id FROM messages m WHERE m.content LIKE '%rare-token%'"+rare.message.sql,
		rare.message.args...)
	require.NoError(t, err)
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan.WriteString(detail + "\n")
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, plan.String(), "SEARCH m USING INTEGER PRIMARY KEY (rowid=?)")
	assert.Contains(t, plan.String(), "VIRTUAL TABLE INDEX 0:M")

	assert.Empty(t, candidates(ContentSearchFilter{Pattern: "common"}).message.sql)
	assert.Empty(t, candidates(ContentSearchFilter{Pattern: "ra"}).message.sql)
	assert.Empty(t, candidates(ContentSearchFilter{Pattern: "rare-token", SessionID: "s1"}).message.sql)
	assert.Equal(t, []substringHit{{"message", 3}},
		substringHits(t, d, ContentSearchFilter{Pattern: "rare-token", Limit: 50}))
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "common", Limit: 50}), 7)
}

// An archive import drops the index with the other FTS tables. A search that
// chose index filters just before the drop must rerun as a scan instead of
// failing on the missing tables.
func TestSubstringSearchScansWhenImportDropsIndexMidSearch(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "where is the rare-token"},
		{"assistant", "nothing to see"},
	})
	countMessagesForSubstringIndex(t, d, "s1")
	f := ContentSearchFilter{
		Pattern: "rare-token", Limit: 50,
		Sources: []string{"messages", "thinking", "tool_input", "tool_result"},
	}

	var usedIndex []bool
	page, err := searchWithSubstringCandidates(ctx, d, f, f.Pattern,
		func(candidates substringCandidateFilters) (ContentSearchPage, error) {
			usedIndex = append(usedIndex, candidates.message.sql != "")
			if len(usedIndex) == 1 {
				require.NoError(t, d.DropFTS(ctx))
			}
			return d.substringSearchPage(ctx, f, candidates)
		})
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false}, usedIndex)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "message", page.Matches[0].Location)
	assert.Equal(t, 0, page.Matches[0].Ordinal)
}

// Cached serving queries open their snapshot during QueryContext and can finish
// against an index dropped afterwards. Cold read-only queries take their first
// step during Next and must retry as a scan when the index has disappeared.
func TestRegexSearchScansWhenImportDropsIndexBeforeFirstRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cold      bool
		wantIndex []bool
	}{
		{"serving", false, []bool{true}},
		{"cold read-only", true, []bool{true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := testDB(t)
			ctx := t.Context()
			seedSearchSession(t, archive, "s1", "proj", [][2]string{
				{"user", "where is the rare-token"},
				{"assistant", "nothing to see"},
			})
			countMessagesForSubstringIndex(t, archive, "s1")
			d := archive
			if tc.cold {
				reader, err := OpenReadOnly(ctx, archive.Path())
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reader.Close()) })
				d = reader
			}
			f := ContentSearchFilter{
				Pattern: `rare-tok(en)`, Mode: "regex", Limit: 50,
				Sources: []string{"messages", "thinking", "tool_input", "tool_result"},
			}
			re, err := regexp.Compile(f.Pattern)
			require.NoError(t, err)
			lit := LiteralPrefix(f.Pattern)

			var usedIndex []bool
			matches, err := searchWithSubstringCandidates(ctx, d, f, lit,
				func(candidates substringCandidateFilters) ([]ContentMatch, error) {
					usedIndex = append(usedIndex, candidates.message.sql != "")
					rows, err := d.regexCandidateRows(ctx, f, lit, candidates)
					if err != nil {
						return nil, err
					}
					if len(usedIndex) == 1 {
						require.NoError(t, archive.DropFTS(ctx))
					}
					return regexMatches(f, re, rows)
				})
			require.NoError(t, err)
			assert.Equal(t, tc.wantIndex, usedIndex)
			require.Len(t, matches, 1)
			assert.Equal(t, "message", matches[0].Location)
			assert.Equal(t, 0, matches[0].Ordinal)
		})
	}
}

// Terms search prefilters sessions through the index only for terms it
// lists selectively, and returns the same exchanges as the scan.
func TestSearchContentTermsIndexMatchesScan(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for i, msgs := range [][2]string{
		{"Deploy-Window opened", "the beta setting is required"},
		{"alpha%_ marker here", "beta follows"},
		{"nothing relevant", "still nothing"},
		{"Über alles", "über reply"},
	} {
		id := fmt.Sprintf("terms-%d", i)
		seedSearchSession(t, d, id, "proj", [][2]string{{"user", msgs[0]}, {"assistant", msgs[1]}})
		countMessagesForSubstringIndex(t, d, id)
	}
	filter := func(pattern string) ContentSearchFilter {
		return ContentSearchFilter{Pattern: pattern, Mode: "terms", Scope: "all", Limit: 50}
	}
	indexed, err := d.termsIndexQueries(ctx, filter("deploy-window beta"), []string{"deploy-window", "beta", "ab"})
	require.NoError(t, err)
	assert.Contains(t, indexed, "deploy-window")
	assert.NotContains(t, indexed, "ab")

	patterns := map[string][]substringHit{
		"deploy-window beta": {{"message", 0}},
		"alpha%_ beta":       {{"message", 0}},
		"über":               {{"message", 0}},
		"missing beta":       {},
	}
	before := map[string][]substringHit{}
	for pattern, want := range patterns {
		got := substringHits(t, d, filter(pattern))
		assert.Equal(t, want, got, pattern)
		before[pattern] = got
	}
	for _, stmt := range dropSubstringIndexSQL {
		_, err := d.getWriter().Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	for pattern := range patterns {
		assert.Equal(t, before[pattern], substringHits(t, d, filter(pattern)), pattern)
	}
}

// Terms search gets the same rerun as substring search when an import drops
// the index between choosing the index prefilter and running the query.
func TestTermsSearchScansWhenImportDropsIndexMidSearch(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "where is the rare-token"},
		{"assistant", "nothing to see"},
	})
	countMessagesForSubstringIndex(t, d, "s1")
	f := ContentSearchFilter{Pattern: "rare-token", Mode: "terms", Scope: "all", Limit: 50}
	terms := ParseContentSearchTerms(f.Pattern)

	var usedIndex []bool
	page, err := searchWithTermsIndex(ctx, d, f, terms,
		func(indexed map[string]string) (ContentSearchPage, error) {
			usedIndex = append(usedIndex, len(indexed) > 0)
			if len(usedIndex) == 1 {
				require.NoError(t, d.DropFTS(ctx))
			}
			return d.termsSearchPage(ctx, f, terms, indexed)
		})
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false}, usedIndex)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "message", page.Matches[0].Location)
	assert.Equal(t, 0, page.Matches[0].Ordinal)
}
