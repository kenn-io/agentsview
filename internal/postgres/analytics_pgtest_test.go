package postgres

import (
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

func TestRankTopSessions_DurationSort(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "a", DurationMin: 10.0},
		{ID: "b", DurationMin: 30.0},
		{ID: "c", DurationMin: 20.0},
	}
	got := rankTopSessions(sessions, true)
	if got[0].ID != "b" || got[1].ID != "c" ||
		got[2].ID != "a" {
		t.Errorf("expected b,c,a order, got %s,%s,%s",
			got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestRankTopSessions_DurationTieBreaker(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "z", DurationMin: 5.0},
		{ID: "a", DurationMin: 5.0},
		{ID: "m", DurationMin: 5.0},
	}
	got := rankTopSessions(sessions, true)
	if got[0].ID != "a" || got[1].ID != "m" ||
		got[2].ID != "z" {
		t.Errorf(
			"expected a,m,z tie-break order, got %s,%s,%s",
			got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestRankTopSessions_NearTiePrecision(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "a", DurationMin: 10.04},
		{ID: "b", DurationMin: 10.06},
	}
	got := rankTopSessions(sessions, true)
	if got[0].ID != "b" {
		t.Errorf("expected b first (10.06 > 10.04), got %s",
			got[0].ID)
	}
	if got[0].DurationMin != 10.1 ||
		got[1].DurationMin != 10.0 {
		t.Errorf("expected rounded 10.1, 10.0; got %.1f, %.1f",
			got[0].DurationMin, got[1].DurationMin)
	}
}

func TestRankTopSessions_TruncatesTo10(t *testing.T) {
	sessions := make([]db.TopSession, 15)
	for i := range sessions {
		sessions[i] = db.TopSession{
			ID:          string(rune('a' + i)),
			DurationMin: float64(i),
		}
	}
	got := rankTopSessions(sessions, true)
	if len(got) != 10 {
		t.Errorf("expected 10 sessions, got %d", len(got))
	}
	if got[0].DurationMin != 14.0 {
		t.Errorf(
			"expected first session duration 14.0, got %.1f",
			got[0].DurationMin)
	}
}

func TestRankTopSessions_NoSortForMessages(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "c", MessageCount: 10},
		{ID: "a", MessageCount: 30},
		{ID: "b", MessageCount: 20},
	}
	got := rankTopSessions(sessions, false)
	if got[0].ID != "c" || got[1].ID != "a" ||
		got[2].ID != "b" {
		t.Errorf(
			"expected preserved order c,a,b, got %s,%s,%s",
			got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestRankTopSessions_NilInput(t *testing.T) {
	got := rankTopSessions(nil, true)
	if got == nil {
		t.Error("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d elements",
			len(got))
	}
}

func TestRankTopSessions_RoundsForDisplay(t *testing.T) {
	sessions := []db.TopSession{
		{ID: "a", DurationMin: 12.349},
		{ID: "b", DurationMin: 12.351},
	}
	got := rankTopSessions(sessions, true)
	if got[0].DurationMin != 12.4 {
		t.Errorf("expected 12.4, got %v",
			got[0].DurationMin)
	}
	if got[1].DurationMin != 12.3 {
		t.Errorf("expected 12.3, got %v",
			got[1].DurationMin)
	}
}
