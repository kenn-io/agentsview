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
	for _, entry := range []struct{ id, project, text string }{
		{"exact-long-snippet", "word-exact-snippet", "invoice " + strings.Repeat("filler ", 80) + "invoice rounding decision"},
		{"invoice-dot", "word-punctuation", "invoice.txt"},
		{"invoice-path", "word-punctuation", "/tmp/invoice"},
		{"invoice-email", "word-punctuation", "a@invoice.example"},
		{"invoice-version", "word-punctuation", "v2.invoice"},
		{"split-punctuation", "word-split-punctuation", "invoice txt"},
		{"cjk-prefix", "word-cjk-boundary", "漢invoice"},
		{"cjk-suffix", "word-cjk-boundary", "invoice漢"},
	} {
		SeedSession(t, d, entry.id, entry.project, func(s *db.Session) { s.UserMessageCount = 2 })
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), entry.id, []db.Message{{
			SessionID: entry.id, Role: "assistant", Content: entry.text,
		}}))
	}
	SeedSession(t, d, "underscore", "word-boundary", func(s *db.Session) { s.UserMessageCount = 2 })
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "underscore", []db.Message{{SessionID: "underscore", Role: "assistant", Content: "invoice_rounding_decision"}}))
	SeedSession(t, d, "boundary-near", "word-boundary", func(s *db.Session) { s.UserMessageCount = 2 })
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "boundary-near", []db.Message{{SessionID: "boundary-near", Role: "assistant", Content: "xinvoice rounding decisionx"}}))
	SeedSession(t, d, "multiline", "word-multiline", func(s *db.Session) { s.UserMessageCount = 2 })
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "multiline", []db.Message{{SessionID: "multiline", Role: "assistant", Content: "decision\ninvoice_rounding"}}))
	SeedSession(t, d, "long", "word-snippet", func(s *db.Session) { s.UserMessageCount = 2 })
	body := strings.Repeat("filler ", 80) + "invoices " + strings.Repeat("filler ", 80)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "long", []db.Message{{SessionID: "long", Role: "assistant", Content: body}}))
}

// AssertWordSearch checks the user-visible ranking and paging contract.
func AssertWordSearch(t *testing.T, store db.Store) {
	t.Helper()
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
	for _, mode := range []string{"fts", "hybrid"} {
		t.Run("single word matches whole words/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "invoice", Mode: mode, Project: "word-search", KeywordOnly: mode == "hybrid",
			})
			require.NoError(t, err)
			assert.Equal(t, []hit{{"exact", "message", 1}, {"two", "message", 1}, {"one", "message", 1}}, project(page.Matches))
		})
		t.Run("stop word leaves one searchable word/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "invoice the", Mode: mode, Project: "word-search", KeywordOnly: mode == "hybrid",
			})
			require.NoError(t, err)
			assert.Equal(t, []hit{{"exact", "message", 1}, {"two", "message", 1}, {"one", "message", 1}}, project(page.Matches))
		})
	}
	for _, tc := range []struct {
		name, pattern, project string
		want                   []hit
	}{
		{"punctuation splits query words", "invoice.txt missingword", "word-split-punctuation", []hit{{"split-punctuation", "message", 1}}},
		{"underscore phrase", `"invoice rounding" decision`, "word-boundary", []hit{{"underscore", "message", 3}}},
		{"multiline", "invoice rounding decision", "word-multiline", []hit{{"multiline", "message", 3}}},
	} {
		for _, mode := range []string{"substring", "fts", "hybrid"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
					Pattern: tc.pattern, Mode: mode, Project: tc.project,
					KeywordOnly: mode == "hybrid",
				})
				require.NoError(t, err)
				assert.Equal(t, tc.want, project(page.Matches))
			})
		}
	}
	t.Run("quoted fts phrase crosses punctuation", func(t *testing.T) {
		page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
			Pattern: `"quarterly forecast"`, Mode: "fts", Project: "word-search",
		})
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		assert.Equal(t, "phrase", page.Matches[0].SessionID)
		wantWords := 2
		if named, ok := store.(db.MemoryBackendNamer); ok && named.MemoryBackendName() == "sqlite" {
			wantWords = 0
		}
		assert.Equal(t, wantWords, page.Matches[0].MatchedWords)
	})
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run("CJK letters are word characters/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "invoice absentword", Mode: mode, Project: "word-cjk-boundary",
				KeywordOnly: mode == "hybrid",
			})
			require.NoError(t, err)
			assert.Empty(t, page.Matches)
		})
		t.Run("partial punctuation candidates keep word boundaries/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "invoice absentword", Mode: mode, Project: "word-punctuation",
				KeywordOnly: mode == "hybrid",
			})
			require.NoError(t, err)
			assert.ElementsMatch(t, []hit{
				{"invoice-dot", "message", 1},
				{"invoice-path", "message", 1},
				{"invoice-email", "message", 1},
				{"invoice-version", "message", 1},
			}, project(page.Matches))
		})
	}
	t.Run("partial punctuation paging", func(t *testing.T) {
		f := db.ContentSearchFilter{
			Pattern: "invoice absentword", Mode: "fts", Project: "word-punctuation", Limit: 2,
		}
		var paged []hit
		for range 3 {
			page, err := store.SearchContent(t.Context(), f)
			require.NoError(t, err)
			for _, match := range page.Matches {
				paged = append(paged, hit{match.SessionID, match.Location, match.MatchedWords})
			}
			if page.NextCursor == 0 {
				break
			}
			f.Cursor = page.NextCursor
		}
		assert.ElementsMatch(t, []hit{
			{"invoice-dot", "message", 1},
			{"invoice-path", "message", 1},
			{"invoice-email", "message", 1},
			{"invoice-version", "message", 1},
		}, paged)
	})
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run("exact phrase snippet/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "invoice rounding decision", Mode: mode, Project: "word-exact-snippet",
				KeywordOnly: mode == "hybrid",
			})
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Contains(t, page.Matches[0].Snippet, "invoice rounding decision")
		})
	}
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run("underscore/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "invoice rounding decision", Mode: mode, Project: "word-boundary", KeywordOnly: mode == "hybrid"})
			require.NoError(t, err)
			want := []hit{{"underscore", "message", 3}, {"boundary-near", "message", 1}}
			if mode == "substring" {
				want = []hit{{"boundary-near", "message", 3}, {"underscore", "message", 3}}
			}
			assert.Equal(t, want, project(page.Matches))
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
			page, err := store.SearchContent(t.Context(), f)
			require.NoError(t, err)
			want := []hit{{"exact", "message", 3}, {"two", "message", 2}, {"one", "message", 1}}
			if mode == "substring" {
				want = []hit{{"exact", "message", 3}, {"tool", "tool_result", 3}, {"two", "message", 2}, {"one", "message", 1}}
			}
			assert.Equal(t, want, project(page.Matches))
			for _, match := range page.Matches {
				if match.SessionID == "two" {
					assert.Contains(t, match.Snippet, "rounding of invoices")
				}
			}
			if mode == "hybrid" {
				return
			}
			f.Limit = 1
			var paged []hit
			for range 5 {
				page, err = store.SearchContent(t.Context(), f)
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
