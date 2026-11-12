package db

import (
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

func TestSearchContentRanksExactTextThenWords(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj",
		[3]string{"one-word", "an invoice arrived", ""},
		[3]string{"two-words", "we settled the rounding of invoices", ""},
		[3]string{"unrelated", "nothing to see", ""},
		[3]string{"tool", "ran the report", "invoice rounding decision recorded"},
	)
	search := func(pattern, mode string) []wordHit {
		t.Helper()
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: pattern, Mode: mode})
		require.NoError(t, err)
		return wordHits(page)
	}

	assert.Equal(t, []wordHit{
		{"tool", "tool_result", 3},
		{"two-words", "message", 2},
		{"one-word", "message", 1},
	}, search("invoice rounding decision", ""))
	assert.Equal(t, []wordHit{{"two-words", "message", 0}}, search(`"rounding of invoices"`, ""))
	assert.Equal(t, []wordHit{
		{"two-words", "message", 2},
		{"one-word", "message", 1},
	}, search("invoice rounding decision", "fts"))
}

func TestSearchContentKeepsSubstringSearch(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj",
		[3]string{"embedded", "prefixrounding happened", ""},
		[3]string{"words", "rounding of invoices", "invoices need rounding"},
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
			[]wordHit{{"words", "tool_result", 0}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := d.SearchContent(t.Context(), tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.want, wordHits(page))
		})
	}
}

func TestSearchContentToolInputKeepsQuotes(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "quoted-tool", "proj", func(s *Session) { s.UserMessageCount = 2 })
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "quoted-tool", []Message{{
		SessionID: "quoted-tool", Role: "assistant", Content: "run",
		ToolCalls: []ToolCall{{ToolName: "Bash", ToolUseID: "call", InputJSON: `{"command":"quoted phrase"}`}},
	}}))
	insertSession(t, d, "unquoted-tool", "proj", func(s *Session) { s.UserMessageCount = 2 })
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "unquoted-tool", []Message{{
		SessionID: "unquoted-tool", Role: "assistant", Content: "run",
		ToolCalls: []ToolCall{{ToolName: "Bash", ToolUseID: "other", InputJSON: `{"command":"prefix quoted phrase suffix"}`}},
	}}))
	page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: `"quoted phrase"`, Sources: []string{"tool_input"}})
	require.NoError(t, err)
	assert.Equal(t, []wordHit{{"quoted-tool", "tool_input", 0}}, wordHits(page))
}

func TestSearchContentExplicitFTSKeepsFTS5(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj",
		[3]string{"forms", "the build fixes missed files", ""},
		[3]string{"other", "a missing semicolon", ""},
	)
	for _, mode := range []string{"", "fts"} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "fix missing", Mode: mode})
		require.NoError(t, err)
		assert.Equal(t, []wordHit{{"forms", "message", 2}, {"other", "message", 1}}, wordHits(page), "mode %q", mode)
	}
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
		[3]string{"phrase", "prefixrounding of invoices decision", ""},
		[3]string{"scattered", "invoices decision and rounding", ""},
		[3]string{"arrow", "handler -> next", ""},
		[3]string{"plain", "handler only", ""},
	)
	for _, tc := range []struct {
		pattern, mode string
		want          []wordHit
	}{
		{`decision "rounding of invoices"`, "", []wordHit{{"phrase", "message", 3}}},
		{`"rounding of invoices" decision`, "fts", []wordHit{{"phrase", "message", 3}}},
		{`decision "rounding of invoices"`, "fts", []wordHit{{"phrase", "message", 3}}},
		{`handler "->"`, "", []wordHit{{"arrow", "message", 1}}},
		{`"->"`, "fts", []wordHit{{"arrow", "message", 0}}},
	} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: tc.pattern, Mode: tc.mode})
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
	assert.ElementsMatch(t, []string{"all-1", "all-2"}, sessions[3:5])
	assert.Equal(t, []string{"some-2", "some-1"}, sessions[5:])
	assert.Equal(t, []int{3, 3, 2, 1}, []int{paged[3].words, paged[4].words, paged[5].words, paged[6].words})

	page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "alpha beta gamma", Limit: 2, Cursor: 7})
	require.NoError(t, err)
	assert.Empty(t, page.Matches)
	assert.Zero(t, page.NextCursor)
}

func TestSearchContentWordsKeepFilters(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "chosen", [3]string{"in-scope", "rounding of invoices", ""})
	seedWordSearch(t, d, "other", [3]string{"out-of-scope", "rounding of invoices", ""})
	for _, mode := range []string{"", "fts"} {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "invoice rounding", Mode: mode, Project: "chosen"})
		require.NoError(t, err)
		assert.Equal(t, []wordHit{{"in-scope", "message", 2}}, wordHits(page), "mode %q", mode)
	}
}

func TestSearchContentWordTierOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		filter  ContentSearchFilter
		entries [][3]string
		want    []wordHit
	}{
		{"default", ContentSearchFilter{Pattern: "alpha beta missing"}, [][3]string{{"newer", "alpha beta long", ""}, {"older", "alpha beta", ""}}, []wordHit{{"newer", "message", 2}, {"older", "message", 2}}},
		{"fts", ContentSearchFilter{Pattern: "alpha beta missing", Mode: "fts"}, [][3]string{{"older", "alpha beta", ""}, {"newer", "alpha beta filler filler filler filler filler filler filler filler", ""}}, []wordHit{{"older", "message", 2}, {"newer", "message", 2}}},
		{"hybrid", ContentSearchFilter{Pattern: "alpha beta missing", Mode: "hybrid", KeywordOnly: true}, [][3]string{{"older", "alpha beta", ""}, {"newer", "alpha beta filler filler filler filler filler filler filler filler", ""}}, []wordHit{{"older", "message", 0}, {"newer", "message", 0}}},
		{"hybrid-full", ContentSearchFilter{Pattern: "alpha beta", Mode: "hybrid", KeywordOnly: true}, [][3]string{{"older", "alpha beta", ""}, {"newer", "alpha beta filler filler filler filler filler filler filler filler", ""}}, []wordHit{{"older", "message", 0}, {"newer", "message", 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			requireFTS(t, d)
			seedWordSearch(t, d, "proj", tc.entries...)
			if tc.name != "default" {
				_, err := d.getWriter().Exec(t.Context(), `UPDATE sessions SET ended_at = CASE id WHEN 'older' THEN '2026-01-01T00:00:00Z' ELSE '2026-06-01T00:00:00Z' END`)
				require.NoError(t, err)
			}
			page, err := d.SearchContent(t.Context(), tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.want, wordHits(page))
			if tc.filter.Mode != "hybrid" {
				tc.filter.Limit = 1
				first, err := d.SearchContent(t.Context(), tc.filter)
				require.NoError(t, err)
				assert.Equal(t, tc.want[:1], wordHits(first))
				require.Equal(t, 1, first.NextCursor)
				tc.filter.Cursor = first.NextCursor
				second, err := d.SearchContent(t.Context(), tc.filter)
				require.NoError(t, err)
				assert.Equal(t, tc.want[1:], wordHits(second))
				assert.Zero(t, second.NextCursor)
			}
		})
	}
}

func TestContentWordPageSkipsPartialTableForFullPage(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "proj", [3]string{"full-1", "gamma beta alpha", ""}, [3]string{"full-2", "beta alpha gamma", ""})
	q, ok := contentWordsQuery("alpha beta gamma")
	require.True(t, ok)
	for _, mode := range []string{"", "fts", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			snapshot, err := d.BeginMessageSnapshot(t.Context())
			require.NoError(t, err)
			defer snapshot.Close()
			f := ContentSearchFilter{Pattern: "alpha beta gamma", Mode: mode}
			where, args := lexicalMessageWhere(f)
			page, err := selectContentWordPage(t.Context(), snapshot.tx, f, q, where, args, 0, 2)
			require.NoError(t, err)
			assert.Len(t, page, 2)
			var count int
			require.NoError(t, snapshot.tx.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_temp_master WHERE name = 'content_word_hits'`).Scan(&count))
			assert.Zero(t, count)
		})
	}
}

func TestSearchContentWordPagesWithFilteredTiers(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)
	seedWordSearch(t, d, "other", [3]string{"excluded-1", "alpha beta", ""}, [3]string{"excluded-2", "alpha beta", ""})
	seedWordSearch(t, d, "chosen", [3]string{"partial-2", "alpha beta", ""}, [3]string{"partial-1", "gamma", ""})
	for _, mode := range []string{"", "fts"} {
		for _, tc := range []struct {
			cursor int
			want   []wordHit
		}{
			{0, []wordHit{{"partial-2", "message", 2}, {"partial-1", "message", 1}}},
			{1, []wordHit{{"partial-1", "message", 1}}},
		} {
			page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "alpha beta gamma", Project: "chosen", Mode: mode, Limit: 2, Cursor: tc.cursor})
			require.NoError(t, err)
			assert.Equal(t, tc.want, wordHits(page), "mode %s cursor %d", mode, tc.cursor)
		}
	}
}
