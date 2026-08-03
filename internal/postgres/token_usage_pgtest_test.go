//go:build pgtest

package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreSessionAndMessageTokenUsage(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
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
	require.NoError(t, err, "insert session")
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
	require.NoError(t, err, "insert message")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetSession(ctx, "token-store-001")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, sess)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 500, sess.TotalOutputTokens)

	msgs, err := store.GetMessages(ctx, "token-store-001", 0, 10, true)
	require.NoError(t, err, "GetMessages")
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].HasOutputTokens)
	assert.True(t, msgs[0].HasContextTokens)
	assert.Equal(t, 200, msgs[0].OutputTokens)
}

func TestPushTokenUsageToPostgres(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "test-machine", true, SyncOptions{})
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "EnsureSchema")

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
	require.NoError(t, local.UpsertSession(sess), "UpsertSession")
	require.NoError(t, local.InsertMessages([]db.Message{{
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
	}}), "InsertMessages")

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	gotSess, err := store.GetSession(ctx, "token-push-001")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, gotSess, "expected pushed session")
	assert.True(t, gotSess.HasTotalOutputTokens)
	assert.True(t, gotSess.HasPeakContextTokens)
	assert.Equal(t, 500, gotSess.TotalOutputTokens)

	gotMsgs, err := store.GetMessages(ctx, "token-push-001", 0, 10, true)
	require.NoError(t, err, "GetMessages")
	require.Len(t, gotMsgs, 1)
	assert.True(t, gotMsgs[0].HasContextTokens)
	assert.True(t, gotMsgs[0].HasOutputTokens)
	assert.Equal(t, 200, gotMsgs[0].OutputTokens)
}

func TestEnsureSchemaBackfillsTokenCoverageFlags(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	pg, err := Open(pgURL, "agentsview", false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, "agentsview"), "EnsureSchema initial")

	_, err = pg.Exec(`
		INSERT INTO source_archives (source_archive_id, source_archive_salt)
		VALUES ('coverage-archive', 'coverage-salt');
		INSERT INTO sessions (
			id, machine, project, agent, message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens,
			source_archive_id, source_database_generation
		) VALUES
			('pg-legacy-nonzero', 'test-machine', 'proj', 'claude', 0,
			 200, 600, FALSE, FALSE, 'coverage-archive', 'coverage-generation'),
			('pg-legacy-zero', 'test-machine', 'proj', 'claude', 1,
			 0, 0, FALSE, FALSE, 'coverage-archive', 'coverage-generation')
	`)
	require.NoError(t, err, "insert legacy sessions")
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
	require.NoError(t, err, "insert legacy message")

	require.NoError(t, EnsureSchema(ctx, pg, "agentsview"), "EnsureSchema backfill")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	nonzero, err := store.GetSession(ctx, "pg-legacy-nonzero")
	require.NoError(t, err, "GetSession nonzero")
	require.NotNil(t, nonzero, "pg-legacy-nonzero missing")
	assert.True(t, nonzero.HasTotalOutputTokens,
		"pg-legacy-nonzero HasTotalOutputTokens")
	assert.True(t, nonzero.HasPeakContextTokens,
		"pg-legacy-nonzero HasPeakContextTokens")

	zero, err := store.GetSession(ctx, "pg-legacy-zero")
	require.NoError(t, err, "GetSession zero")
	require.NotNil(t, zero, "pg-legacy-zero missing")
	assert.True(t, zero.HasTotalOutputTokens,
		"pg-legacy-zero HasTotalOutputTokens")
	assert.True(t, zero.HasPeakContextTokens,
		"pg-legacy-zero HasPeakContextTokens")

	msgs, err := store.GetMessages(ctx, "pg-legacy-zero", 0, 10, true)
	require.NoError(t, err, "GetMessages zero")
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].HasContextTokens, "message HasContextTokens")
	assert.True(t, msgs[0].HasOutputTokens, "message HasOutputTokens")
}
