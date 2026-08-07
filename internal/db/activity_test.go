package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetSessionActivity(t *testing.T) {
	d := testDB(t)
	sid := "test-activity"

	err := d.UpsertSession(Session{
		ID:        sid,
		Agent:     "claude",
		StartedAt: new("2026-03-26T10:00:00Z"),
	})
	require.NoError(t, err)

	// Insert messages spanning ~29 minutes.
	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "hello", Timestamp: "2026-03-26T10:00:00Z", ContentLength: 5},
		{SessionID: sid, Ordinal: 1, Role: "assistant", Content: "hi", Timestamp: "2026-03-26T10:00:30Z", ContentLength: 2},
		{SessionID: sid, Ordinal: 2, Role: "user", Content: "next", Timestamp: "2026-03-26T10:01:30Z", ContentLength: 4},
		{SessionID: sid, Ordinal: 3, Role: "assistant", Content: "resp", Timestamp: "2026-03-26T10:02:00Z", ContentLength: 4},
		// Gap: no messages from 10:02 to 10:28.
		{SessionID: sid, Ordinal: 4, Role: "user", Content: "back", Timestamp: "2026-03-26T10:28:00Z", ContentLength: 4},
		{SessionID: sid, Ordinal: 5, Role: "assistant", Content: "wb", Timestamp: "2026-03-26T10:29:00Z", ContentLength: 2},
		// System message — should be excluded from counts.
		{SessionID: sid, Ordinal: 6, Role: "user", Content: "This session is being continued from a previous conversation.", Timestamp: "2026-03-26T10:29:30Z", ContentLength: 60, IsSystem: true},
	}
	require.NoError(t, d.InsertMessages(msgs))

	resp, err := d.GetSessionActivity(context.Background(), sid)
	require.NoError(t, err)

	// 29 min span => 1min buckets (snapInterval(1740) = 60).
	assert.Equal(t, int64(60), resp.IntervalSeconds, "interval")

	// System message should still count toward total (7 total messages).
	assert.Equal(t, 7, resp.TotalMessages, "total")

	// Should have 30 buckets (min 0 to min 29).
	assert.GreaterOrEqual(t, len(resp.Buckets), 28, "bucket count")

	// First bucket (10:00-10:01) should have user=1, assistant=1.
	first := resp.Buckets[0]
	assert.Equal(t, 1, first.UserCount, "first bucket user")
	assert.Equal(t, 1, first.AssistantCount, "first bucket asst")
	require.NotNil(t, first.FirstOrdinal, "first bucket first_ordinal")
	assert.Equal(t, 0, *first.FirstOrdinal, "first bucket first_ordinal")

	// Middle empty bucket should have nil FirstOrdinal.
	mid := resp.Buckets[15]
	assert.Equal(t, 0, mid.UserCount, "mid bucket user")
	assert.Equal(t, 0, mid.AssistantCount, "mid bucket asst")
	assert.Nil(t, mid.FirstOrdinal, "mid bucket first_ordinal")
}

func TestGetSessionActivity_NoMessages(t *testing.T) {
	d := testDB(t)
	sid := "test-empty"

	require.NoError(t, d.UpsertSession(Session{ID: sid, Agent: "claude"}))

	resp, err := d.GetSessionActivity(context.Background(), sid)
	require.NoError(t, err)
	assert.Empty(t, resp.Buckets, "buckets")
}

func TestGetSessionActivity_NullTimestamps(t *testing.T) {
	d := testDB(t)
	sid := "test-null-ts"

	require.NoError(t, d.UpsertSession(Session{ID: sid, Agent: "claude"}))

	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2},
		{SessionID: sid, Ordinal: 1, Role: "assistant", Content: "hello", ContentLength: 5},
	}
	require.NoError(t, d.InsertMessages(msgs))

	resp, err := d.GetSessionActivity(context.Background(), sid)
	require.NoError(t, err)
	assert.Empty(t, resp.Buckets, "buckets")
	assert.Equal(t, 2, resp.TotalMessages, "total")
}

func TestGetSessionActivity_SingleMessage(t *testing.T) {
	d := testDB(t)
	sid := "test-single"

	require.NoError(t, d.UpsertSession(Session{ID: sid, Agent: "claude"}))

	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "hi", Timestamp: "2026-03-26T10:00:00Z", ContentLength: 2},
	}
	require.NoError(t, d.InsertMessages(msgs))

	resp, err := d.GetSessionActivity(context.Background(), sid)
	require.NoError(t, err)
	require.Len(t, resp.Buckets, 1, "buckets")
	assert.Equal(t, 1, resp.Buckets[0].UserCount, "user count")
}

func TestGetSessionActivity_MalformedTimestamps(t *testing.T) {
	d := testDB(t)
	sid := "test-malformed-ts"

	require.NoError(t, d.UpsertSession(Session{ID: sid, Agent: "claude"}))

	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "hi", Timestamp: "2026-03-26T10:00:00Z", ContentLength: 2},
		{SessionID: sid, Ordinal: 1, Role: "assistant", Content: "hello", Timestamp: "2026-03-26T10:00:15Z", ContentLength: 5},
		{SessionID: sid, Ordinal: 2, Role: "user", Content: "bye", Timestamp: "2026-03-26T10:00:30Z", ContentLength: 3},
	}
	require.NoError(t, d.InsertMessages(msgs))
	_, err := d.getWriter().Exec(
		"UPDATE messages SET timestamp = 'not-a-timestamp' WHERE session_id = ? AND ordinal = 1",
		sid,
	)
	require.NoError(t, err, "seed malformed legacy timestamp")

	resp, err := d.GetSessionActivity(context.Background(), sid)
	require.NoError(t, err)

	// Malformed timestamp excluded from buckets; valid ones bucketed.
	require.NotEmpty(t, resp.Buckets, "expected at least 1 bucket")
	// Both valid user messages (ord 0 and 2) are within 30s,
	// so they land in the same bucket.
	assert.Equal(t, 2, resp.Buckets[0].UserCount, "first bucket user")
	assert.Equal(t, 0, resp.Buckets[0].AssistantCount, "first bucket asst")
	assert.Equal(t, 3, resp.TotalMessages, "total")
}

func TestGetSessionActivity_FractionalTimestamps(t *testing.T) {
	d := testDB(t)
	sid := "test-frac-ts"

	require.NoError(t, d.UpsertSession(Session{ID: sid, Agent: "claude"}))

	// Two messages within the same 60s bucket but with fractional
	// timestamps that would be mis-bucketed by whole-second truncation.
	// 10:00:00.900 and 10:00:59.100 are 58.2s apart — same 60s bucket.
	msgs := []Message{
		{SessionID: sid, Ordinal: 0, Role: "user", Content: "a", Timestamp: "2026-03-26T10:00:00.900Z", ContentLength: 1},
		{SessionID: sid, Ordinal: 1, Role: "assistant", Content: "b", Timestamp: "2026-03-26T10:00:59.100Z", ContentLength: 1},
		// This message is in the next bucket (60.1s after the anchor).
		{SessionID: sid, Ordinal: 2, Role: "user", Content: "c", Timestamp: "2026-03-26T10:01:01.000Z", ContentLength: 1},
	}
	require.NoError(t, d.InsertMessages(msgs))

	resp, err := d.GetSessionActivity(context.Background(), sid)
	require.NoError(t, err)

	require.Equal(t, int64(60), resp.IntervalSeconds, "interval")

	// First bucket should have both fractional-second messages.
	require.NotEmpty(t, resp.Buckets, "expected at least 1 bucket")
	first := resp.Buckets[0]
	assert.Equal(t, 1, first.UserCount, "first bucket user")
	assert.Equal(t, 1, first.AssistantCount, "first bucket asst")

	// Second bucket should have the third message.
	require.GreaterOrEqual(t, len(resp.Buckets), 2, "expected at least 2 buckets")
	second := resp.Buckets[1]
	assert.Equal(t, 1, second.UserCount, "second bucket user")
}

func TestSnapInterval(t *testing.T) {
	tests := []struct {
		name     string
		duration int64 // seconds
		want     int64
	}{
		{"30s session", 30, 60},
		{"5m session", 300, 60},
		{"10m session", 600, 60},
		{"20m session", 1200, 60},
		{"30m session", 1800, 60},
		{"1h session", 3600, 120},
		{"2h session", 7200, 300},
		{"4h session", 14400, 600},
		{"8h session", 28800, 900},
		{"12h session", 43200, 1800},
		{"16h session", 57600, 1800},
		{"24h session", 86400, 3600},
		{"48h session", 172800, 7200},
		// Extreme: 30 days. 7200s would give 361 buckets,
		// so interval scales up to keep count <= 50.
		// ceil(2592000 / 49) = 52898
		{"30d session", 2592000, 52898},
		{"0s session", 0, 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SnapInterval(tt.duration)
			assert.Equal(t, tt.want, got, "SnapInterval(%d)", tt.duration)
		})
	}
}
