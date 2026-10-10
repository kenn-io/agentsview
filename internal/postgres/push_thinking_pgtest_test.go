//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// TestPushThinkingText_SanitizesNullAndInvalidUTF8 verifies that
// bulkInsertMessages runs ThinkingText through sanitizePG before
// sending it to PostgreSQL. Without the sanitize call, a NUL byte
// or invalid UTF-8 in a thinking block would make the PG INSERT
// reject the entire batch and stall the push.
func TestPushThinkingText_SanitizesNullAndInvalidUTF8(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"thinking-test-machine", true,
		storage.PusherOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := time.Now().UTC().Format(time.RFC3339)
	first := "hello"
	sess := db.Session{
		ID:           "think-1",
		Project:      "proj",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &first,
		StartedAt:    &started,
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(t.Context(), sess), "upsert")

	// Message whose thinking_text contains a NUL byte and a
	// truncated multi-byte UTF-8 sequence. Before the fix the
	// insert would fail with "invalid byte sequence".
	thinking := "plan\x00step\xe2"
	require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{
		SessionID:    "think-1",
		Ordinal:      0,
		Role:         "assistant",
		Content:      "ok",
		ThinkingText: thinking,
		HasThinking:  true,
	}}), "insert local message")

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	msgs, err := store.GetMessages(ctx, "think-1", 0, 10, true)
	require.NoError(t, err, "GetMessages")
	require.Len(t, msgs, 1)
	// NUL bytes and invalid UTF-8 must be stripped; the
	// remaining text stays intact and in order.
	assert.Equal(t, "planstep", msgs[0].ThinkingText,
		"sanitize skipped?")
}

// TestPushCopiesStoredDialogue verifies push copies the dialogue SQLite
// stored rather than deriving it again from the replica's copy.
func TestPushCopiesStoredDialogue(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"dialogue-test-machine", true,
		storage.PusherOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := time.Now().UTC().Format(time.RFC3339)
	first := "hello"
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{
		ID: "dialogue-1", Project: "proj", Machine: "local", Agent: "pi",
		FirstMessage: &first, StartedAt: &started, MessageCount: 2,
	}), "upsert")
	require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{
		SessionID: "dialogue-1", Ordinal: 0, Role: "user", Content: "hello",
	}, {
		SessionID: "dialogue-1", Ordinal: 1, Role: "assistant",
		Content:      "[Thinking]\nplan\n[/Thinking]\nDone.\n[Pi tool]",
		ThinkingText: "plan", HasThinking: true, HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "pi_tool", Category: "Other", Rendering: "[Pi tool]",
		}},
	}}), "insert local messages")

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	var dialogue []*string
	rows, err := ps.pg.QueryContext(ctx,
		`SELECT dialogue_text FROM agentsview.messages
		 WHERE session_id = 'dialogue-1' ORDER BY ordinal`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var d *string
		require.NoError(t, rows.Scan(&d))
		dialogue = append(dialogue, d)
	}
	require.NoError(t, rows.Err())
	require.Len(t, dialogue, 2)
	assert.Nil(t, dialogue[0])
	require.NotNil(t, dialogue[1])
	assert.Equal(t, "Done.", *dialogue[1])
}
