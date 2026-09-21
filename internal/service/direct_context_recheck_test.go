package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

// After the context windows are fetched in reads that follow the search
// snapshot, a match whose session no longer carries the cited revision must
// lose that revision (becoming unbound), while an unchanged match keeps it.
func TestDropContextMatchesWithChangedRevision(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	b := &directBackend{db: d, evidenceSource: "test-source"}

	dbtest.SeedSession(t, d, "stable", "proj", func(s *db.Session) {
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		dbtest.UserMsg("stable", 0, "anchored evidence text"),
	}))
	stable, err := d.GetSession(t.Context(), "stable")
	require.NoError(t, err)
	require.NotNil(t, stable)
	require.NotEmpty(t, stable.TranscriptRevision)

	matches := []db.ContentMatch{
		{SessionID: "stable", TranscriptRevision: *stable.TranscriptRevision},
		{SessionID: "stable", TranscriptRevision: "stale-revision-value"},
		{SessionID: "stable"},
	}
	require.NoError(t, b.dropContextMatchesWithChangedRevision(t.Context(), matches))

	assert.Equal(t, *stable.TranscriptRevision, matches[0].TranscriptRevision,
		"unchanged revision must be kept")
	assert.Empty(t, matches[1].TranscriptRevision,
		"changed revision must drop the match to unbound")
	assert.Empty(t, matches[2].TranscriptRevision)
}
