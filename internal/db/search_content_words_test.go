package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedWordSearch stores one session per entry, newest first, each holding one
// assistant message. A tool result is attached when result is set.
func seedWordSearch(t *testing.T, d *DB, project string, entries ...[3]string) {
	t.Helper()
	for i, e := range entries {
		id, content, result := e[0], e[1], e[2]
		ended := "2026-05-20T12:00:" + itoa(59-i) + "Z"
		insertSession(t, d, id, project, func(s *Session) {
			s.Agent = "claude"
			s.UserMessageCount = 2
			s.EndedAt = &ended
		})
		msg := Message{SessionID: id, Role: "assistant", Content: content, Timestamp: ended}
		if result != "" {
			msg.ToolCalls = []ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "tu-" + id, ResultContent: result}}
		}
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), id, []Message{msg}))
	}
}

func TestSearchContentStemmedMatchSnippet(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj", [3]string{"stemmed", strings.Repeat("filler ", 100) + "connecting succeeded", ""})
	for _, mode := range []string{"fts", "hybrid"} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "connection unrelated", Mode: mode, KeywordOnly: mode == "hybrid"})
		require.NoError(t, err)
		require.Len(t, page.Matches, 1, "mode %q", mode)
		assert.Contains(t, page.Matches[0].Snippet, "connecting", "mode %q", mode)
	}
}

func TestSearchContentWordsFallsBackWhenImportDropsFTS(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj", [3]string{"exact", "invoice rounding decision", ""})
	f := ContentSearchFilter{Pattern: "invoice rounding decision", Limit: 50, Sources: []string{"messages"}}
	q, ok := contentWordsQuery(f.Pattern)
	require.True(t, ok)
	require.NoError(t, d.DropFTS(t.Context()))

	page, err := d.searchContentWordsOrSubstring(t.Context(), f, q)
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "exact", page.Matches[0].SessionID)
	assert.Contains(t, page.Matches[0].Snippet, f.Pattern)
}

type wordHit struct {
	session, location string
	words             int
}

func wordHits(page ContentSearchPage) []wordHit {
	out := make([]wordHit, len(page.Matches))
	for i, m := range page.Matches {
		out[i] = wordHit{m.SessionID, m.Location, m.MatchedWords}
	}
	return out
}

func TestSearchContentCurlyQuotedToolResult(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj",
		[3]string{"tool", "ran the report", "invoice rounding decision recorded"},
	)
	page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: `“invoice rounding decision”`})
	require.NoError(t, err)
	assert.Equal(t, []wordHit{{"tool", "tool_result", 0}}, wordHits(page))
}

func TestSearchContentKeepsSubstringSearch(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj",
		[3]string{"embedded", "prefixrounding happened", ""},
		[3]string{"words", "rounding of invoices", "invoices need rounding"},
		[3]string{"curly-tool", "no match", "found “invoices need” here"},
	)
	for _, tc := range []struct {
		name   string
		filter ContentSearchFilter
		want   []wordHit
	}{
		{
			"single word",
			ContentSearchFilter{Pattern: "rounding"},
			[]wordHit{{"embedded", "message", 0}, {"words", "message", 0}, {"words", "tool_result", 0}},
		},
		{
			"no messages source",
			ContentSearchFilter{Pattern: "invoices need", Sources: []string{"tool_result"}},
			[]wordHit{{"words", "tool_result", 0}, {"curly-tool", "tool_result", 0}},
		},
		{
			"curly quotes without messages are literal",
			ContentSearchFilter{Pattern: `“invoices need”`, Sources: []string{"tool_result"}},
			[]wordHit{{"curly-tool", "tool_result", 0}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := d.SearchContent(t.Context(), tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.want, wordHits(page))
		})
	}
}

func TestSearchContentExplicitFTSKeepsFTS5(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj",
		[3]string{"forms", "the build fixes missed files", ""},
		[3]string{"other", "a missing semicolon", ""},
	)
	for pattern, want := range map[string][]wordHit{
		`"fix" "missing"`:      {{"forms", "message", 0}},
		`"fix" OR "semicolon"`: {{"forms", "message", 0}, {"other", "message", 0}},
		`"fix" NOT "build"`:    {},
	} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: pattern, Mode: "fts"})
		require.NoError(t, err)
		assert.ElementsMatch(t, want, wordHits(page), pattern)
	}
}

func TestSearchContentNaturalPhrasesMatchText(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj",
		[3]string{"phrase", "rounding, of invoices decision", ""},
		[3]string{"curly", "alpha beta gamma", ""},
		[3]string{"separated", "alpha beta then gamma", ""},
	)
	seedWordSearch(t, d, "stemmed", [3]string{"stemmed", "round of invoice decision", ""})
	for _, tc := range []struct {
		pattern, mode, project string
		want                   []wordHit
	}{
		{`decision "rounding of invoices"`, "fts", "stemmed", []wordHit{}},
		{"alpha “beta gamma”", "", "proj", []wordHit{{"curly", "message", 3}}},
	} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: tc.pattern, Mode: tc.mode, Project: tc.project})
		require.NoError(t, err)
		assert.Equal(t, tc.want, wordHits(page), "%s mode %q", tc.pattern, tc.mode)
	}
}

func TestSearchContentWordPagesFollowExactTotal(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj",
		[3]string{"exact-1", "alpha beta gamma one", ""},
		[3]string{"all-1", "gamma beta alpha", ""},
		[3]string{"exact-2", "alpha beta gamma two", ""},
		[3]string{"some-1", "alpha only", ""},
		[3]string{"exact-3", "alpha beta gamma three", ""},
		[3]string{"all-2", "beta gamma alpha", ""},
		[3]string{"some-2", "beta and gamma", ""},
	)
	var paged []wordHit
	cursor := 0
	for range 10 {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "alpha beta gamma", Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		paged = append(paged, wordHits(page)...)
		if page.NextCursor == 0 {
			break
		}
		assert.Equal(t, cursor+2, page.NextCursor)
		cursor = page.NextCursor
	}
	whole, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "alpha beta gamma", Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, wordHits(whole), paged)
	sessions := make([]string, len(paged))
	for i, h := range paged {
		sessions[i] = h.session
	}
	assert.Equal(t, []string{"exact-1", "exact-2", "exact-3"}, sessions[:3])
	assert.Equal(t, []string{"all-1", "all-2"}, sessions[3:5])
	assert.Equal(t, []string{"some-2", "some-1"}, sessions[5:])
	assert.Equal(t, []int{3, 3, 2, 1}, []int{paged[3].words, paged[4].words, paged[5].words, paged[6].words})

	page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "alpha beta gamma", Limit: 2, Cursor: 7})
	require.NoError(t, err)
	assert.Empty(t, page.Matches)
	assert.Zero(t, page.NextCursor)

	t.Run("phrase plus word", func(t *testing.T) {
		d := testDB(t)
		requireFTS(t, d)
		seedWordSearch(t, d, "proj",
			[3]string{"phrase-heavy", "alpha beta alpha beta alpha beta gamma", ""},
			[3]string{"word-heavy", "alpha beta gamma gamma gamma gamma", ""},
			[3]string{"balanced", "alpha beta gamma", ""},
			[3]string{"partial", "alpha beta", ""},
			[3]string{"partial2", "alpha beta", ""},
			[3]string{"partial3", "alpha beta", ""},
			[3]string{"partial4", "alpha beta", ""},
			[3]string{"partial5", "alpha beta", ""},
		)
		whole, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: `"alpha beta" gamma`, Mode: "fts", Limit: 20})
		require.NoError(t, err)
		require.Len(t, whole.Matches, 8)
		var paged []wordHit
		cursor := 0
		for range 10 {
			page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: `"alpha beta" gamma`, Mode: "fts", Limit: 2, Cursor: cursor})
			require.NoError(t, err)
			paged = append(paged, wordHits(page)...)
			if page.NextCursor == 0 {
				break
			}
			assert.Equal(t, cursor+2, page.NextCursor)
			cursor = page.NextCursor
		}
		assert.Equal(t, wordHits(whole), paged)

		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: `"alpha beta" gamma`, Mode: "fts", Limit: 2, Cursor: 8})
		require.NoError(t, err)
		assert.Empty(t, page.Matches)
		assert.Zero(t, page.NextCursor)
	})
}

func TestSearchContentWordTierOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		filter  ContentSearchFilter
		entries [][3]string
		want    []wordHit
		update  string
	}{
		{"default", ContentSearchFilter{Pattern: "alpha beta missing"}, [][3]string{{"newer", "alpha beta long", ""}, {"older", "alpha beta", ""}}, []wordHit{{"newer", "message", 2}, {"older", "message", 2}}, ""},
		{"fts", ContentSearchFilter{Pattern: "alpha beta missing", Mode: "fts"}, [][3]string{{"older", "alpha beta", ""}, {"newer", "alpha beta filler filler filler filler filler filler filler filler", ""}}, []wordHit{{"older", "message", 2}, {"newer", "message", 2}}, `UPDATE sessions SET ended_at = CASE id WHEN 'older' THEN '2026-01-01T00:00:00Z' ELSE '2026-06-01T00:00:00Z' END`},
		{"hybrid", ContentSearchFilter{Pattern: "alpha beta missing", Mode: "hybrid", KeywordOnly: true}, [][3]string{{"older", "alpha beta", ""}, {"newer", "alpha beta filler filler filler filler filler filler filler filler", ""}}, []wordHit{{"older", "message", 2}, {"newer", "message", 2}}, `UPDATE sessions SET ended_at = CASE id WHEN 'older' THEN '2026-01-01T00:00:00Z' ELSE '2026-06-01T00:00:00Z' END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			requireFTS(t, d)
			seedWordSearch(t, d, "proj", tc.entries...)
			if tc.update != "" {
				_, err := d.getWriter().Exec(t.Context(), tc.update)
				require.NoError(t, err)
			}
			page, err := d.SearchContent(t.Context(), tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.want, wordHits(page))
		})
	}
	t.Run("fts-ordinal", func(t *testing.T) {
		d := testDB(t)
		requireFTS(t, d)
		for _, tc := range []struct {
			id      string
			ordinal int
		}{{"later", 5}, {"earlier", 1}} {
			insertSession(t, d, tc.id, "proj", func(s *Session) { s.UserMessageCount = 2 })
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: tc.id, Ordinal: tc.ordinal, Role: "user", Content: "alpha beta"}}))
		}
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "alpha beta", Mode: "fts"})
		require.NoError(t, err)
		assert.Equal(t, []wordHit{{"earlier", "message", 2}, {"later", "message", 2}}, wordHits(page))
	})
}

func TestSearchContentWordPagesWithFilteredTiers(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "other", [3]string{"excluded-1", "alpha beta", ""}, [3]string{"excluded-2", "alpha beta", ""})
	seedWordSearch(t, d, "chosen", [3]string{"partial-2", "alpha beta", ""}, [3]string{"partial-1", "gamma", ""})
	for _, tc := range []struct {
		cursor int
		want   []wordHit
	}{
		{0, []wordHit{{"partial-2", "message", 2}, {"partial-1", "message", 1}}},
		{1, []wordHit{{"partial-1", "message", 1}}},
	} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "alpha beta gamma", Project: "chosen", Limit: 2, Cursor: tc.cursor})
		require.NoError(t, err)
		assert.Equal(t, tc.want, wordHits(page), "cursor %d", tc.cursor)
	}
}
