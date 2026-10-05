package review

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalDateAndAddDays(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	tests := []struct {
		name string
		t    time.Time
		loc  *time.Location
		want string
	}{
		{name: "utc", t: time.Date(2026, 9, 22, 23, 59, 0, 0, time.UTC), loc: time.UTC, want: "2026-09-22"},
		{name: "tokyo rolls forward", t: time.Date(2026, 9, 22, 15, 30, 0, 0, time.UTC), loc: tokyo, want: "2026-09-23"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, localDate(tt.t, tt.loc)) })
	}
	assert.Equal(t, "2026-03-01", addDays("2026-02-28", 1))
	assert.Equal(t, "2026-09-15", addDays("2026-09-22", -7))
	assert.Equal(t, "2027-01-01", addDays("2026-12-31", 1))
}
