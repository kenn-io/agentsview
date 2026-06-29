package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestBuildAnalyticsWhere_ModelFilter(t *testing.T) {
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithDate(
		db.AnalyticsFilter{
			From:  "2024-06-01",
			To:    "2024-06-03",
			Model: "gpt-4o, claude-3-5-sonnet",
		},
		pgDateColS,
		pb,
		true,
		"s.id",
	)

	assert.Contains(t, where,
		"EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id",
	)
	assert.Contains(t, where, "m.model IN (")
	assert.Len(t, pb.args, 4, "date range plus two model args")
	assert.Equal(t, "gpt-4o", pb.args[2])
	assert.Equal(t, "claude-3-5-sonnet", pb.args[3])
}

func TestBuildAnalyticsWhere_ModelFilterTrimsEmptyValues(t *testing.T) {
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithDate(
		db.AnalyticsFilter{
			From:  "2024-06-01",
			To:    "2024-06-03",
			Model: " , gpt-4o , ",
		},
		pgDateCol,
		pb,
		true,
		"id",
	)

	assert.Contains(t, where, "m.model = ")
	assert.NotContains(t, where, "m.model IN (")
	assert.Equal(t, "gpt-4o", pb.args[2])
	assert.Equal(t, 1,
		strings.Count(where, "m.model = "),
		"expected a single model predicate",
	)
}

func TestRankTopSessions_DurationSort(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "a", ActiveDurationMin: 10.0},
		{ID: "b", ActiveDurationMin: 30.0},
		{ID: "c", ActiveDurationMin: 20.0},
	}
	got := rankTopSessions(sessions, true)
	require.Len(t, got, 3)
	assert.Equal(t, "b", got[0].ID)
	assert.Equal(t, "c", got[1].ID)
	assert.Equal(t, "a", got[2].ID)
}

func TestRankTopSessions_DurationTieBreaker(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "z", ActiveDurationMin: 5.0},
		{ID: "a", ActiveDurationMin: 5.0},
		{ID: "m", ActiveDurationMin: 5.0},
	}
	got := rankTopSessions(sessions, true)
	require.Len(t, got, 3)
	assert.Equal(t, "a", got[0].ID)
	assert.Equal(t, "m", got[1].ID)
	assert.Equal(t, "z", got[2].ID)
}

func TestRankTopSessions_NearTiePrecision(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "a", ActiveDurationMin: 10.04},
		{ID: "b", ActiveDurationMin: 10.06},
	}
	got := rankTopSessions(sessions, true)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].ID, "10.06 > 10.04")
	assert.Equal(t, 10.1, got[0].ActiveDurationMin)
	assert.Equal(t, 10.0, got[1].ActiveDurationMin)
}

func TestRankTopSessions_TruncatesTo10(t *testing.T) {
	sessions := make([]db.TopSession, 15)
	for i := range sessions {
		sessions[i] = db.TopSession{
			ID:                string(rune('a' + i)),
			ActiveDurationMin: float64(i),
		}
	}
	got := rankTopSessions(sessions, true)
	require.Len(t, got, 10)
	assert.Equal(t, 14.0, got[0].ActiveDurationMin)
}

func TestRankTopSessions_UsesActiveDurationForSort(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "wall", DurationMin: 120.0, ActiveDurationMin: 1.0},
		{ID: "active", DurationMin: 10.0, ActiveDurationMin: 15.0},
	}
	got := rankTopSessions(sessions, true)
	require.Len(t, got, 2)
	assert.Equal(t, "active", got[0].ID)
	assert.Equal(t, 15.0, got[0].ActiveDurationMin)
	assert.Equal(t, 120.0, got[1].DurationMin)
}

func TestRankTopSessions_NoSortForMessages(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "c", MessageCount: 10},
		{ID: "a", MessageCount: 30},
		{ID: "b", MessageCount: 20},
	}
	got := rankTopSessions(sessions, false)
	require.Len(t, got, 3)
	assert.Equal(t, "c", got[0].ID)
	assert.Equal(t, "a", got[1].ID)
	assert.Equal(t, "b", got[2].ID)
}

func TestRankTopSessions_NilInput(t *testing.T) {
	got := rankTopSessions(nil, true)
	require.NotNil(t, got, "expected non-nil empty slice")
	assert.Empty(t, got)
}

func TestRankTopSessions_RoundsForDisplay(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "a", ActiveDurationMin: 12.349},
		{ID: "b", ActiveDurationMin: 12.351},
	}
	got := rankTopSessions(sessions, true)
	require.Len(t, got, 2)
	assert.Equal(t, 12.4, got[0].ActiveDurationMin)
	assert.Equal(t, 12.3, got[1].ActiveDurationMin)
}
