package db

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTermsAnchorSessions writes sessions whose transcripts mix the cases
// that decide exchange boundaries: sidechain branches, assistant rows before
// the first user message, user rows that are system messages by flag or by
// prefix, other roles, and dialogue text that differs from content.
func seedTermsAnchorSessions(t *testing.T, d *DB) {
	t.Helper()
	rng := rand.New(rand.NewPCG(7, 11))
	words := []string{"alpha-term", "ALPHA-TERM", "beta-term", "gamma-term", "filler", "filler", "filler"}
	roles := []string{"user", "user", "assistant", "assistant", "assistant", "tool"}
	for i := range 40 {
		id := fmt.Sprintf("anchor-%02d", i)
		insertSession(t, d, id, "proj", func(s *Session) {
			s.Agent = "claude"
			s.UserMessageCount = 2
			s.StartedAt = new(fmt.Sprintf("2026-05-%02dT12:00:00Z", 1+i%20))
			if i%7 == 3 {
				s.RelationshipType = "subagent"
				s.ParentSessionID = new("anchor-00")
			}
		})
		var msgs []Message
		for ordinal := range 6 + rng.IntN(20) {
			m := Message{
				SessionID: id, Ordinal: ordinal, Role: roles[rng.IntN(len(roles))],
				Content:     words[rng.IntN(len(words))] + " text " + words[rng.IntN(len(words))],
				Timestamp:   fmt.Sprintf("2026-05-20T12:%02d:00Z", ordinal),
				IsSidechain: rng.IntN(5) == 0,
			}
			switch rng.IntN(12) {
			case 0:
				m.IsSystem = true
			case 1:
				m.Content = "<command-name>" + m.Content
			case 2:
				m.DialogueText = new(words[rng.IntN(len(words))] + " dialogue")
			}
			msgs = append(msgs, m)
		}
		require.NoError(t, d.InsertMessages(t.Context(), msgs))
		_, err := d.getWriter().Exec(t.Context(),
			`UPDATE sessions SET message_count = 1000 WHERE id = ?`, id)
		require.NoError(t, err)
	}
}

// The query that starts from the messages the substring index lists returns
// the same pages as the query that groups every message into exchanges.
func TestSearchContentTermsAnchorMatchesScan(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedTermsAnchorSessions(t, d)
	var nonEmpty int
	for _, pattern := range []string{
		"alpha-term", "alpha-term beta-term", "beta-term gamma-term",
		"gamma-term filler", "filler alpha-term beta-term", "dialogue beta-term",
	} {
		for _, scope := range []string{"all", "top", "subordinate"} {
			for _, cursor := range []int{0, 4, 8} {
				f := ContentSearchFilter{Pattern: pattern, Mode: "terms", Scope: scope, Limit: 4, Cursor: cursor}
				terms := ParseContentSearchTerms(pattern)
				indexed, err := d.termsIndexQueries(ctx, f, terms)
				require.NoError(t, err)
				_, anchored := termsAnchorTerm(terms, indexed)
				require.True(t, anchored, pattern)

				got, err := d.SearchContent(ctx, f)
				require.NoError(t, err)
				want, err := d.termsSearchPage(ctx, f, terms, nil)
				require.NoError(t, err)
				assert.Equal(t, want, got, "%s scope=%s cursor=%d", pattern, scope, cursor)
				if len(want.Matches) > 0 {
					nonEmpty++
				}
			}
		}
	}
	assert.Greater(t, nonEmpty, 20)
}
