//go:build pgtest

package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

func TestStoreSessionAndMessageTokenUsage(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	_, err = pg.Exec(`
		INSERT INTO sessions (
			id, machine, project, agent,
			message_count, user_message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		) VALUES (
			'token-store-001', 'test-machine', 'test-project', 'claude',
			1, 1, 500, 900, TRUE, TRUE
		)
		ON CONFLICT (id) DO UPDATE SET
			total_output_tokens = EXCLUDED.total_output_tokens,
			peak_context_tokens = EXCLUDED.peak_context_tokens,
			has_total_output_tokens = EXCLUDED.has_total_output_tokens,
			has_peak_context_tokens = EXCLUDED.has_peak_context_tokens
	`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages (
			session_id, ordinal, role, content,
			content_length, model, token_usage,
			context_tokens, output_tokens,
			has_context_tokens, has_output_tokens
		) VALUES (
			'token-store-001', 0, 'assistant', 'hello',
			5, 'claude-sonnet-4-20250514', '{"output_tokens":200}',
			900, 200, TRUE, TRUE
		)
		ON CONFLICT (session_id, ordinal) DO UPDATE SET
			context_tokens = EXCLUDED.context_tokens,
			output_tokens = EXCLUDED.output_tokens,
			has_context_tokens = EXCLUDED.has_context_tokens,
			has_output_tokens = EXCLUDED.has_output_tokens
	`)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetSession(ctx, "token-store-001")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session")
	}
	if !sess.HasTotalOutputTokens {
		t.Error("HasTotalOutputTokens = false, want true")
	}
	if !sess.HasPeakContextTokens {
		t.Error("HasPeakContextTokens = false, want true")
	}
	if sess.TotalOutputTokens != 500 {
		t.Errorf("TotalOutputTokens = %d, want 500", sess.TotalOutputTokens)
	}

	msgs, err := store.GetMessages(ctx, "token-store-001", 0, 10, true)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("GetMessages len = %d, want 1", len(msgs))
	}
	if !msgs[0].HasOutputTokens {
		t.Error("HasOutputTokens = false, want true")
	}
	if !msgs[0].HasContextTokens {
		t.Error("HasContextTokens = false, want true")
	}
	if msgs[0].OutputTokens != 200 {
		t.Errorf("OutputTokens = %d, want 200", msgs[0].OutputTokens)
	}
}

func TestPushTokenUsageToPostgres(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "test-machine", true, nil, nil)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	sess := db.Session{
		ID:                   "token-push-001",
		Project:              "proj",
		Machine:              "local",
		Agent:                "claude",
		MessageCount:         1,
		UserMessageCount:     0,
		TotalOutputTokens:    500,
		PeakContextTokens:    900,
		HasTotalOutputTokens: true,
		HasPeakContextTokens: true,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := local.InsertMessages([]db.Message{{
		SessionID:        "token-push-001",
		Ordinal:          0,
		Role:             "assistant",
		Content:          "hello",
		ContentLength:    5,
		Model:            "claude-sonnet-4-20250514",
		TokenUsage:       json.RawMessage(`{"output_tokens":200}`),
		ContextTokens:    900,
		OutputTokens:     200,
		HasContextTokens: true,
		HasOutputTokens:  true,
	}}); err != nil {
		t.Fatalf("InsertMessages: %v", err)
	}

	if _, err := ps.Push(ctx, false); err != nil {
		t.Fatalf("Push: %v", err)
	}

	store, err := NewStore(pgURL, "agentsview", true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	gotSess, err := store.GetSession(ctx, "token-push-001")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if gotSess == nil {
		t.Fatal("expected pushed session")
	}
	if !gotSess.HasTotalOutputTokens {
		t.Error("HasTotalOutputTokens = false, want true")
	}
	if !gotSess.HasPeakContextTokens {
		t.Error("HasPeakContextTokens = false, want true")
	}
	if gotSess.TotalOutputTokens != 500 {
		t.Errorf("TotalOutputTokens = %d, want 500", gotSess.TotalOutputTokens)
	}

	gotMsgs, err := store.GetMessages(ctx, "token-push-001", 0, 10, true)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(gotMsgs) != 1 {
		t.Fatalf("GetMessages len = %d, want 1", len(gotMsgs))
	}
	if !gotMsgs[0].HasContextTokens {
		t.Error("HasContextTokens = false, want true")
	}
	if !gotMsgs[0].HasOutputTokens {
		t.Error("HasOutputTokens = false, want true")
	}
	if gotMsgs[0].OutputTokens != 200 {
		t.Errorf("OutputTokens = %d, want 200", gotMsgs[0].OutputTokens)
	}
}

func TestEnsureSchemaBackfillsTokenCoverageFlags(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	pg, err := Open(pgURL, "agentsview", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()

	ctx := context.Background()
	if err := EnsureSchema(ctx, pg, "agentsview"); err != nil {
		t.Fatalf("EnsureSchema initial: %v", err)
	}

	_, err = pg.Exec(`
		INSERT INTO sessions (
			id, machine, project, agent, message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		) VALUES
			('pg-legacy-nonzero', 'test-machine', 'proj', 'claude', 0,
			 200, 600, FALSE, FALSE),
			('pg-legacy-zero', 'test-machine', 'proj', 'claude', 1,
			 0, 0, FALSE, FALSE)
	`)
	if err != nil {
		t.Fatalf("insert legacy sessions: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages (
			session_id, ordinal, role, content, content_length,
			model, token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens
		) VALUES
			('pg-legacy-zero', 0, 'assistant', 'hi', 2,
			 'claude-sonnet-4-20250514',
			 '{"input_tokens":0,"output_tokens":0}', 0, 0, FALSE, FALSE)
	`)
	if err != nil {
		t.Fatalf("insert legacy message: %v", err)
	}

	if err := EnsureSchema(ctx, pg, "agentsview"); err != nil {
		t.Fatalf("EnsureSchema backfill: %v", err)
	}

	store, err := NewStore(pgURL, "agentsview", true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	nonzero, err := store.GetSession(ctx, "pg-legacy-nonzero")
	if err != nil {
		t.Fatalf("GetSession nonzero: %v", err)
	}
	if nonzero == nil {
		t.Fatal("pg-legacy-nonzero missing")
	}
	if !nonzero.HasTotalOutputTokens {
		t.Error("pg-legacy-nonzero HasTotalOutputTokens = false, want true")
	}
	if !nonzero.HasPeakContextTokens {
		t.Error("pg-legacy-nonzero HasPeakContextTokens = false, want true")
	}

	zero, err := store.GetSession(ctx, "pg-legacy-zero")
	if err != nil {
		t.Fatalf("GetSession zero: %v", err)
	}
	if zero == nil {
		t.Fatal("pg-legacy-zero missing")
	}
	if !zero.HasTotalOutputTokens {
		t.Error("pg-legacy-zero HasTotalOutputTokens = false, want true")
	}
	if !zero.HasPeakContextTokens {
		t.Error("pg-legacy-zero HasPeakContextTokens = false, want true")
	}

	msgs, err := store.GetMessages(ctx, "pg-legacy-zero", 0, 10, true)
	if err != nil {
		t.Fatalf("GetMessages zero: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	if !msgs[0].HasContextTokens {
		t.Error("message HasContextTokens = false, want true")
	}
	if !msgs[0].HasOutputTokens {
		t.Error("message HasOutputTokens = false, want true")
	}
}
