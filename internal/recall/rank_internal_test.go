package recall

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPathBaseHandlesBackslashSeparators(t *testing.T) {
	assert := assert.New(t)

	assert.Equal("recall_entries.go", pathBase("internal/db/recall_entries.go"))
	assert.Equal("myproj", pathBase(`C:\work\myproj`))
	assert.Equal("leaf", pathBase(`C:\work\leaf\`))
	assert.Equal("single", pathBase("single"))
	assert.Empty(pathBase("   "))
}

func TestQueryCalendarWindowsRequiresAdjacentMonthYear(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	feb := time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC)
	want := []timeWindow{{start: feb, end: feb.AddDate(0, 1, 0)}}

	require.Equal(want, queryCalendarWindows([]string{"february", "2024"}))
	require.Equal(want, queryCalendarWindows([]string{"2024", "february"}))

	assert.Nil(queryCalendarWindows([]string{"february", "database", "2024"}))
	assert.Nil(queryCalendarWindows([]string{"2024"}))
	assert.Nil(queryCalendarWindows([]string{"may", "notes"}))
}

func TestOrderedTokensHaveAdjacentRequiresConsecutiveOrder(t *testing.T) {
	assert := assert.New(t)

	assert.True(orderedTokensHaveAdjacent([]string{"last", "month", "notes"}, "last", "month"))
	assert.False(orderedTokensHaveAdjacent([]string{"month", "last"}, "last", "month"))
	assert.False(orderedTokensHaveAdjacent([]string{"last", "full", "month"}, "last", "month"))
	assert.False(orderedTokensHaveAdjacent(nil, "last", "month"))
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
