package dbtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// NameSearchProject is the project SeedNameSearch writes its sessions to.
const NameSearchProject = "name-search"

// SeedNameSearch writes sessions whose name or first message contains
// "namedneedle". Two have an embeddable message and match only by name, two
// have only system messages, and one also matches in message content. Use
// AssertNameSearch after pushing it.
func SeedNameSearch(t *testing.T, d *db.DB) {
	t.Helper()
	seed := func(id, day string, name, first *string, msgs ...db.Message) {
		t.Helper()
		ts := day + "T12:00:00Z"
		SeedSession(t, d, id, NameSearchProject, func(s *db.Session) {
			s.SessionName = name
			s.FirstMessage = first
			s.StartedAt = new(ts)
			s.EndedAt = new(ts)
			s.MessageCount = len(msgs)
		})
		for i := range msgs {
			msgs[i].SessionID = id
			msgs[i].Ordinal = i
			msgs[i].Timestamp = ts
		}
		SeedMessages(t, d, msgs...)
	}
	seed("name-search-first", "2026-03-01", nil, new("namedneedle first prompt"),
		UserMsg("", 0, "plain body"))
	seed("name-search-title", "2026-03-02", new("namedneedle title"), nil,
		UserMsg("", 0, "plain body"))
	seed("name-search-content", "2026-03-03", new("namedneedle content"), nil,
		UserMsg("", 0, "plain body"), AsstMsg("", 0, "body namedneedle hit"))
	flagged := UserMsg("", 0, "plain body")
	flagged.IsSystem = true
	seed("name-search-flagged", "2026-03-04", new("namedneedle flagged"), nil, flagged)
	seed("name-search-prefixed", "2026-03-05", new("namedneedle prefixed"), nil,
		UserMsg("", 0, "This session is being continued from a previous conversation"))
}

// AssertNameSearch checks the rows a mirror returns for SeedNameSearch. A
// session that matches only by name returns ordinal -1 with the matched text
// as its snippet. Sessions without an embeddable message are left out, and a
// session matched by message content appears once, from its message.
func AssertNameSearch(t *testing.T, store db.Store) {
	t.Helper()
	type row struct {
		Session string
		Ordinal int
		Snippet string
	}
	content := row{"name-search-content", 1, "body namedneedle hit"}
	title := row{"name-search-title", -1, "namedneedle title"}
	first := row{"name-search-first", -1, "namedneedle first prompt"}
	for _, tc := range []struct {
		name   string
		filter db.SearchFilter
		want   []row
	}{
		{"all projects", db.SearchFilter{}, []row{content, title, first}},
		{"project", db.SearchFilter{Project: NameSearchProject}, []row{content, title, first}},
		{"project and dates", db.SearchFilter{
			Project: NameSearchProject, DateFrom: "2026-03-02", DateTo: "2026-03-05",
		}, []row{content, title}},
		{"recency", db.SearchFilter{Sort: "recency"}, []row{content, title, first}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.filter
			f.Query = "namedneedle"
			f.Limit = 10
			page, err := store.Search(t.Context(), f)
			require.NoError(t, err)
			got := make([]row, len(page.Results))
			for i, r := range page.Results {
				got[i] = row{r.SessionID, r.Ordinal, r.Snippet}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
