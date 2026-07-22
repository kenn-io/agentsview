//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreStarsAndPins(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_curation_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	_, err = pg.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('cur-star-1', 'machine-a', 'proj-curation',
			 'codex', 'star one',
			 '2026-05-01T00:00:00Z'::timestamptz, 0, 0),
			('cur-star-2', 'machine-a', 'proj-curation',
			 'codex', 'star two',
			 '2026-05-01T00:01:00Z'::timestamptz, 0, 0),
			('cur-pin-1', 'machine-a', 'proj-curation',
			 'claude', 'pin source',
			 '2026-05-01T00:02:00Z'::timestamptz, 2, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('cur-pin-1', 0, 'user', 'question',
			 '2026-05-01T00:02:00Z'::timestamptz, 8,
			 'uuid-question'),
			('cur-pin-1', 1, 'assistant', 'answer',
			 '2026-05-01T00:02:01Z'::timestamptz, 6,
			 'uuid-answer')`)
	require.NoError(t, err, "insert messages")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ok, err := store.StarSession("cur-star-1")
	require.NoError(t, err, "StarSession existing")
	require.True(t, ok, "StarSession existing")
	ok, err = store.StarSession("missing")
	require.NoError(t, err, "StarSession missing")
	assert.False(t, ok, "StarSession missing")
	bulkStarred, err := store.BulkStarSessions([]string{"cur-star-2", "missing"})
	require.NoError(t, err, "BulkStarSessions")
	assert.Equal(t, []string{"cur-star-2"}, bulkStarred, "starred ids returned")
	bulkStarred, err = store.BulkStarSessions([]string{"cur-star-1", "cur-star-2"})
	require.NoError(t, err, "BulkStarSessions already starred")
	assert.Empty(t, bulkStarred, "already-starred ids should not be returned")

	ids, err := store.ListStarredSessionIDs(ctx)
	require.NoError(t, err, "ListStarredSessionIDs")
	wantStars := map[string]bool{
		"cur-star-1": true,
		"cur-star-2": true,
	}
	require.Len(t, ids, len(wantStars), "starred ids = %v", ids)
	for _, id := range ids {
		assert.True(t, wantStars[id], "unexpected starred id %q in %v", id, ids)
	}
	removed, err := store.UnstarSession("cur-star-1")
	require.NoError(t, err, "UnstarSession")
	require.True(t, removed, "UnstarSession")
	removed, err = store.UnstarSession("cur-star-1")
	require.NoError(t, err, "UnstarSession no-op")
	require.False(t, removed, "UnstarSession no-op")
	ids, err = store.ListStarredSessionIDs(ctx)
	require.NoError(t, err, "ListStarredSessionIDs after unstar")
	require.Len(t, ids, 1)
	assert.Equal(t, "cur-star-2", ids[0])

	note := "keep this"
	pinID, err := store.PinMessage("cur-pin-1", 1, &note)
	require.NoError(t, err, "PinMessage")
	require.NotZero(t, pinID, "PinMessage returned 0, want row id")
	updatedNote := "updated"
	pinID2, err := store.PinMessage("cur-pin-1", 1, &updatedNote)
	require.NoError(t, err, "PinMessage update")
	assert.Equal(t, pinID, pinID2)
	missingPin, err := store.PinMessage("cur-pin-1", 99, nil)
	require.NoError(t, err, "PinMessage missing message")
	assert.Zero(t, missingPin)

	pins, err := store.ListPinnedMessages(ctx, "cur-pin-1", "")
	require.NoError(t, err, "ListPinnedMessages session")
	require.Len(t, pins, 1)
	assert.Equal(t, int64(1), pins[0].MessageID)
	assert.Equal(t, 1, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, updatedNote, *pins[0].Note)

	allPins, err := store.ListPinnedMessages(ctx, "", "proj-curation")
	require.NoError(t, err, "ListPinnedMessages all")
	require.Len(t, allPins, 1)
	require.NotNil(t, allPins[0].Content)
	assert.Equal(t, "answer", *allPins[0].Content)
	require.NotNil(t, allPins[0].Role)
	assert.Equal(t, "assistant", *allPins[0].Role)
	require.NotNil(t, allPins[0].SessionProject)
	assert.Equal(t, "proj-curation", *allPins[0].SessionProject)

	require.NoError(t, store.UnpinMessage("cur-pin-1", 1), "UnpinMessage")
	pins, err = store.ListPinnedMessages(ctx, "cur-pin-1", "")
	require.NoError(t, err, "ListPinnedMessages after unpin")
	assert.Empty(t, pins)
}

func TestPushPreservesMultiplePGPinsBySourceUUID(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"curation-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "New sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "EnsureSchema")

	sess := db.Session{
		ID:           "pg-pin-rewrite",
		Project:      "proj-curation",
		Machine:      "local",
		Agent:        "codex",
		MessageCount: 3,
		CreatedAt:    "2026-05-01T00:00:00Z",
	}
	require.NoError(t, local.UpsertSession(sess), "UpsertSession first")
	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID:  "pg-pin-rewrite",
			Ordinal:    0,
			Role:       "user",
			Content:    "question",
			SourceUUID: "uuid-question",
		},
		{
			SessionID:  "pg-pin-rewrite",
			Ordinal:    1,
			Role:       "assistant",
			Content:    "answer one",
			SourceUUID: "uuid-answer-one",
		},
		{
			SessionID:  "pg-pin-rewrite",
			Ordinal:    2,
			Role:       "assistant",
			Content:    "answer two",
			SourceUUID: "uuid-answer-two",
		},
	}), "InsertMessages first")
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "Push first")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	noteOne := "important one"
	_, err = store.PinMessage("pg-pin-rewrite", 1, &noteOne)
	require.NoError(t, err, "PinMessage one")
	noteTwo := "important two"
	_, err = store.PinMessage("pg-pin-rewrite", 2, &noteTwo)
	require.NoError(t, err, "PinMessage two")

	sess.MessageCount = 4
	require.NoError(t, local.UpsertSession(sess), "UpsertSession second")
	require.NoError(t, local.ReplaceSessionMessages(
		"pg-pin-rewrite",
		[]db.Message{
			{
				SessionID:  "pg-pin-rewrite",
				Ordinal:    0,
				Role:       "user",
				Content:    "question",
				SourceUUID: "uuid-question",
			},
			{
				SessionID:         "pg-pin-rewrite",
				Ordinal:           1,
				Role:              "user",
				Content:           "[compact]",
				SourceUUID:        "uuid-boundary",
				IsCompactBoundary: true,
			},
			{
				SessionID:  "pg-pin-rewrite",
				Ordinal:    2,
				Role:       "assistant",
				Content:    "answer one",
				SourceUUID: "uuid-answer-one",
			},
			{
				SessionID:  "pg-pin-rewrite",
				Ordinal:    3,
				Role:       "assistant",
				Content:    "answer two",
				SourceUUID: "uuid-answer-two",
			},
		},
	), "ReplaceSessionMessages")

	_, err = ps.Push(ctx, true, nil)
	require.NoError(t, err, "Push rewrite")

	pins, err := store.ListPinnedMessages(ctx, "pg-pin-rewrite", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 2)

	byNote := map[string]db.PinnedMessage{}
	for _, pin := range pins {
		require.NotNil(t, pin.Note, "pin note should be populated")
		byNote[*pin.Note] = pin
	}
	pin, ok := byNote[noteOne]
	require.True(t, ok, "pin for %q missing: %v", noteOne, pins)
	assert.Equal(t, int64(2), pin.MessageID)
	assert.Equal(t, 2, pin.Ordinal)
	pin, ok = byNote[noteTwo]
	require.True(t, ok, "pin for %q missing: %v", noteTwo, pins)
	assert.Equal(t, int64(3), pin.MessageID)
	assert.Equal(t, 3, pin.Ordinal)
}

func TestPushBackfillsLegacyPinSourceUUIDBeforeOrdinalShift(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"curation-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "New sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "EnsureSchema")

	sess := db.Session{
		ID:               "pg-legacy-pin-shift",
		Project:          "proj-curation",
		Machine:          "local",
		Agent:            "codex",
		MessageCount:     2,
		UserMessageCount: 1,
		CreatedAt:        "2026-05-01T00:00:00Z",
	}
	require.NoError(t, local.UpsertSession(sess), "UpsertSession first")
	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID:  "pg-legacy-pin-shift",
			Ordinal:    0,
			Role:       "user",
			Content:    "question",
			SourceUUID: "uuid-question",
		},
		{
			SessionID:  "pg-legacy-pin-shift",
			Ordinal:    1,
			Role:       "assistant",
			Content:    "answer",
			SourceUUID: "uuid-answer",
		},
	}), "InsertMessages first")
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "Push first")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	note := "legacy pin"
	_, err = store.PinMessage("pg-legacy-pin-shift", 1, &note)
	require.NoError(t, err, "PinMessage")
	_, err = ps.pg.ExecContext(ctx, `
		UPDATE pinned_messages
		SET source_uuid = ''
		WHERE session_id = $1 AND message_id = $2`,
		"pg-legacy-pin-shift", 1,
	)
	require.NoError(t, err, "clear source_uuid to simulate legacy pin")

	sess.MessageCount = 3
	require.NoError(t, local.UpsertSession(sess), "UpsertSession second")
	require.NoError(t, local.ReplaceSessionMessages(
		"pg-legacy-pin-shift",
		[]db.Message{
			{
				SessionID:  "pg-legacy-pin-shift",
				Ordinal:    0,
				Role:       "user",
				Content:    "question",
				SourceUUID: "uuid-question",
			},
			{
				SessionID:         "pg-legacy-pin-shift",
				Ordinal:           1,
				Role:              "user",
				Content:           "[compact]",
				SourceUUID:        "uuid-boundary",
				IsCompactBoundary: true,
			},
			{
				SessionID:  "pg-legacy-pin-shift",
				Ordinal:    2,
				Role:       "assistant",
				Content:    "answer",
				SourceUUID: "uuid-answer",
			},
		},
	), "ReplaceSessionMessages")

	_, err = ps.Push(ctx, true, nil)
	require.NoError(t, err, "Push rewrite")

	pins, err := store.ListPinnedMessages(ctx, "pg-legacy-pin-shift", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1)
	assert.Equal(t, int64(2), pins[0].MessageID)
	assert.Equal(t, 2, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, note, *pins[0].Note)
}

func TestReconcilePinnedMessagesPrefersCurrentTargetPin(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_duplicate_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	_, err = pg.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('pg-pin-duplicate', 'machine-a', 'proj-curation',
			 'codex', 'duplicate source repair',
			 '2026-05-01T00:00:00Z'::timestamptz, 3, 1)`)
	require.NoError(t, err, "insert session")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-duplicate', 0, 'user', 'question',
			 '2026-05-01T00:00:00Z'::timestamptz, 8,
			 'uuid-question'),
			('pg-pin-duplicate', 1, 'user', '[compact]',
			 '2026-05-01T00:00:01Z'::timestamptz, 9,
			 'uuid-boundary'),
			('pg-pin-duplicate', 2, 'assistant', 'answer',
			 '2026-05-01T00:00:02Z'::timestamptz, 6,
			 'uuid-answer')`)
	require.NoError(t, err, "insert messages")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('pg-pin-duplicate', 1, 1, 'uuid-answer',
			 'stale note',
			 '2026-05-01T00:01:00Z'::timestamptz),
			('pg-pin-duplicate', 2, 2, 'uuid-answer',
			 'current note',
			 '2026-05-01T00:02:00Z'::timestamptz)`)
	require.NoError(t, err, "insert pins")

	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin tx")
	if err := reconcilePinnedMessages(
		ctx, tx, "pg-pin-duplicate",
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("reconcilePinnedMessages: %v", err)
	}
	require.NoError(t, tx.Commit(), "commit tx")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	pins, err := store.ListPinnedMessages(ctx, "pg-pin-duplicate", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1, "pins = %v", pins)
	assert.Equal(t, int64(2), pins[0].MessageID)
	assert.Equal(t, 2, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, "current note", *pins[0].Note)
}

// TestReconcilePinnedMessagesPrunesPinWhenSourceUUIDGone covers the
// case where a source-backed pin's source_uuid no longer exists in
// the messages table, but a different message now occupies the
// pin's original ordinal. The pin must be deleted: otherwise it
// would silently re-anchor on an unrelated message.
func TestReconcilePinnedMessagesPrunesPinWhenSourceUUIDGone(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_source_gone_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	_, err = pg.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('pg-pin-source-gone', 'machine-a', 'proj-curation',
			 'codex', 'source uuid gone',
			 '2026-05-01T00:00:00Z'::timestamptz, 2, 1)`)
	require.NoError(t, err, "insert session")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-source-gone', 0, 'user', 'question',
			 '2026-05-01T00:00:00Z'::timestamptz, 8,
			 'uuid-question'),
			('pg-pin-source-gone', 1, 'assistant', 'new answer',
			 '2026-05-01T00:00:01Z'::timestamptz, 10,
			 'uuid-new-answer')`)
	require.NoError(t, err, "insert messages")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('pg-pin-source-gone', 1, 1, 'uuid-gone-forever',
			 'stale pin',
			 '2026-05-01T00:01:00Z'::timestamptz)`)
	require.NoError(t, err, "insert pin")

	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin tx")
	if err := reconcilePinnedMessages(
		ctx, tx, "pg-pin-source-gone",
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("reconcilePinnedMessages: %v", err)
	}
	require.NoError(t, tx.Commit(), "commit tx")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	pins, err := store.ListPinnedMessages(ctx, "pg-pin-source-gone", "")
	require.NoError(t, err, "ListPinnedMessages")
	assert.Empty(t, pins,
		"stale source_uuid should be pruned: %v", pins)
}

// TestReconcilePinnedMessagesKeepsPinOnLaterDuplicateSourceUUID
// covers the case where multiple messages in the same session share
// the same source_uuid (the schema permits it) and the pin sits on
// the later duplicate. Reconciliation must keep the pin where it is
// rather than relocating it to the lowest-ordinal duplicate.
func TestReconcilePinnedMessagesKeepsPinOnLaterDuplicateSourceUUID(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_dup_uuid_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	_, err = pg.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('pg-pin-dup-uuid', 'machine-a', 'proj-curation',
			 'claude', 'duplicate source uuid',
			 '2026-05-01T00:00:00Z'::timestamptz, 3, 1)`)
	require.NoError(t, err, "insert session")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-dup-uuid', 0, 'user', 'question',
			 '2026-05-01T00:00:00Z'::timestamptz, 8,
			 'uuid-question'),
			('pg-pin-dup-uuid', 1, 'assistant', 'first answer',
			 '2026-05-01T00:00:01Z'::timestamptz, 12,
			 'uuid-shared'),
			('pg-pin-dup-uuid', 2, 'assistant', 'second answer',
			 '2026-05-01T00:00:02Z'::timestamptz, 13,
			 'uuid-shared')`)
	require.NoError(t, err, "insert messages")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('pg-pin-dup-uuid', 2, 2, 'uuid-shared',
			 'pin on later duplicate',
			 '2026-05-01T00:01:00Z'::timestamptz)`)
	require.NoError(t, err, "insert pin")

	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin tx")
	if err := reconcilePinnedMessages(
		ctx, tx, "pg-pin-dup-uuid",
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("reconcilePinnedMessages: %v", err)
	}
	require.NoError(t, tx.Commit(), "commit tx")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	pins, err := store.ListPinnedMessages(ctx, "pg-pin-dup-uuid", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1, "pins = %v", pins)
	assert.Equal(t, int64(2), pins[0].MessageID,
		"must not relocate to lower-ordinal duplicate")
	assert.Equal(t, 2, pins[0].Ordinal,
		"must not relocate to lower-ordinal duplicate")
}

// TestPinMessageRepinRefreshesSourceUUID covers re-pinning the same
// (session_id, message_id). The stored source_uuid must reflect the
// message currently at message_id; otherwise the next reconciliation
// would follow the stale uuid away from where the user just pinned.
func TestPinMessageRepinRefreshesSourceUUID(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_repin_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	_, err = pg.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('pg-pin-repin', 'machine-a', 'proj-curation',
			 'claude', 'repin refreshes uuid',
			 '2026-05-01T00:00:00Z'::timestamptz, 2, 1)`)
	require.NoError(t, err, "insert session")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-repin', 0, 'user', 'question',
			 '2026-05-01T00:00:00Z'::timestamptz, 8,
			 'uuid-question'),
			('pg-pin-repin', 1, 'assistant', 'original',
			 '2026-05-01T00:00:01Z'::timestamptz, 8,
			 'uuid-original')`)
	require.NoError(t, err, "insert messages")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	originalNote := "first"
	_, err = store.PinMessage("pg-pin-repin", 1, &originalNote)
	require.NoError(t, err, "PinMessage initial")
	var initialSourceUUID, initialCreatedAt string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT source_uuid, created_at::text
		FROM pinned_messages
		WHERE session_id = $1 AND message_id = $2`,
		"pg-pin-repin", 1,
	).Scan(&initialSourceUUID, &initialCreatedAt), "query initial pin")
	assert.Equal(t, "uuid-original", initialSourceUUID)

	// Simulate a session rewrite that replaces the message at
	// ordinal 1 with a different message (different source_uuid)
	// while reusing the ordinal.
	_, err = pg.ExecContext(ctx, `
		UPDATE messages
		SET source_uuid = 'uuid-replacement',
			content = 'replaced'
		WHERE session_id = $1 AND ordinal = $2`,
		"pg-pin-repin", 1,
	)
	require.NoError(t, err, "update message source_uuid")

	updatedNote := "second"
	_, err = store.PinMessage("pg-pin-repin", 1, &updatedNote)
	require.NoError(t, err, "PinMessage repin")

	var gotSourceUUID, gotCreatedAt string
	var gotNote *string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT source_uuid, note, created_at::text
		FROM pinned_messages
		WHERE session_id = $1 AND message_id = $2`,
		"pg-pin-repin", 1,
	).Scan(&gotSourceUUID, &gotNote, &gotCreatedAt), "query repinned pin")
	assert.Equal(t, "uuid-replacement", gotSourceUUID,
		"stale source_uuid would steer the next reconciliation away from the current message")
	require.NotNil(t, gotNote)
	assert.Equal(t, updatedNote, *gotNote)
	assert.Equal(t, initialCreatedAt, gotCreatedAt, "created_at must be preserved")
}
