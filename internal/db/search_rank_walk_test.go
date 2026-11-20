package db

import (
	"fmt"
	"strings"
	"testing"

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
					require.Len(t, walked, 116, "limit %d", limit)
					assert.Equal(t, -1, walked[len(walked)-1].Ordinal, "title-only rows come last")
				}
				if from == "2027-01-01" {
					assert.Empty(t, walked)
				}
			}
		}
	}
}

func TestSearchDateRankWalkLargeTie(t *testing.T) {
	d := testDB(t)
	_, err := d.getWriter().Exec(t.Context(), `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<4104)
		INSERT INTO sessions(id, project, agent, ended_at) SELECT printf('tie-%04d', i), 'proj', 'claude', '2026-05-11T12:00:00Z' FROM n`)
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), `INSERT INTO messages(session_id, ordinal, role, content) SELECT id, 0, 'user', 'deploy the handler' FROM sessions`)
	require.NoError(t, err)
	for _, cursor := range []int{0, 5} {
		f := SearchFilter{Query: "handler", DateFrom: "2026-05-01", Limit: 5, Cursor: cursor}
		got, err := d.Search(t.Context(), f)
		require.NoError(t, err)
		f.Project = "proj"
		want, err := d.Search(t.Context(), f)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Len(t, got.Results, 5)
		assert.Equal(t, fmt.Sprintf("tie-%04d", cursor), got.Results[0].SessionID)
		assert.Contains(t, got.Results[0].Snippet, "<mark>handler</mark>")
		assert.Equal(t, cursor+5, got.NextCursor)
	}
}
