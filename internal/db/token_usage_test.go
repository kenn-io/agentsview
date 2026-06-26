// ABOUTME: Tests for token usage storage in sessions and messages.
// ABOUTME: Verifies migration, insert, and retrieval of token fields.
package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationAddsTokenColumns(t *testing.T) {
	d := testDB(t)
	w := d.getWriter()

	// Verify message token columns exist by querying pragma.
	for _, col := range []string{
		"model", "token_usage",
		"context_tokens", "output_tokens",
		"has_context_tokens", "has_output_tokens",
	} {
		var count int
		err := w.QueryRow(
			"SELECT count(*) FROM pragma_table_info('messages')"+
				" WHERE name = ?", col,
		).Scan(&count)
		require.NoError(t, err, "probing messages.%s", col)
		assert.Equal(t, 1, count, "expected messages.%s to exist", col)
	}

	// Verify session token columns exist.
	for _, col := range []string{
		"total_output_tokens", "peak_context_tokens",
		"has_total_output_tokens", "has_peak_context_tokens",
	} {
		var count int
		err := w.QueryRow(
			"SELECT count(*) FROM pragma_table_info('sessions')"+
				" WHERE name = ?", col,
		).Scan(&count)
		require.NoError(t, err, "probing sessions.%s", col)
		assert.Equal(t, 1, count, "expected sessions.%s to exist", col)
	}
}

func TestMigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	d1, err := Open(path)
	require.NoError(t, err, "first open")
	d1.Close()

	// Re-open should not fail even though columns already exist.
	d2, err := Open(path)
	require.NoError(t, err, "second open")
	d2.Close()
}

func TestInsertAndGetMessagesTokenUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "proj")

	msgs := []Message{
		{
			SessionID:        "s1",
			Ordinal:          0,
			Role:             "user",
			Content:          "hello",
			ContentLength:    5,
			Model:            "claude-sonnet-4-20250514",
			TokenUsage:       json.RawMessage(`{"input":100,"output":0}`),
			ContextTokens:    500,
			OutputTokens:     0,
			HasContextTokens: true,
			HasOutputTokens:  true,
		},
		{
			SessionID:        "s1",
			Ordinal:          1,
			Role:             "assistant",
			Content:          "world",
			ContentLength:    5,
			Model:            "claude-sonnet-4-20250514",
			TokenUsage:       json.RawMessage(`{"input":0,"output":200}`),
			ContextTokens:    600,
			OutputTokens:     200,
			HasContextTokens: true,
			HasOutputTokens:  true,
		},
	}
	insertMessages(t, d, msgs...)

	got, err := d.GetMessages(ctx, "s1", 0, 100, true)
	require.NoError(t, err, "GetMessages")

	require.Len(t, got, 2)

	// Verify first message fields.
	assert.Equal(t, "claude-sonnet-4-20250514", got[0].Model, "msg[0].Model")
	assert.Equal(t, `{"input":100,"output":0}`, string(got[0].TokenUsage),
		"msg[0].TokenUsage")
	assert.Equal(t, 500, got[0].ContextTokens, "msg[0].ContextTokens")
	assert.True(t, got[0].HasContextTokens, "msg[0].HasContextTokens")
	assert.True(t, got[0].HasOutputTokens, "msg[0].HasOutputTokens")

	// Verify second message fields.
	assert.Equal(t, 200, got[1].OutputTokens, "msg[1].OutputTokens")
	assert.Equal(t, 600, got[1].ContextTokens, "msg[1].ContextTokens")
	assert.True(t, got[1].HasContextTokens, "msg[1].HasContextTokens")
	assert.True(t, got[1].HasOutputTokens, "msg[1].HasOutputTokens")
}

func TestGetAllMessagesTokenUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID:     "s1",
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hi",
		ContentLength: 2,
		Model:         "gpt-4o",
		TokenUsage:    json.RawMessage(`{"input":50,"output":150}`),
		ContextTokens: 300,
		OutputTokens:  150,
	})

	got, err := d.GetAllMessages(ctx, "s1")
	require.NoError(t, err, "GetAllMessages")

	require.Len(t, got, 1)
	assert.Equal(t, "gpt-4o", got[0].Model, "Model")
	assert.Equal(t, 150, got[0].OutputTokens, "OutputTokens")
	assert.Equal(t, 300, got[0].ContextTokens, "ContextTokens")
}

func TestGetMessageByOrdinalTokenUsage(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID:     "s1",
		Ordinal:       0,
		Role:          "user",
		Content:       "test",
		ContentLength: 4,
		Model:         "claude-sonnet-4-20250514",
		TokenUsage:    json.RawMessage(`{"cache_read":42}`),
		ContextTokens: 250,
		OutputTokens:  99,
	})

	m, err := d.GetMessageByOrdinal("s1", 0)
	require.NoError(t, err, "GetMessageByOrdinal")
	require.NotNil(t, m, "expected message")
	assert.Equal(t, "claude-sonnet-4-20250514", m.Model, "Model")
	assert.Equal(t, `{"cache_read":42}`, string(m.TokenUsage), "TokenUsage")
	assert.Equal(t, 250, m.ContextTokens, "ContextTokens")
	assert.Equal(t, 99, m.OutputTokens, "OutputTokens")
}

func TestUpsertSessionTokenUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	s := Session{
		ID:                   "s1",
		Project:              "proj",
		Machine:              defaultMachine,
		Agent:                defaultAgent,
		MessageCount:         5,
		TotalOutputTokens:    2000,
		PeakContextTokens:    8000,
		HasTotalOutputTokens: true,
		HasPeakContextTokens: true,
	}
	require.NoError(t, d.UpsertSession(s), "upsert")

	got, err := d.GetSession(ctx, "s1")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, got, "expected session")
	assert.Equal(t, 2000, got.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 8000, got.PeakContextTokens, "PeakContextTokens")
	assert.True(t, got.HasTotalOutputTokens, "HasTotalOutputTokens")
	assert.True(t, got.HasPeakContextTokens, "HasPeakContextTokens")

	// Update with new token values.
	s.TotalOutputTokens = 2500
	s.PeakContextTokens = 9000
	require.NoError(t, d.UpsertSession(s), "upsert update")

	got, err = d.GetSession(ctx, "s1")
	require.NoError(t, err, "GetSession after update")
	assert.Equal(t, 2500, got.TotalOutputTokens, "TotalOutputTokens after update")
	assert.Equal(t, 9000, got.PeakContextTokens, "PeakContextTokens after update")
	assert.True(t, got.HasTotalOutputTokens, "HasTotalOutputTokens after update")
	assert.True(t, got.HasPeakContextTokens, "HasPeakContextTokens after update")
}

func TestSessionTokenUsageDefaultsToZero(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Insert session without setting token fields.
	insertSession(t, d, "s1", "proj")

	got, err := d.GetSession(ctx, "s1")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, got, "expected session")
	assert.Equal(t, 0, got.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 0, got.PeakContextTokens, "PeakContextTokens")
	assert.False(t, got.HasTotalOutputTokens, "HasTotalOutputTokens")
	assert.False(t, got.HasPeakContextTokens, "HasPeakContextTokens")
}

func TestMessageTokenUsageDefaultsToZero(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "proj")
	// Insert message without setting token fields.
	insertMessages(t, d, userMsg("s1", 0, "hello"))

	got, err := d.GetMessages(ctx, "s1", 0, 100, true)
	require.NoError(t, err, "GetMessages")
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Model, "Model")
	assert.Empty(t, got[0].TokenUsage, "TokenUsage")
	assert.Equal(t, 0, got[0].ContextTokens, "ContextTokens")
	assert.Equal(t, 0, got[0].OutputTokens, "OutputTokens")
	assert.False(t, got[0].HasContextTokens, "HasContextTokens")
	assert.False(t, got[0].HasOutputTokens, "HasOutputTokens")
}

func TestGetSessionFullTokenUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	s := Session{
		ID:                "s1",
		Project:           "proj",
		Machine:           defaultMachine,
		Agent:             defaultAgent,
		MessageCount:      1,
		TotalOutputTokens: 600,
		PeakContextTokens: 4000,
	}
	require.NoError(t, d.UpsertSession(s), "upsert")

	got, err := d.GetSessionFull(ctx, "s1")
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, got, "expected session")
	assert.Equal(t, 600, got.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 4000, got.PeakContextTokens, "PeakContextTokens")
}

func TestReplaceSessionMessagesTokenUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID:     "s1",
		Ordinal:       0,
		Role:          "user",
		Content:       "old",
		ContentLength: 3,
		OutputTokens:  10,
	})

	// Replace with new messages that have different token values.
	newMsgs := []Message{{
		SessionID:        "s1",
		Ordinal:          0,
		Role:             "user",
		Content:          "new",
		ContentLength:    3,
		Model:            "claude-sonnet-4-20250514",
		TokenUsage:       json.RawMessage(`{"input":999,"output":888}`),
		ContextTokens:    700,
		OutputTokens:     888,
		HasContextTokens: true,
		HasOutputTokens:  true,
	}}
	require.NoError(t,
		d.ReplaceSessionMessages("s1", newMsgs),
		"ReplaceSessionMessages",
	)

	got, err := d.GetMessages(ctx, "s1", 0, 100, true)
	require.NoError(t, err, "GetMessages after replace")
	require.Len(t, got, 1)
	assert.Equal(t, "claude-sonnet-4-20250514", got[0].Model, "Model")
	assert.Equal(t, `{"input":999,"output":888}`, string(got[0].TokenUsage),
		"TokenUsage")
	assert.Equal(t, 700, got[0].ContextTokens, "ContextTokens")
	assert.Equal(t, 888, got[0].OutputTokens, "OutputTokens")
	assert.True(t, got[0].HasContextTokens, "HasContextTokens")
	assert.True(t, got[0].HasOutputTokens, "HasOutputTokens")
}

func TestListSessionsTokenUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	s := Session{
		ID:                   "s1",
		Project:              "proj",
		Machine:              defaultMachine,
		Agent:                defaultAgent,
		MessageCount:         2,
		TotalOutputTokens:    222,
		PeakContextTokens:    5000,
		HasTotalOutputTokens: true,
		HasPeakContextTokens: true,
	}
	require.NoError(t, d.UpsertSession(s), "upsert")

	page, err := d.ListSessions(ctx, SessionFilter{})
	require.NoError(t, err, "ListSessions")
	require.Len(t, page.Sessions, 1)
	got := page.Sessions[0]
	assert.Equal(t, 222, got.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 5000, got.PeakContextTokens, "PeakContextTokens")
	assert.True(t, got.HasTotalOutputTokens, "HasTotalOutputTokens")
	assert.True(t, got.HasPeakContextTokens, "HasPeakContextTokens")
}

func TestIncrementalUpdatePreservesTokenTotals(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	s := Session{
		ID:                   "inc-tokens",
		Project:              "proj",
		Machine:              "test",
		Agent:                "claude",
		MessageCount:         5,
		UserMessageCount:     2,
		TotalOutputTokens:    1000,
		PeakContextTokens:    8000,
		HasTotalOutputTokens: true,
		HasPeakContextTokens: true,
		FilePath:             new("/tmp/s.jsonl"),
		FileSize:             new(int64(2048)),
		FileMtime:            new(int64(100)),
	}
	require.NoError(t, d.UpsertSession(s), "upsert")

	t.Run("metadata-only update preserves tokens", func(t *testing.T) {
		// Simulate a no-new-messages incremental update that
		// only advances file_size and ended_at. Token totals
		// must be carried forward, not reset to zero.
		ended := "2024-01-15T10:30:00Z"
		err := callUpdateSessionIncrementalCompat(
			t,
			d,
			"inc-tokens",
			&ended,
			5,
			2,
			4096,
			200,
			0,
			"",
			1000,
			8000,
			true,
			true,
		)
		require.NoError(t, err, "incremental update")

		got, err := d.GetSessionFull(ctx, "inc-tokens")
		require.NoError(t, err, "get session")
		assert.Equal(t, 1000, got.TotalOutputTokens, "TotalOutputTokens")
		assert.Equal(t, 8000, got.PeakContextTokens, "PeakContextTokens")
		assert.True(t, got.HasTotalOutputTokens, "HasTotalOutputTokens")
		assert.True(t, got.HasPeakContextTokens, "HasPeakContextTokens")
	})

	t.Run("update with new messages advances tokens", func(t *testing.T) {
		ended := "2024-01-15T11:00:00Z"
		err := callUpdateSessionIncrementalCompat(
			t,
			d,
			"inc-tokens",
			&ended,
			8,
			3,
			8192,
			300,
			0,
			"",
			1500,
			9000,
			true,
			true,
		)
		require.NoError(t, err, "incremental update")

		got, err := d.GetSessionFull(ctx, "inc-tokens")
		require.NoError(t, err, "get session")
		assert.Equal(t, 1500, got.TotalOutputTokens, "TotalOutputTokens")
		assert.Equal(t, 9000, got.PeakContextTokens, "PeakContextTokens")
		assert.True(t, got.HasTotalOutputTokens, "HasTotalOutputTokens")
		assert.True(t, got.HasPeakContextTokens, "HasPeakContextTokens")
	})

	t.Run("idempotent retry does not inflate tokens", func(t *testing.T) {
		// Same call again simulates a retry — absolute values
		// should produce the same result.
		ended := "2024-01-15T11:00:00Z"
		err := callUpdateSessionIncrementalCompat(
			t,
			d,
			"inc-tokens",
			&ended,
			8,
			3,
			8192,
			300,
			0,
			"",
			1500,
			9000,
			true,
			true,
		)
		require.NoError(t, err, "retry update")

		got, err := d.GetSessionFull(ctx, "inc-tokens")
		require.NoError(t, err, "get session")
		assert.Equal(t, 1500, got.TotalOutputTokens,
			"TotalOutputTokens (retry inflated)")
		assert.True(t, got.HasTotalOutputTokens, "HasTotalOutputTokens")
		assert.True(t, got.HasPeakContextTokens, "HasPeakContextTokens")
	})
}
