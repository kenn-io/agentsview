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
