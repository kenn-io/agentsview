package recall

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPathBaseHandlesBackslashSeparators(t *testing.T) {
	assert.Equal(t, "recall_entries.go", pathBase("internal/db/recall_entries.go"))
	assert.Equal(t, "myproj", pathBase(`C:\work\myproj`))
	assert.Equal(t, "leaf", pathBase(`C:\work\leaf\`))
	assert.Equal(t, "single", pathBase("single"))
	assert.Equal(t, "", pathBase("   "))
}

func TestQueryCalendarWindowsRequiresAdjacentMonthYear(t *testing.T) {
	feb := time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC)
	want := []timeWindow{{start: feb, end: feb.AddDate(0, 1, 0)}}

	require.Equal(t, want, queryCalendarWindows([]string{"february", "2024"}))
	require.Equal(t, want, queryCalendarWindows([]string{"2024", "february"}))

	assert.Nil(t, queryCalendarWindows([]string{"february", "database", "2024"}))
	assert.Nil(t, queryCalendarWindows([]string{"2024"}))
	assert.Nil(t, queryCalendarWindows([]string{"may", "notes"}))
}

func TestOrderedTokensHaveAdjacentRequiresConsecutiveOrder(t *testing.T) {
	assert.True(t, orderedTokensHaveAdjacent([]string{"last", "month", "notes"}, "last", "month"))
	assert.False(t, orderedTokensHaveAdjacent([]string{"month", "last"}, "last", "month"))
	assert.False(t, orderedTokensHaveAdjacent([]string{"last", "full", "month"}, "last", "month"))
	assert.False(t, orderedTokensHaveAdjacent(nil, "last", "month"))
}

func TestStartOfISOWeekReturnsMondayMidnight(t *testing.T) {
	// 2024-02-14 is a Wednesday; its ISO week starts Monday 2024-02-12.
	wednesday := time.Date(2024, time.February, 14, 9, 30, 0, 0, time.UTC)
	monday := time.Date(2024, time.February, 12, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, monday, startOfISOWeek(wednesday))
	// A Monday is its own week start.
	assert.Equal(t, monday, startOfISOWeek(monday))
	// Sunday belongs to the week that began the prior Monday.
	sunday := time.Date(2024, time.February, 18, 23, 0, 0, 0, time.UTC)
	assert.Equal(t, monday, startOfISOWeek(sunday))
}
