package service_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestSearchContentHintsBoundedCounts(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for i := range 53 {
		id := fmt.Sprintf("hidden-%d", i)
		dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) { s.UserMessageCount = 1 })
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{dbtest.UserMsg(id, 0, "marker")}))
	}
	svc := service.NewDirectBackend(d, nil)
	req := service.ContentSearchRequest{Pattern: "marker"}
	res, err := svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	hints := service.SearchContentHints(t.Context(), svc, req, res, true)
	require.Len(t, hints, 1)
	assert.Equal(t, "more than 50 matches are in one-shot sessions, hidden by default", hints[0].Message)
	req.IncludeOneShot = true
	res, err = svc.SearchContent(t.Context(), req)
	require.NoError(t, err)
	assert.Empty(t, service.SearchContentHints(t.Context(), svc, req, res, true))
	req.IncludeOneShot = false
	req.Cursor = 1
	assert.Empty(t, service.SearchContentHints(t.Context(), svc, req, &service.ContentSearchResult{}, true))
}
