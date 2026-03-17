package postgres

import (
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/db"
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
			if ok != tt.wantOK {
				t.Fatalf(
					"ParseSQLiteTimestamp(%q) ok = %v, "+
						"want %v",
					tt.input, ok, tt.wantOK,
				)
			}
			if !ok {
				return
			}
			gotStr := got.UTC().Format(time.RFC3339Nano)
			if gotStr != tt.wantUTC {
				t.Errorf(
					"ParseSQLiteTimestamp(%q) = %q, "+
						"want %q",
					tt.input, gotStr, tt.wantUTC,
				)
			}
		})
	}
}

func TestFormatISO8601(t *testing.T) {
	ts := time.Date(
		2026, 3, 11, 12, 34, 56, 123456789,
		time.UTC,
	)
	got := FormatISO8601(ts)
	want := "2026-03-11T12:34:56.123456789Z"
	if got != want {
		t.Errorf("FormatISO8601() = %q, want %q", got, want)
	}
}

func TestFormatISO8601NonUTC(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	ts := time.Date(2026, 3, 11, 7, 34, 56, 0, loc)
	got := FormatISO8601(ts)
	want := "2026-03-11T12:34:56Z"
	if got != want {
		t.Errorf(
			"FormatISO8601() = %q, want %q (should be UTC)",
			got, want,
		)
	}
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
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
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
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPreviousLocalSyncTimestamp(t *testing.T) {
	got, err := PreviousLocalSyncTimestamp(
		"2026-03-11T12:34:56.124Z",
	)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	want := "2026-03-11T12:34:56.123Z"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPreviousLocalSyncTimestampEmpty(t *testing.T) {
	got, err := PreviousLocalSyncTimestamp("")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestNormalizeLocalSyncStateTimestamps(t *testing.T) {
	local, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	defer local.Close()

	if err := local.SetSyncState(
		"last_push_at",
		"2026-03-11T12:34:56.123456789Z",
	); err != nil {
		t.Fatalf("SetSyncState: %v", err)
	}

	if err := NormalizeLocalSyncStateTimestamps(local); err != nil {
		t.Fatalf("NormalizeLocalSyncStateTimestamps: %v", err)
	}

	got, err := local.GetSyncState("last_push_at")
	if err != nil {
		t.Fatalf("GetSyncState: %v", err)
	}
	want := "2026-03-11T12:34:56.123Z"
	if got != want {
		t.Errorf("last_push_at = %q, want %q", got, want)
	}
}
