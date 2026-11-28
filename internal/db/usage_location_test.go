package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadLocationOrFollowsLocalTimezoneChange(t *testing.T) {
	original := time.Local                      //nolint:forbidigo // Exercise a process-local timezone change between report requests.
	t.Cleanup(func() { time.Local = original }) //nolint:forbidigo // Restore the process-local timezone after the test.
	instant := time.Date(2024, time.April, 1, 3, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		zone, wantDate string
	}{
		{"UTC", "2024-04-01"},
		{"America/New_York", "2024-03-31"},
		{"UTC", "2024-04-01"},
	} {
		loc, err := time.LoadLocation(tc.zone)
		require.NoError(t, err)
		time.Local = loc //nolint:forbidigo // Simulate the timezone changes made by analytics parity tests.
		assert.Equal(t, tc.wantDate,
			instant.In(LoadLocationOr("Local", time.UTC)).Format(time.DateOnly), tc.zone)
	}
}
