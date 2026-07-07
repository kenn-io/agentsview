package timeutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPtr(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want *string
	}{
		{
			name: "zero time returns nil",
			in:   time.Time{},
			want: nil,
		},
		{
			name: "non-zero returns RFC3339Nano UTC",
			in:   time.Date(2024, 6, 15, 12, 30, 45, 123000000, time.UTC),
			want: new("2024-06-15T12:30:45.123Z"),
		},
		{
			name: "converts to UTC",
			in:   time.Date(2024, 6, 15, 7, 30, 0, 0, time.FixedZone("EST", -5*60*60)),
			want: new("2024-06-15T12:30:00Z"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Ptr(tt.in)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
		})
	}
}

func TestFormat(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"zero time returns empty", time.Time{}, ""},
		{"non-zero returns RFC3339Nano UTC", time.Date(2024, 6, 15, 12, 30, 45, 0, time.UTC), "2024-06-15T12:30:45Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Format(tt.in))
		})
	}
}

func TestIsValidDate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"valid date", "2024-06-15", true},
		{"empty string", "", false},
		{"wrong separator", "2024/06/15", false},
		{"two-digit year", "24-06-15", false},
		{"includes time", "2024-06-15T00:00:00Z", false},
		{"impossible month", "2024-13-01", false},
		{"impossible day", "2024-02-30", false},
		{"non-numeric", "not-a-date", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsValidDate(tt.in))
		})
	}
}

func TestIsValidTimestamp(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"RFC3339 UTC", "2024-06-15T12:30:45Z", true},
		{"RFC3339 offset", "2024-06-15T12:30:45-05:00", true},
		{"RFC3339Nano", "2024-06-15T12:30:45.123456789Z", true},
		{"empty string", "", false},
		{"date only", "2024-06-15", false},
		{"missing timezone", "2024-06-15T12:30:45", false},
		{"non-numeric", "not-a-timestamp", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsValidTimestamp(tt.in))
		})
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{"hours", "3h", now.Add(-3 * time.Hour)},
		{"days", "14d", now.Add(-14 * 24 * time.Hour)},
		{"weeks", "2w", now.Add(-14 * 24 * time.Hour)},
		{"months", "3m", now.AddDate(0, -3, 0)},
		{"years", "1y", now.AddDate(-1, 0, 0)},
		{"single digit unit", "1d", now.Add(-24 * time.Hour)},
		{
			"absolute date is midnight in now's location",
			"2026-01-01",
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSince(now, tt.in)
			require.NoError(t, err)
			assert.True(t, tt.want.Equal(got),
				"ParseSince(%q) = %v, want %v", tt.in, got, tt.want)
		})
	}
}

// TestParseSince_MonthArithmeticCrossesYearBoundary verifies month/year units
// use calendar-aware AddDate rather than a fixed-duration approximation, so
// "2m" from mid-January lands in the previous November/year rather than a
// rough 60-day subtraction.
func TestParseSince_MonthArithmeticCrossesYearBoundary(t *testing.T) {
	now := time.Date(2026, 1, 15, 8, 30, 0, 0, time.UTC)
	got, err := ParseSince(now, "2m")
	require.NoError(t, err)
	want := time.Date(2025, 11, 15, 8, 30, 0, 0, time.UTC)
	assert.True(t, want.Equal(got), "got %v, want %v", got, want)
}

// TestParseSince_AbsoluteDateUsesNowsLocation verifies the YYYY-MM-DD form
// resolves to that date's midnight in now's location rather than always UTC.
func TestParseSince_AbsoluteDateUsesNowsLocation(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, ny)

	got, err := ParseSince(now, "2026-01-01")
	require.NoError(t, err)
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, ny)
	assert.True(t, want.Equal(got), "got %v, want %v", got, want)
	assert.Equal(t, ny.String(), got.Location().String())
}

func TestParseSince_RejectsInvalidForms(t *testing.T) {
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		in   string
	}{
		{"empty string", ""},
		{"unknown unit", "3x"},
		{"unit before number", "m3"},
		{"negative number", "-3d"},
		{"zero is not positive", "0d"},
		{"unit only", "d"},
		{"double unit", "3dd"},
		{"decimal number", "3.5d"},
		{"trailing space", "3d "},
		{"leading space", " 3d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSince(now, tt.in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.in)
		})
	}
}

func TestBestEffortLocalTimezone(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)

	tests := []struct {
		name  string
		envTZ string
		loc   *time.Location
		want  string
	}{
		{
			name:  "valid env wins over local",
			envTZ: "Europe/Berlin",
			loc:   la,
			want:  "Europe/Berlin",
		},
		{
			name:  "invalid env falls back to local",
			envTZ: "not/a-zone",
			loc:   la,
			want:  "America/Los_Angeles",
		},
		{
			name:  "local sentinel is blank",
			envTZ: "",
			loc:   time.FixedZone("Local", 0),
			want:  "",
		},
		{
			name:  "invalid local name is blank",
			envTZ: "",
			loc:   time.FixedZone("DuckLocal", 0),
			want:  "",
		},
		{
			name:  "valid local IANA name passes through",
			envTZ: "",
			loc:   berlin,
			want:  "Europe/Berlin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				bestEffortLocalTimezone(tt.envTZ, tt.loc))
		})
	}
}
