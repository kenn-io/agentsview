package db

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rank walk must return the pages that ranking every match returns.
// Project-filtered searches always rank every match, so a single-project
// archive compares the two paths on the same data: sessions that tie on
// rank with each other and between their own messages, sessions with
// distinct ranks, enough ties to pass the walk's row bound, and title-only
// matches that follow every message match.
func TestSearchRankWalkMatchesFullRanking(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seed := func(id, title string, contents ...string) {
		t.Helper()
		insertSession(t, d, id, "proj", func(s *Session) {
			s.FirstMessage = new(title)
			s.EndedAt = new(fmt.Sprintf("2026-05-%02dT12:00:00Z", 1+len(id)%28))
		})
		var msgs []Message
		for i, content := range contents {
			msgs = append(msgs, userMsg(id, i, content))
		}
		insertMessages(t, d, msgs...)
	}
	insertSession(t, d, "older", "proj", func(s *Session) {
		s.FirstMessage = new("untitled")
		s.EndedAt = new("2026-04-01T12:00:00Z")
	})
	var older []Message
	for i := range 200 {
		older = append(older, userMsg("older", i, "deploy the handler"))
	}
	insertMessages(t, d, older...)
	for i := range 100 {
		seed(fmt.Sprintf("tie-%03d", i), "untitled", "intro", "deploy the handler", "middle", "deploy the handler")
	}
	for i := range 10 {
		seed(fmt.Sprintf("varied-%02d", i), "untitled",
			"handler "+strings.Repeat("word ", i), strings.Repeat("filler ", 3)+"handler")
	}
	for i := range 3 {
		seed(fmt.Sprintf("title-%d", i), "the handler title", "nothing relevant")
	}
	// Equal ranks resolve to the lowest ordinal even when that message was
	// stored, and so read, after its tie.
	for i := range 3 {
		id := fmt.Sprintf("late-%d", i)
		insertSession(t, d, id, "proj", func(s *Session) {
			s.FirstMessage = new("untitled")
			s.EndedAt = new("2026-05-11T12:00:00Z")
		})
		insertMessages(t, d, userMsg(id, 5, "handler"))
		insertMessages(t, d, userMsg(id, 2, "handler"))
	}

	walk, err := walkSearchRanks(ctx, d.getReader(), "messages_fts", "messages_fts.rendered MATCH ?", []any{`"handler"`}, 2, false)
	require.NoError(t, err)
	require.NotNil(t, walk, "the walk must serve a page with distinct ranks")
	assert.False(t, walk.complete)

	// Count the visibility checks made while ranking the same eligible hits.
	// A project filter forces full ranking; all sessions have that project.
	d.rawReader().SetMaxOpenConns(1)
	conn, err := d.getReader().Conn(ctx)
	require.NoError(t, err)
	visited := 0
	require.NoError(t, conn.Raw(func(raw any) error {
		return raw.(interface{ RegisterFunc(string, any, bool) error }).RegisterFunc("unicode", func(content string) int {
			if content == "deploy the handler" {
				visited++
			}
			r, _ := utf8.DecodeRuneInString(content)
			return int(r)
		}, false)
	}))
	require.NoError(t, conn.Close())
	filter := SearchFilter{Query: "deploy", Limit: 1, DateFrom: "2026-05-01", Project: "proj"}
	full, err := d.Search(ctx, filter)
	require.NoError(t, err)
	fullVisits := visited
	require.GreaterOrEqual(t, fullVisits, 200)
	visited = 0
	filter.Project = ""
	for cursor, want := range []string{"tie-000", "tie-001"} {
		filter.Cursor = cursor
		page, err := d.Search(ctx, filter)
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		assert.Equal(t, want, page.Results[0].SessionID)
		assert.Contains(t, page.Results[0].Snippet, "deploy")
		if cursor == 0 {
			assert.Equal(t, full, page)
			require.Equal(t, fullVisits, visited, "search must not rank the matches twice")
		}
	}

	for _, from := range []string{"", "2026-05-08", "2026-05-09", "2027-01-01"} {
		for _, query := range []string{"handler", "deploy"} {
			for _, limit := range []int{1, 2, 7, 50} {
				var walked, ranked []SearchResult
				for cursor := 0; ; cursor += limit {
					got, err := d.Search(ctx, SearchFilter{Query: query, Limit: limit, Cursor: cursor, DateFrom: from})
					require.NoError(t, err)
					want, err := d.Search(ctx, SearchFilter{Query: query, Limit: limit, Cursor: cursor, DateFrom: from, Project: "proj"})
					require.NoError(t, err)
					require.Equal(t, want, got, "%s from %s limit %d cursor %d", query, from, limit, cursor)
					walked = append(walked, got.Results...)
					ranked = append(ranked, want.Results...)
					if got.NextCursor == 0 {
						break
					}
				}
				assert.Equal(t, ranked, walked)
				if query == "handler" && from == "" {
					require.Len(t, walked, 117, "limit %d", limit)
					assert.Equal(t, -1, walked[len(walked)-1].Ordinal, "title-only rows come last")
				}
				if from == "2027-01-01" {
					assert.Empty(t, walked)
				}
			}
		}
	}
}
