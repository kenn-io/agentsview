package pgsync

import (
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

func TestNormalizeSyncTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "second precision",
			input: "2026-03-11T12:34:56Z",
			want:  "2026-03-11T12:34:56.000000Z",
		},
		{
			name:  "nanosecond precision",
			input: "2026-03-11T12:34:56.123456789Z",
			want:  "2026-03-11T12:34:56.123456Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeSyncTimestamp(tt.input)
			if err != nil {
				t.Fatalf("normalizeSyncTimestamp() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeSyncTimestamp() = %q, want %q", got, tt.want)
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
			name:  "second precision",
			input: "2026-03-11T12:34:56Z",
			want:  "2026-03-11T12:34:56.000Z",
		},
		{
			name:  "microsecond precision",
			input: "2026-03-11T12:34:56.123456Z",
			want:  "2026-03-11T12:34:56.123Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeLocalSyncTimestamp(tt.input)
			if err != nil {
				t.Fatalf("normalizeLocalSyncTimestamp() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeLocalSyncTimestamp() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeLocalSyncStateTimestamps(t *testing.T) {
	local, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	defer local.Close()

	if err := local.SetSyncState("last_push_at", "2026-03-11T12:34:56.123456789Z"); err != nil {
		t.Fatalf("SetSyncState(last_push_at): %v", err)
	}

	if err := normalizeLocalSyncStateTimestamps(local); err != nil {
		t.Fatalf("normalizeLocalSyncStateTimestamps(): %v", err)
	}

	gotPush, err := local.GetSyncState("last_push_at")
	if err != nil {
		t.Fatalf("GetSyncState(last_push_at): %v", err)
	}
	if gotPush != "2026-03-11T12:34:56.123Z" {
		t.Fatalf("last_push_at = %q, want %q", gotPush, "2026-03-11T12:34:56.123Z")
	}
}

func TestPreviousLocalSyncTimestamp(t *testing.T) {
	got, err := previousLocalSyncTimestamp("2026-03-11T12:34:56.124Z")
	if err != nil {
		t.Fatalf("previousLocalSyncTimestamp() error = %v", err)
	}
	if got != "2026-03-11T12:34:56.123Z" {
		t.Fatalf("previousLocalSyncTimestamp() = %q, want %q", got, "2026-03-11T12:34:56.123Z")
	}
}
