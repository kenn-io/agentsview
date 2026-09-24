//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestHostedCursorsSurviveUnpublishedGenerationSelection(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "capture-a", "")
	parsed := projectionOutcome("older session")
	recent := projectionOutcome("recent session").Outcome.Results[0]
	recent.Result.Session.ID = "codex:recent"
	recent.Result.Session.SourceSessionID = "recent"
	recent.Result.Session.StartedAt = recent.Result.Session.StartedAt.Add(time.Hour)
	parsed.Outcome.Results = append(parsed.Outcome.Results, recent)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, parsed))
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	h.SetCursorSecret([]byte("synthetic cursor secret"))
	page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	require.Equal(t, "codex:recent", page.Sessions[0].ID)
	require.NotEmpty(t, page.NextCursor)
	sidebar, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, sidebar.NextCursor)
	report, err := h.EncodeActivityReportToken([]byte(`{"report":"published"}`))
	require.NoError(t, err)

	// Acceptance selects a successor; its rows are not visible until projection.
	next, _ := f.accept(t, "device-a", "capture-b", accepted.Receipt)
	_, err = f.sink.SelectSourceGeneration(t.Context(), next, "parser-2")
	require.NoError(t, err)
	second, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 1, Cursor: page.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Sessions, 1)
	assert.Equal(t, "codex:portable", second.Sessions[0].ID)
	assert.Empty(t, second.NextCursor)
	secondSidebar, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: 1, Cursor: sidebar.NextCursor})
	require.NoError(t, err)
	require.Len(t, secondSidebar.Sessions, 1)
	assert.Equal(t, "codex:portable", secondSidebar.Sessions[0].ID)
	assert.Empty(t, secondSidebar.NextCursor)
	decoded, err := h.DecodeCursor(page.NextCursor)
	require.NoError(t, err)
	assert.Equal(t, "codex:recent", decoded.ID)
	payload, err := h.DecodeActivityReportToken(report)
	require.NoError(t, err)
	assert.JSONEq(t, `{"report":"published"}`, string(payload))
}
