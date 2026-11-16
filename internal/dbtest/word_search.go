package dbtest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// SeedWordSearch creates exact, partial, and nonmatching search results.
func SeedWordSearch(t *testing.T, d *db.DB) {
	t.Helper()
	for i, entry := range []struct{ id, text, result string }{
		{"exact", "invoice rounding decision", ""},
		{"two", "rounding of invoices", ""},
		{"phrase", "quarterly; forecast arrived", ""},
		{"case", "ORBIT LEDGER SWIFT", ""},
		{"one", "an invoice arrived", ""},
		{"near", "an xinvoice arrived", ""},
		{"other", "nothing relevant", ""},
		{"tool", "ran the report", "invoice rounding decision recorded"},
	} {
		ended := fmt.Sprintf("2026-05-20T12:00:%02dZ", 59-i)
		SeedSession(t, d, entry.id, "word-search", func(s *db.Session) {
			s.EndedAt = &ended
			s.UserMessageCount = 2
		})
		m := db.Message{SessionID: entry.id, Role: "assistant", Content: entry.text, Timestamp: ended}
		if entry.result != "" {
			m.ToolCalls = []db.ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "word-tool", ResultContent: entry.result}}
		}
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), entry.id, []db.Message{m}))
	}
	for i, entry := range []struct{ id, project, text string }{
		{"star-both", "word-star", "invoice* rounding"},
		{"star-partial", "word-star", "rounding"},
		{"literal-star", "word-literal-star", "invoice*"},
		{"plain-invoice", "word-literal-star", "invoice"},
		{"literal-not", "word-literal-not", `NOT "invoice"`},
		{"plain-not-invoice", "word-literal-not", "invoice"},
		{"unicode-suffix", "word-unicode", "invoice中"},
		{"unicode-prefix", "word-unicode", "中invoice"},
		{"unicode-phrase", "word-unicode", "quarterly中forecast"},
		{"separator", "word-unicode", "invoice_ quarterly; forecast"},
		{"both-invalid", "word-unicode-all", "invoice中 rounding"},
		{"both-number", "word-unicode-all", "invoice١ rounding"},
		{"both-valid", "word-unicode-all", "invoice_ rounding"},
		{"compound", "word-compound", "invoices rounding decided"},
		{"accent", "word-accent", "café"},
	} {
		SeedSession(t, d, entry.id, entry.project, func(s *db.Session) {
			s.UserMessageCount = 2
			if entry.project == "word-unicode-all" {
				ended := fmt.Sprintf("2026-05-20T12:00:%02dZ", 59-i)
				s.EndedAt = &ended
			}
		})
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), entry.id, []db.Message{{
			SessionID: entry.id, Role: "assistant", Content: entry.text,
		}}))
	}
	SeedSession(t, d, "long", "word-snippet", func(s *db.Session) { s.UserMessageCount = 2 })
	body := strings.Repeat("filler ", 80) + "invoices " + strings.Repeat("filler ", 80)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "long", []db.Message{{SessionID: "long", Role: "assistant", Content: body}}))
}

// AssertMirrorWordAccents checks the mirrors' literal accent matching.
func AssertMirrorWordAccents(t *testing.T, store db.Store) {
	t.Helper()
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		for _, tc := range []struct {
			pattern string
			want    bool
		}{{"café absentword", true}, {"cafe absentword", false}} {
			t.Run("accent/"+tc.pattern+"/"+mode, func(t *testing.T) {
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: tc.pattern, Mode: mode, Project: "word-accent", KeywordOnly: mode == "hybrid"})
				require.NoError(t, err)
				if tc.want {
					require.Len(t, page.Matches, 1)
					assert.Equal(t, "accent", page.Matches[0].SessionID)
					assert.Equal(t, 1, page.Matches[0].MatchedWords)
				} else {
					assert.Empty(t, page.Matches)
				}
			})
		}
	}
}

// AssertExplicitMirrorFTS checks the mirrors' existing literal expression handling.
func AssertExplicitMirrorFTS(t *testing.T, store db.Store, modes ...string) {
	t.Helper()
	if len(modes) == 0 {
		modes = []string{"fts", "hybrid"}
	}
	for _, tc := range []struct{ pattern, project, want string }{
		{"invoice*", "word-literal-star", "literal-star"},
		{"invoice* rounding", "word-star", "star-both"},
		{`NOT "invoice"`, "word-literal-not", "literal-not"},
	} {
		for _, mode := range modes {
			t.Run("explicit fts/"+tc.pattern+"/"+mode, func(t *testing.T) {
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
					Pattern: tc.pattern, Project: tc.project, Mode: mode, KeywordOnly: mode == "hybrid",
				})
				require.NoError(t, err)
				require.Len(t, page.Matches, 1)
				assert.Equal(t, tc.want, page.Matches[0].SessionID)
			})
		}
	}
}

// AssertWordSearch checks the user-visible ranking and paging contract.
func AssertWordSearch(t *testing.T, store db.Store) {
	t.Helper()
	t.Run("compound query term", func(t *testing.T) {
		page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "invoice_rounding absentword", Mode: "fts", Project: "word-compound"})
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		assert.Equal(t, "compound", page.Matches[0].SessionID)
		assert.Equal(t, 1, page.Matches[0].MatchedWords)
	})
	type hit struct {
		session, location string
		words             int
	}
	project := func(matches []db.ContentMatch) []hit {
		var hits []hit
		for _, m := range matches {
			hits = append(hits, hit{m.SessionID, m.Location, m.MatchedWords})
		}
		return hits
	}
	for _, tc := range []struct {
		pattern string
		words   int
	}{{"invoice absentword", 1}, {`policy "quarterly forecast"`, 2}} {
		for _, mode := range []string{"substring", "fts", "hybrid"} {
			t.Run("unicode boundary/"+tc.pattern+"/"+mode, func(t *testing.T) {
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: tc.pattern, Mode: mode, Project: "word-unicode", KeywordOnly: mode == "hybrid"})
				require.NoError(t, err)
				assert.Equal(t, []hit{{"separator", "message", tc.words}}, project(page.Matches))
			})
		}
	}
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run("unicode boundary/all words/"+mode, func(t *testing.T) {
			if mode == "hybrid" {
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "invoice rounding", Mode: mode, Project: "word-unicode-all", KeywordOnly: true, Limit: 1})
				require.NoError(t, err)
				assert.Equal(t, []hit{{"both-valid", "message", 2}}, project(page.Matches))
			} else {
				filter := db.ContentSearchFilter{Pattern: "invoice rounding", Mode: mode, Project: "word-unicode-all", Limit: 1}
				var paged []hit
				for range 3 {
					page, err := store.SearchContent(t.Context(), filter)
					require.NoError(t, err)
					paged = append(paged, project(page.Matches)...)
					filter.Cursor = page.NextCursor
				}
				assert.Equal(t, []hit{{"both-valid", "message", 2}, {"both-invalid", "message", 1}, {"both-number", "message", 1}}, paged)
			}
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "invoice rounding", Mode: mode, Project: "word-unicode-all", KeywordOnly: mode == "hybrid", Limit: 50})
			require.NoError(t, err)
			assert.Equal(t, []hit{{"both-valid", "message", 2}, {"both-invalid", "message", 1}, {"both-number", "message", 1}}, project(page.Matches))
		})
	}
	for _, mode := range []string{"fts", "hybrid"} {
		t.Run("single word matches whole words/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "invoice", Mode: mode, Project: "word-search", KeywordOnly: mode == "hybrid",
			})
			require.NoError(t, err)
			assert.Equal(t, []hit{{"exact", "message", 1}, {"two", "message", 1}, {"one", "message", 1}}, project(page.Matches))
		})
	}
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run("snippet/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "invoice missing", Mode: mode, Project: "word-snippet", KeywordOnly: mode == "hybrid"})
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Contains(t, page.Matches[0].Snippet, "invoices")
		})
	}
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			f := db.ContentSearchFilter{Pattern: "invoice rounding decision", Mode: mode, Project: "word-search", Limit: 50, KeywordOnly: mode == "hybrid"}
			want := []hit{{"exact", "message", 3}, {"two", "message", 2}, {"one", "message", 1}}
			if mode == "substring" {
				want = []hit{{"exact", "message", 3}, {"tool", "tool_result", 3}, {"two", "message", 2}, {"one", "message", 1}}
			}
			page, err := store.SearchContent(t.Context(), f)
			require.NoError(t, err)
			assert.Equal(t, want, project(page.Matches))
			if mode == "hybrid" {
				return
			}
			f.Limit = 1
			var paged []hit
			for range 5 {
				page, err := store.SearchContent(t.Context(), f)
				require.NoError(t, err)
				paged = append(paged, project(page.Matches)...)
				if page.NextCursor == 0 {
					break
				}
				f.Cursor = page.NextCursor
			}
			assert.Equal(t, want, paged)
		})
	}
	for _, tc := range []struct {
		pattern string
		want    []hit
	}{
		{`"rounding of invoices"`, []hit{{"two", "message", 0}}},
		{`policy "quarterly forecast"`, []hit{{"phrase", "message", 2}}},
		{`policy "forecast quarterly"`, nil},
		{`policy "quarterly missing"`, nil},
	} {
		page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: tc.pattern, Project: "word-search"})
		require.NoError(t, err)
		assert.Equal(t, tc.want, project(page.Matches), tc.pattern)
	}
	for _, tc := range []struct {
		name   string
		filter db.ContentSearchFilter
		want   []hit
	}{
		{
			name:   "single word remains substring search",
			filter: db.ContentSearchFilter{Pattern: "invoice", Project: "word-search", Sources: []string{"messages"}},
			want:   []hit{{"exact", "message", 0}, {"two", "message", 0}, {"one", "message", 0}, {"near", "message", 0}},
		},
		{
			name:   "word matching folds case",
			filter: db.ContentSearchFilter{Pattern: "orbit ledger swift", Mode: "fts", Project: "word-search"},
			want:   []hit{{"case", "message", 3}},
		},
		{
			name:   "non-message sources stay exact",
			filter: db.ContentSearchFilter{Pattern: "invoice rounding decision", Project: "word-search", Sources: []string{"tool_result"}},
			want:   []hit{{"tool", "tool_result", 0}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.want, project(page.Matches))
		})
	}
}
