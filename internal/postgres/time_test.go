package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestParseSQLiteTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOK  bool
		wantUTC string
	}{
		{
			"RFC3339Nano",
			"2026-03-11T12:34:56.123456789Z",
			true,
			"2026-03-11T12:34:56.123456789Z",
		},
		{
			"millisecond",
			"2026-03-11T12:34:56.000Z",
			true,
			"2026-03-11T12:34:56Z",
		},
		{
			"second only",
			"2026-03-11T12:34:56Z",
			true,
			"2026-03-11T12:34:56Z",
		},
		{
			"space separated",
			"2026-03-11 12:34:56",
			true,
			"2026-03-11T12:34:56Z",
		},
		{
			"empty string",
			"",
			false,
			"",
		},
		{
			"garbage",
			"not-a-timestamp",
			false,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseSQLiteTimestamp(tt.input)
			require.Equal(t, tt.wantOK, ok)
			if !ok {
				return
			}
			gotStr := got.UTC().Format(time.RFC3339Nano)
			assert.Equal(t, tt.wantUTC, gotStr)
		})
	}
}

func TestFormatISO8601(t *testing.T) {
	ts := time.Date(
		2026, 3, 11, 12, 34, 56, 123456789,
		time.UTC,
	)
	assert.Equal(t, "2026-03-11T12:34:56.123456789Z", FormatISO8601(ts))
}

func TestFormatISO8601NonUTC(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	ts := time.Date(2026, 3, 11, 7, 34, 56, 0, loc)
	assert.Equal(t, "2026-03-11T12:34:56Z", FormatISO8601(ts),
		"should be UTC")
}

func TestNormalizeSyncTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"second precision",
			"2026-03-11T12:34:56Z",
			"2026-03-11T12:34:56.000000Z",
		},
		{
			"nanosecond precision",
			"2026-03-11T12:34:56.123456789Z",
			"2026-03-11T12:34:56.123456Z",
		},
		{
			"empty",
			"",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeSyncTimestamp(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeLocalSyncTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"second precision",
			"2026-03-11T12:34:56Z",
			"2026-03-11T12:34:56.000Z",
		},
		{
			"microsecond precision",
			"2026-03-11T12:34:56.123456Z",
			"2026-03-11T12:34:56.123Z",
		},
		{
			"empty",
			"",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeLocalSyncTimestamp(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeLocalSyncStateTimestamps(t *testing.T) {
	local, err := db.Open(t.TempDir() + "/test.db")
	require.NoError(t, err)
	defer local.Close()

	require.NoError(t, local.SetSyncState(
		"last_push_at",
		"2026-03-11T12:34:56.123456789Z",
	))

	require.NoError(t, NormalizeLocalSyncStateTimestamps(local))

	got, err := local.GetSyncState("last_push_at")
	require.NoError(t, err)
	assert.Equal(t, "2026-03-11T12:34:56.123Z", got)
}
