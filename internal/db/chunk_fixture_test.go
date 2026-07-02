package db

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const chunkedAnalyticsFixtureSessionCount = maxSQLVars + 1

var (
	chunkedAnalyticsOnce sync.Once
	chunkedAnalyticsDir  string
	chunkedAnalyticsPath string
)

func openChunkedAnalyticsFixtureDB(t *testing.T) *DB {
	t.Helper()

	chunkedAnalyticsOnce.Do(func() {
		chunkedAnalyticsDir, chunkedAnalyticsPath =
			buildChunkedAnalyticsFixtureTemplate(t)
	})

	dst := filepath.Join(t.TempDir(), "test.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		require.NoError(t,
			copyTemplateDBFile(
				chunkedAnalyticsPath+suffix, dst+suffix, suffix == "",
			),
			"copy chunked analytics fixture %q", suffix)
	}
	d, err := OpenPreparedTestDB(dst)
	require.NoError(t, err, "open chunked analytics fixture")
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	return d
}

func buildChunkedAnalyticsFixtureTemplate(t *testing.T) (string, string) {
	t.Helper()

	dir, err := os.MkdirTemp("", "agentsview-chunked-analytics-*")
	require.NoError(t, err, "create chunked analytics fixture dir")
	path := filepath.Join(dir, "test.db")
	require.NoError(t, copyTestDBTemplate(path),
		"copy base db template for chunked analytics fixture")

	d, err := OpenPreparedTestDB(path)
	require.NoError(t, err, "open chunked analytics template")
	seedChunkedAnalyticsFixture(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, d.CheckpointWALTruncate(ctx),
		"checkpoint chunked analytics template")
	require.NoError(t, d.Close(), "close chunked analytics template")
	return dir, path
}

func seedChunkedAnalyticsFixture(t *testing.T, d *DB) {
	t.Helper()

	const startedAt = "2024-06-01T09:00:00Z"
	const endedAt = "2024-06-01T09:01:00Z"
	writes := make([]SessionBatchWrite, 0, chunkedAnalyticsFixtureSessionCount)
	for i := range chunkedAnalyticsFixtureSessionCount {
		id := "chunk-" + itoa(i)
		writes = append(writes, SessionBatchWrite{
			Session: Session{
				ID:               id,
				Project:          "proj",
				Machine:          defaultMachine,
				Agent:            "claude",
				MessageCount:     2,
				UserMessageCount: 1,
				StartedAt:        Ptr(startedAt),
				EndedAt:          Ptr(endedAt),
				FirstMessage:     Ptr("q"),
			},
			Messages: []Message{
				{
					SessionID: id, Ordinal: 0, Role: "user",
					Content: "q", ContentLength: 1,
					Timestamp: startedAt,
				},
				{
					SessionID: id, Ordinal: 1,
					Role:          "assistant",
					Content:       "a",
					ContentLength: 1,
					Timestamp:     "2024-06-01T09:00:10Z",
					Model:         "claude-sonnet-4-20250514",
					TokenUsage: json.RawMessage(
						`{"input_tokens":100,"output_tokens":10}`,
					),
				},
			},
		})
	}
	result, err := d.WriteSessionBatchAtomic(writes)
	require.NoError(t, err, "WriteSessionBatchAtomic chunked fixture")
	require.Equal(t, chunkedAnalyticsFixtureSessionCount,
		result.WrittenSessions, "WrittenSessions")
	require.Equal(t, 2*chunkedAnalyticsFixtureSessionCount,
		result.WrittenMessages, "WrittenMessages")
}
