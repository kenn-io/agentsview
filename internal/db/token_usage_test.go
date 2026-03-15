// ABOUTME: Tests for token usage storage in sessions and messages.
// ABOUTME: Verifies migration, insert, and retrieval of token fields.
package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestMigrationAddsTokenColumns(t *testing.T) {
	d := testDB(t)
	w := d.getWriter()

	// Verify message token columns exist by querying pragma.
	for _, col := range []string{
		"model", "token_usage",
		"context_tokens", "output_tokens",
	} {
		var count int
		err := w.QueryRow(
			"SELECT count(*) FROM pragma_table_info('messages')"+
				" WHERE name = ?", col,
		).Scan(&count)
		requireNoError(t, err, "probing messages."+col)
		if count != 1 {
			t.Errorf("expected messages.%s to exist", col)
		}
	}

	// Verify session token columns exist.
	for _, col := range []string{
		"total_output_tokens", "peak_context_tokens",
	} {
		var count int
		err := w.QueryRow(
			"SELECT count(*) FROM pragma_table_info('sessions')"+
				" WHERE name = ?", col,
		).Scan(&count)
		requireNoError(t, err, "probing sessions."+col)
		if count != 1 {
			t.Errorf("expected sessions.%s to exist", col)
		}
	}
}

func TestMigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	d1, err := Open(path)
	requireNoError(t, err, "first open")
	d1.Close()

	// Re-open should not fail even though columns already exist.
	d2, err := Open(path)
	requireNoError(t, err, "second open")
	d2.Close()
}

func TestInsertAndGetMessagesTokenUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "proj")

	msgs := []Message{
		{
			SessionID:     "s1",
			Ordinal:       0,
			Role:          "user",
			Content:       "hello",
			ContentLength: 5,
			Model:         "claude-sonnet-4-20250514",
			TokenUsage:    json.RawMessage(`{"input":100,"output":0}`),
			ContextTokens: 500,
			OutputTokens:  0,
		},
		{
			SessionID:     "s1",
			Ordinal:       1,
			Role:          "assistant",
			Content:       "world",
			ContentLength: 5,
			Model:         "claude-sonnet-4-20250514",
			TokenUsage:    json.RawMessage(`{"input":0,"output":200}`),
			ContextTokens: 600,
			OutputTokens:  200,
		},
	}
	insertMessages(t, d, msgs...)

	got, err := d.GetMessages(ctx, "s1", 0, 100, true)
	requireNoError(t, err, "GetMessages")

	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}

	// Verify first message fields.
	if got[0].Model != "claude-sonnet-4-20250514" {
		t.Errorf("msg[0].Model = %q, want %q",
			got[0].Model, "claude-sonnet-4-20250514")
	}
	if string(got[0].TokenUsage) != `{"input":100,"output":0}` {
		t.Errorf("msg[0].TokenUsage = %q, want %q",
			string(got[0].TokenUsage), `{"input":100,"output":0}`)
	}
	if got[0].ContextTokens != 500 {
		t.Errorf("msg[0].ContextTokens = %d, want 500",
			got[0].ContextTokens)
	}

	// Verify second message fields.
	if got[1].OutputTokens != 200 {
		t.Errorf("msg[1].OutputTokens = %d, want 200",
			got[1].OutputTokens)
	}
	if got[1].ContextTokens != 600 {
		t.Errorf("msg[1].ContextTokens = %d, want 600",
			got[1].ContextTokens)
	}
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
	requireNoError(t, err, "GetAllMessages")

	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].Model != "gpt-4o" {
		t.Errorf("Model = %q, want %q", got[0].Model, "gpt-4o")
	}
	if got[0].OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150", got[0].OutputTokens)
	}
	if got[0].ContextTokens != 300 {
		t.Errorf("ContextTokens = %d, want 300", got[0].ContextTokens)
	}
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
	requireNoError(t, err, "GetMessageByOrdinal")
	if m == nil {
		t.Fatal("expected message, got nil")
	}
	if m.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want %q",
			m.Model, "claude-sonnet-4-20250514")
	}
	if string(m.TokenUsage) != `{"cache_read":42}` {
		t.Errorf("TokenUsage = %q, want %q",
			string(m.TokenUsage), `{"cache_read":42}`)
	}
	if m.ContextTokens != 250 {
		t.Errorf("ContextTokens = %d, want 250",
			m.ContextTokens)
	}
	if m.OutputTokens != 99 {
		t.Errorf("OutputTokens = %d, want 99", m.OutputTokens)
	}
}

func TestUpsertSessionTokenUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	s := Session{
		ID:                "s1",
		Project:           "proj",
		Machine:           defaultMachine,
		Agent:             defaultAgent,
		MessageCount:      5,
		TotalOutputTokens: 2000,
		PeakContextTokens: 8000,
	}
	requireNoError(t, d.UpsertSession(s), "upsert")

	got, err := d.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession")
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.TotalOutputTokens != 2000 {
		t.Errorf("TotalOutputTokens = %d, want 2000",
			got.TotalOutputTokens)
	}
	if got.PeakContextTokens != 8000 {
		t.Errorf("PeakContextTokens = %d, want 8000",
			got.PeakContextTokens)
	}

	// Update with new token values.
	s.TotalOutputTokens = 2500
	s.PeakContextTokens = 9000
	requireNoError(t, d.UpsertSession(s), "upsert update")

	got, err = d.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession after update")
	if got.TotalOutputTokens != 2500 {
		t.Errorf("TotalOutputTokens after update = %d, want 2500",
			got.TotalOutputTokens)
	}
	if got.PeakContextTokens != 9000 {
		t.Errorf("PeakContextTokens after update = %d, want 9000",
			got.PeakContextTokens)
	}
}

func TestSessionTokenUsageDefaultsToZero(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Insert session without setting token fields.
	insertSession(t, d, "s1", "proj")

	got, err := d.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession")
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.TotalOutputTokens != 0 {
		t.Errorf("TotalOutputTokens = %d, want 0",
			got.TotalOutputTokens)
	}
	if got.PeakContextTokens != 0 {
		t.Errorf("PeakContextTokens = %d, want 0",
			got.PeakContextTokens)
	}
}

func TestMessageTokenUsageDefaultsToZero(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "proj")
	// Insert message without setting token fields.
	insertMessages(t, d, userMsg("s1", 0, "hello"))

	got, err := d.GetMessages(ctx, "s1", 0, 100, true)
	requireNoError(t, err, "GetMessages")
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].Model != "" {
		t.Errorf("Model = %q, want empty", got[0].Model)
	}
	if len(got[0].TokenUsage) != 0 {
		t.Errorf("TokenUsage = %q, want empty",
			string(got[0].TokenUsage))
	}
	if got[0].ContextTokens != 0 {
		t.Errorf("ContextTokens = %d, want 0",
			got[0].ContextTokens)
	}
	if got[0].OutputTokens != 0 {
		t.Errorf("OutputTokens = %d, want 0",
			got[0].OutputTokens)
	}
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
	requireNoError(t, d.UpsertSession(s), "upsert")

	got, err := d.GetSessionFull(ctx, "s1")
	requireNoError(t, err, "GetSessionFull")
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.TotalOutputTokens != 600 {
		t.Errorf("TotalOutputTokens = %d, want 600",
			got.TotalOutputTokens)
	}
	if got.PeakContextTokens != 4000 {
		t.Errorf("PeakContextTokens = %d, want 4000",
			got.PeakContextTokens)
	}
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
		SessionID:     "s1",
		Ordinal:       0,
		Role:          "user",
		Content:       "new",
		ContentLength: 3,
		Model:         "claude-sonnet-4-20250514",
		TokenUsage:    json.RawMessage(`{"input":999,"output":888}`),
		ContextTokens: 700,
		OutputTokens:  888,
	}}
	requireNoError(t,
		d.ReplaceSessionMessages("s1", newMsgs),
		"ReplaceSessionMessages",
	)

	got, err := d.GetMessages(ctx, "s1", 0, 100, true)
	requireNoError(t, err, "GetMessages after replace")
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want %q",
			got[0].Model, "claude-sonnet-4-20250514")
	}
	if string(got[0].TokenUsage) != `{"input":999,"output":888}` {
		t.Errorf("TokenUsage = %q, want %q",
			string(got[0].TokenUsage), `{"input":999,"output":888}`)
	}
	if got[0].ContextTokens != 700 {
		t.Errorf("ContextTokens = %d, want 700",
			got[0].ContextTokens)
	}
	if got[0].OutputTokens != 888 {
		t.Errorf("OutputTokens = %d, want 888",
			got[0].OutputTokens)
	}
}

func TestListSessionsTokenUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	s := Session{
		ID:                "s1",
		Project:           "proj",
		Machine:           defaultMachine,
		Agent:             defaultAgent,
		MessageCount:      2,
		TotalOutputTokens: 222,
		PeakContextTokens: 5000,
	}
	requireNoError(t, d.UpsertSession(s), "upsert")

	page, err := d.ListSessions(ctx, SessionFilter{})
	requireNoError(t, err, "ListSessions")
	if len(page.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d",
			len(page.Sessions))
	}
	got := page.Sessions[0]
	if got.TotalOutputTokens != 222 {
		t.Errorf("TotalOutputTokens = %d, want 222",
			got.TotalOutputTokens)
	}
	if got.PeakContextTokens != 5000 {
		t.Errorf("PeakContextTokens = %d, want 5000",
			got.PeakContextTokens)
	}
}

func TestIncrementalUpdatePreservesTokenTotals(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	s := Session{
		ID:                "inc-tokens",
		Project:           "proj",
		Machine:           "test",
		Agent:             "claude",
		MessageCount:      5,
		UserMessageCount:  2,
		TotalOutputTokens: 1000,
		PeakContextTokens: 8000,
		FilePath:          Ptr("/tmp/s.jsonl"),
		FileSize:          Ptr(int64(2048)),
		FileMtime:         Ptr(int64(100)),
	}
	requireNoError(t, d.UpsertSession(s), "upsert")

	t.Run("metadata-only update preserves tokens", func(t *testing.T) {
		// Simulate a no-new-messages incremental update that
		// only advances file_size and ended_at. Token totals
		// must be carried forward, not reset to zero.
		ended := "2024-01-15T10:30:00Z"
		err := d.UpdateSessionIncremental(
			"inc-tokens", &ended, 5, 2, 4096, 200,
			1000, 8000,
		)
		requireNoError(t, err, "incremental update")

		got, err := d.GetSessionFull(ctx, "inc-tokens")
		requireNoError(t, err, "get session")
		if got.TotalOutputTokens != 1000 {
			t.Errorf(
				"TotalOutputTokens = %d, want 1000",
				got.TotalOutputTokens,
			)
		}
		if got.PeakContextTokens != 8000 {
			t.Errorf(
				"PeakContextTokens = %d, want 8000",
				got.PeakContextTokens,
			)
		}
	})

	t.Run("update with new messages advances tokens", func(t *testing.T) {
		ended := "2024-01-15T11:00:00Z"
		err := d.UpdateSessionIncremental(
			"inc-tokens", &ended, 8, 3, 8192, 300,
			1500, 9000,
		)
		requireNoError(t, err, "incremental update")

		got, err := d.GetSessionFull(ctx, "inc-tokens")
		requireNoError(t, err, "get session")
		if got.TotalOutputTokens != 1500 {
			t.Errorf(
				"TotalOutputTokens = %d, want 1500",
				got.TotalOutputTokens,
			)
		}
		if got.PeakContextTokens != 9000 {
			t.Errorf(
				"PeakContextTokens = %d, want 9000",
				got.PeakContextTokens,
			)
		}
	})

	t.Run("idempotent retry does not inflate tokens", func(t *testing.T) {
		// Same call again simulates a retry — absolute values
		// should produce the same result.
		ended := "2024-01-15T11:00:00Z"
		err := d.UpdateSessionIncremental(
			"inc-tokens", &ended, 8, 3, 8192, 300,
			1500, 9000,
		)
		requireNoError(t, err, "retry update")

		got, err := d.GetSessionFull(ctx, "inc-tokens")
		requireNoError(t, err, "get session")
		if got.TotalOutputTokens != 1500 {
			t.Errorf(
				"TotalOutputTokens = %d, want 1500"+
					" (retry inflated)",
				got.TotalOutputTokens,
			)
		}
	})
}
