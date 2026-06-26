package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// seedServiceSearchSession creates a session with a single user message
// whose content contains the given text. The session has UserMessageCount=2
// so it is not excluded by the default one-shot filter.
func seedServiceSearchSession(
	t *testing.T, d *db.DB, id, project, msgContent string,
) {
	t.Helper()
	dbtest.SeedSessionWithMessages(t, d, id, project, []db.Message{
		dbtest.UserMsg(id, 0, msgContent),
		dbtest.AsstMsg(id, 1, "understood"),
	}, dbtest.WithMessageCounts(3, 2))
}

func TestDirectSearchContentRedacts(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	seedServiceSearchSession(t, d, "x1", "proj",
		"my key is AKIA7QHWN2DKR4FYPLJM ok")
	be := service.NewDirectBackend(d, nil)

	// default: secret should be redacted
	res, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "AKIA", Mode: "substring", Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	assert.False(t, strings.Contains(res.Matches[0].Snippet, "AKIA7QHWN2DKR4FYPLJM"),
		"default search leaked secret: %q", res.Matches[0].Snippet)

	// reveal: full secret should be present
	rev, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "AKIA", Mode: "substring", Limit: 50, Reveal: true,
	})
	require.NoError(t, err)
	require.Len(t, rev.Matches, 1)
	assert.True(t, strings.Contains(rev.Matches[0].Snippet, "AKIA7QHWN2DKR4FYPLJM"),
		"reveal should show full secret: %q", rev.Matches[0].Snippet)
}

func TestDirectSearchContentFTSSourceGuard(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	be := service.NewDirectBackend(d, nil)

	_, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "test", Mode: "fts",
		Sources: []string{"tool_result"},
		Limit:   50,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages only")
}
