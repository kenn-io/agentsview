package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestSearchContent_ActiveBeforeLimit(t *testing.T) {
	ts, d := newTestToolset(t)
	for i := range 7 {
		id := fmt.Sprintf("session-%d", i)
		ended := "2024-06-15T10:00:00Z"
		if i < 3 {
			ended = "2024-06-15T11:59:00Z"
		}
		dbtest.SeedSession(t, d, id, "project-a", func(s *db.Session) {
			s.UserMessageCount = 2
			s.MessageCount = 3
			s.EndedAt = &ended
		})
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
			dbtest.UserMsg(id, 0, "docker-compose.test.yml"),
		}))
	}
	_, out, err := ts.searchContent(t.Context(), nil, searchContentIn{
		Pattern: "docker-compose.test.yml", Limit: 3,
	})
	require.NoError(t, err)
	require.Len(t, out.Matches, 3)
	assert.True(t, out.Exclusions.RecentActive)
	require.NotNil(t, out.NextCursor)
	_, page, err := ts.searchContent(t.Context(), nil, searchContentIn{
		Pattern: "docker-compose.test.yml", Limit: 3, Cursor: *out.NextCursor,
	})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Nil(t, page.NextCursor)
	ids := []string{page.Matches[0].SessionID}
	for _, match := range out.Matches {
		ids = append(ids, match.SessionID)
	}
	assert.ElementsMatch(t, []string{"session-3", "session-4", "session-5", "session-6"}, ids)
}
