package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

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
