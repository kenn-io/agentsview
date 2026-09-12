//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	"go.kenn.io/agentsview/internal/db"
)

func reconcilePinnedMessages(
	ctx context.Context, tx bun.IDB, sessionID string,
) error {
	pins, err := snapshotPinnedMessages(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	return restorePinnedMessages(ctx, tx, sessionID, pins)
}

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
	require.NoError(t, store.BulkStarSessions(
		[]string{"cur-star-2", "missing"},
	), "BulkStarSessions")

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
	require.NoError(t, store.UnstarSession("cur-star-1"), "UnstarSession")
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

	// The canonical schema can populate a source message ID after a pin was
	// created with PostgreSQL's former ordinal-only public identity. Pin reads
	// and deletes must continue through the authoritative session/ordinal key.
	_, err = pg.ExecContext(ctx, `
		UPDATE messages SET id = 1001
		WHERE session_id = 'cur-pin-1' AND ordinal = 1`)
	require.NoError(t, err, "populate canonical source message id")
	pins, err = store.ListPinnedMessages(ctx, "cur-pin-1", "")
	require.NoError(t, err, "ListPinnedMessages after source id population")
	require.Len(t, pins, 1)
	assert.Equal(t, int64(1001), pins[0].MessageID)

	require.NoError(t, store.UnpinMessage("cur-pin-1", 1001), "UnpinMessage")
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

// TestPushDropsEditedLegacyPinInBothStores documents the intended
// limit for UUID-less pins: a replacement that edits the pinned
// message destroys the only identity the pin can follow, so both the
// local SQLite archive and the next PostgreSQL push drop the pin —
// the stores stay consistent instead of diverging on heuristics.
func TestPushDropsEditedLegacyPinInBothStores(t *testing.T) {
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

	seed := func(sessionID string) db.Session {
		sess := db.Session{
			ID:           sessionID,
			Project:      "proj-curation",
			Machine:      "local",
			Agent:        "claude",
			MessageCount: 2,
			CreatedAt:    "2026-05-01T00:00:00Z",
		}
		require.NoError(t, local.UpsertSession(sess),
			"UpsertSession %s", sessionID)
		require.NoError(t, local.InsertMessages([]db.Message{
			{
				SessionID: sessionID, Ordinal: 0,
				Role: "user", Content: "question",
			},
			{
				SessionID: sessionID, Ordinal: 1,
				Role: "assistant", Content: "draft answer",
			},
		}), "InsertMessages %s", sessionID)
		return sess
	}
	uploadSess := seed("pg-pin-upload-edit")
	seed("pg-pin-reparse-edit")
	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "Push first")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	pinBoth := func(sessionID string) {
		msgs, err := local.GetAllMessages(ctx, sessionID)
		require.NoError(t, err, "GetAllMessages %s", sessionID)
		require.Len(t, msgs, 2, "seeded messages %s", sessionID)
		_, err = local.PinMessage(sessionID, msgs[1].ID, nil)
		require.NoError(t, err, "local PinMessage %s", sessionID)
		note := "keep " + sessionID
		_, err = store.PinMessage(sessionID, 1, &note)
		require.NoError(t, err, "pg PinMessage %s", sessionID)
	}
	pinBoth("pg-pin-upload-edit")
	pinBoth("pg-pin-reparse-edit")

	// Both replacement entry points edit the pinned message.
	edited := func(sessionID string) []db.Message {
		return []db.Message{
			{
				SessionID: sessionID, Ordinal: 0,
				Role: "user", Content: "question",
			},
			{
				SessionID: sessionID, Ordinal: 1,
				Role: "assistant", Content: "edited answer",
			},
		}
	}
	_, err = local.WriteSessionBatch([]db.SessionBatchWrite{{
		Session:         uploadSess,
		Messages:        edited("pg-pin-upload-edit"),
		DataVersion:     db.CurrentDataVersion(),
		ReplaceMessages: true,
	}})
	require.NoError(t, err, "explicit re-upload")
	require.NoError(t, local.ReplaceSessionMessages(
		"pg-pin-reparse-edit", edited("pg-pin-reparse-edit"),
	), "reparse replacement")

	for _, sessionID := range []string{
		"pg-pin-upload-edit", "pg-pin-reparse-edit",
	} {
		localPins, err := local.ListPinnedMessages(ctx, sessionID, "")
		require.NoError(t, err, "local pins %s", sessionID)
		require.Empty(t, localPins,
			"editing the pinned message drops the local pin (%s)",
			sessionID)
	}

	_, err = ps.Push(ctx, true, nil)
	require.NoError(t, err, "Push rewrite")

	for _, sessionID := range []string{
		"pg-pin-upload-edit", "pg-pin-reparse-edit",
	} {
		pins, err := store.ListPinnedMessages(ctx, sessionID, "")
		require.NoError(t, err, "pg pins %s", sessionID)
		assert.Empty(t, pins,
			"push must mirror the local drop (%s)", sessionID)
	}
}

func TestPushReconcilesPGPinsByPriorMessageIdentity(t *testing.T) {
	pgURL := testPGURL(t)

	tests := []struct {
		name        string
		oldMessages []db.Message
		newMessages []db.Message
		wantPin     bool
		wantOrdinal int
	}{
		{
			name: "duplicate UUID target removed",
			oldMessages: []db.Message{
				{
					Ordinal: 0, Role: "user", Content: "pinned",
					SourceUUID: "duplicate",
				},
				{
					Ordinal: 1, Role: "assistant", Content: "other",
					SourceUUID: "duplicate",
				},
				{
					Ordinal: 2, Role: "user", Content: "tail",
					SourceUUID: "tail",
				},
			},
			newMessages: []db.Message{
				{
					Ordinal: 0, Role: "assistant", Content: "other",
					SourceUUID: "duplicate",
				},
				{
					Ordinal: 1, Role: "user", Content: "tail",
					SourceUUID: "tail",
				},
			},
		},
		{
			name: "UUID-less prompt split by IDE envelope",
			oldMessages: []db.Message{
				{
					Ordinal: 0, Role: "user",
					Content: "<ide_opened_file>ctx</ide_opened_file>prompt",
				},
				{
					Ordinal: 1, Role: "assistant", Content: "answer",
					SourceUUID: "answer",
				},
			},
			newMessages: []db.Message{
				{
					Ordinal: 0, Role: "user",
					Content:    "<ide_opened_file>ctx</ide_opened_file>",
					SourceUUID: "entry:ide-context", IsSystem: true,
				},
				{
					Ordinal: 1, Role: "user", Content: "prompt",
					SourceUUID: "entry",
				},
				{
					Ordinal: 2, Role: "assistant", Content: "answer",
					SourceUUID: "answer",
				},
			},
		},
		{
			name: "UUID enrichment",
			oldMessages: []db.Message{
				{Ordinal: 0, Role: "user", Content: "pinned"},
				{Ordinal: 1, Role: "assistant", Content: "answer"},
			},
			newMessages: []db.Message{
				{
					Ordinal: 0, Role: "user", Content: "pinned",
					SourceUUID: "new-provider-uuid",
				},
				{Ordinal: 1, Role: "assistant", Content: "answer"},
			},
			wantPin:     true,
			wantOrdinal: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanPGSchema(t, pgURL)
			t.Cleanup(func() { cleanPGSchema(t, pgURL) })

			local := testDB(t)
			ps, err := New(
				pgURL, "agentsview", local,
				"curation-machine", true, SyncOptions{},
			)
			require.NoError(t, err, "New sync")
			defer ps.Close()

			ctx := context.Background()
			require.NoError(t, ps.EnsureSchema(ctx), "EnsureSchema")

			const sessionID = "pg-pin-prior-identity"
			sess := db.Session{
				ID: sessionID, Project: "proj-curation",
				Machine: "local", Agent: "claude",
				MessageCount: len(tt.oldMessages),
				CreatedAt:    "2026-05-01T00:00:00Z",
			}
			require.NoError(t, local.UpsertSession(sess), "UpsertSession old")
			oldMessages := append([]db.Message(nil), tt.oldMessages...)
			for i := range oldMessages {
				oldMessages[i].SessionID = sessionID
			}
			require.NoError(t, local.InsertMessages(oldMessages),
				"InsertMessages old")
			_, err = ps.Push(ctx, false, nil)
			require.NoError(t, err, "Push old")

			store, err := NewStore(pgURL, "agentsview", true)
			require.NoError(t, err, "NewStore")
			defer store.Close()
			_, err = store.PinMessage(sessionID, 0, nil)
			require.NoError(t, err, "PinMessage")

			newMessages := append([]db.Message(nil), tt.newMessages...)
			for i := range newMessages {
				newMessages[i].SessionID = sessionID
			}
			sess.MessageCount = len(newMessages)
			require.NoError(t, local.UpsertSession(sess), "UpsertSession new")
			require.NoError(t,
				local.ReplaceSessionMessages(sessionID, newMessages),
				"ReplaceSessionMessages")
			_, err = ps.Push(ctx, true, nil)
			require.NoError(t, err, "Push new")

			pins, err := store.ListPinnedMessages(ctx, sessionID, "")
			require.NoError(t, err, "ListPinnedMessages")
			if !tt.wantPin {
				assert.Empty(t, pins,
					"an ambiguous old identity must not retain a pin")
				return
			}
			require.Len(t, pins, 1, "pins = %v", pins)
			assert.Equal(t, tt.wantOrdinal, pins[0].Ordinal)
		})
	}
}

func TestRestorePinnedMessagesPreservesPinCreatedAfterSnapshot(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_snapshot_race_test"
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
			('pg-pin-snapshot-race', 'machine-a', 'proj-curation',
			 'codex', 'snapshot race',
			 '2026-05-01T00:00:00Z'::timestamptz, 2, 1);
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-snapshot-race', 0, 'user', 'first',
			 '2026-05-01T00:00:00Z'::timestamptz, 5, 'uuid-first'),
			('pg-pin-snapshot-race', 1, 'assistant', 'second',
			 '2026-05-01T00:00:01Z'::timestamptz, 6, 'uuid-second');
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid, note)
		VALUES
			('pg-pin-snapshot-race', 0, 0, 'uuid-first', 'old pin')`)
	require.NoError(t, err, "seed session and old pin")

	tx, err := bun.NewDB(pg, pgdialect.New()).BeginTx(ctx, nil)
	require.NoError(t, err, "begin tx")
	pins, err := snapshotPinnedMessages(ctx, tx, "pg-pin-snapshot-race")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("snapshotPinnedMessages: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid, note)
		VALUES
			('pg-pin-snapshot-race', 1, 1, 'uuid-second', 'new pin')`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert post-snapshot pin: %v", err)
	}
	if err := restorePinnedMessages(
		ctx, tx, "pg-pin-snapshot-race", pins,
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("restorePinnedMessages: %v", err)
	}
	require.NoError(t, tx.Commit(), "commit tx")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()
	got, err := store.ListPinnedMessages(ctx, "pg-pin-snapshot-race", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, got, 2, "post-snapshot pin must survive: %v", got)
	byOrdinal := make(map[int]db.PinnedMessage, len(got))
	for _, pin := range got {
		byOrdinal[pin.Ordinal] = pin
	}
	assert.Contains(t, byOrdinal, 0, "snapshotted pin restored")
	assert.Contains(t, byOrdinal, 1, "post-snapshot pin preserved")
}

func TestPinMessageSerializesWithSessionReplacement(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_session_lock_test"
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
			('pg-pin-session-lock', 'machine-a', 'proj-curation',
			 'codex', 'session lock',
			 '2026-05-01T00:00:00Z'::timestamptz, 1, 1);
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-session-lock', 0, 'user', 'message',
			 '2026-05-01T00:00:00Z'::timestamptz, 7, 'uuid-message')`)
	require.NoError(t, err, "seed session")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()
	store.pg.SetMaxOpenConns(1)
	_, err = store.pg.ExecContext(ctx, `SET lock_timeout = '50ms'`)
	require.NoError(t, err, "set lock timeout")

	lockTx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin lock tx")
	var lockedSessionID string
	require.NoError(t, lockTx.QueryRowContext(ctx, `
		SELECT id FROM sessions WHERE id = $1 FOR UPDATE`,
		"pg-pin-session-lock",
	).Scan(&lockedSessionID), "lock session pins")

	_, err = store.PinMessage("pg-pin-session-lock", 0, nil)
	require.Error(t, err,
		"pin mutation must wait while replacement owns the session lock")
	assert.ErrorContains(t, err, "locking pg pins for session")
	require.NoError(t, lockTx.Rollback(), "release session lock")

	_, err = store.pg.ExecContext(ctx, `SET lock_timeout = 0`)
	require.NoError(t, err, "clear lock timeout")
	pinID, err := store.PinMessage("pg-pin-session-lock", 0, nil)
	require.NoError(t, err, "PinMessage after replacement lock")
	assert.NotZero(t, pinID, "PinMessage after replacement lock")
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
			 '2026-05-01T00:03:00Z'::timestamptz),
			('pg-pin-duplicate', 2, 2, 'uuid-answer',
			 'current note',
			 '2026-05-01T00:02:00Z'::timestamptz)`)
	require.NoError(t, err, "insert pins")

	tx, err := bun.NewDB(pg, pgdialect.New()).BeginTx(ctx, nil)
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

func TestReconcilePinnedMessagesFollowsStoredUniqueSourceUUID(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_shifted_uuid_test"
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
			('pg-pin-shifted-uuid', 'machine-a', 'proj-curation',
			 'claude', 'shifted source uuid',
			 '2026-05-01T00:00:00Z'::timestamptz, 2, 1);
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-shifted-uuid', 0, 'user', '[context]',
			 '2026-05-01T00:00:00Z'::timestamptz, 9,
			 'uuid-context'),
			('pg-pin-shifted-uuid', 1, 'user', 'question',
			 '2026-05-01T00:00:01Z'::timestamptz, 8,
			 'uuid-question');
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('pg-pin-shifted-uuid', 0, 0, 'uuid-question',
			 'keep shifted pin',
			 '2026-05-01T00:01:00Z'::timestamptz)`)
	require.NoError(t, err, "seed shifted pin")

	tx, err := bun.NewDB(pg, pgdialect.New()).BeginTx(ctx, nil)
	require.NoError(t, err, "begin tx")
	if err := reconcilePinnedMessages(
		ctx, tx, "pg-pin-shifted-uuid",
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("reconcilePinnedMessages: %v", err)
	}
	require.NoError(t, tx.Commit(), "commit tx")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	pins, err := store.ListPinnedMessages(ctx, "pg-pin-shifted-uuid", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1, "pins = %v", pins)
	assert.Equal(t, int64(1), pins[0].MessageID)
	assert.Equal(t, 1, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, "keep shifted pin", *pins[0].Note)
}

func TestRestorePinnedMessagesUsesResolvedAnchorOrdinalForNewDuplicateUUID(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_shifted_duplicate_test"
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
			('pg-pin-shifted-duplicate', 'machine-a', 'proj-curation',
			 'claude', 'shifted source becomes duplicate',
			 '2026-05-01T00:00:00Z'::timestamptz, 2, 1);
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-shifted-duplicate', 0, 'user', '[context]',
			 '2026-05-01T00:00:00Z'::timestamptz, 9,
			 'uuid-context'),
			('pg-pin-shifted-duplicate', 1, 'assistant', 'answer',
			 '2026-05-01T00:00:01Z'::timestamptz, 6,
			 'uuid-answer');
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('pg-pin-shifted-duplicate', 0, 0, 'uuid-answer',
			 'keep shifted duplicate pin',
			 '2026-05-01T00:01:00Z'::timestamptz)`)
	require.NoError(t, err, "seed shifted pin")

	tx, err := bun.NewDB(pg, pgdialect.New()).BeginTx(ctx, nil)
	require.NoError(t, err, "begin tx")
	pins, err := snapshotPinnedMessages(
		ctx, tx, "pg-pin-shifted-duplicate",
	)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("snapshotPinnedMessages: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE session_id = 'pg-pin-shifted-duplicate';
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-shifted-duplicate', 0, 'assistant', 'retry',
			 '2026-05-01T00:00:00Z'::timestamptz, 5,
			 'uuid-answer'),
			('pg-pin-shifted-duplicate', 1, 'assistant', 'answer',
			 '2026-05-01T00:00:01Z'::timestamptz, 6,
			 'uuid-answer')`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("replace messages: %v", err)
	}
	if err := restorePinnedMessages(
		ctx, tx, "pg-pin-shifted-duplicate", pins,
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("restorePinnedMessages: %v", err)
	}
	require.NoError(t, tx.Commit(), "commit tx")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.ListPinnedMessages(
		ctx, "pg-pin-shifted-duplicate", "",
	)
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, got, 1, "pins = %v", got)
	assert.Equal(t, 1, got[0].Ordinal)
	require.NotNil(t, got[0].Note)
	assert.Equal(t, "keep shifted duplicate pin", *got[0].Note)
}

// restoreIdenticalDuplicatePins seeds a session holding two identical
// (source_uuid, role, content) messages with a pin on the second one,
// replaces the messages with replacementValues through the
// snapshot/restore cycle a push performs, and returns the surviving
// pins.
func restoreIdenticalDuplicatePins(
	t *testing.T, schema, sessionID, replacementValues string,
) []db.PinnedMessage {
	t.Helper()
	pgURL := testPGURL(t)

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
			('`+sessionID+`', 'machine-a', 'proj-curation',
			 'claude', 'identical duplicates',
			 '2026-05-01T00:00:00Z'::timestamptz, 2, 2);
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('`+sessionID+`', 0, 'user', 'same',
			 '2026-05-01T00:00:00Z'::timestamptz, 4, 'dup'),
			('`+sessionID+`', 1, 'user', 'same',
			 '2026-05-01T00:00:01Z'::timestamptz, 4, 'dup');
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('`+sessionID+`', 1, 1, 'dup',
			 'pinned duplicate',
			 '2026-05-01T00:01:00Z'::timestamptz)`)
	require.NoError(t, err, "seed identical duplicate pin")

	tx, err := bun.NewDB(pg, pgdialect.New()).BeginTx(ctx, nil)
	require.NoError(t, err, "begin tx")
	pins, err := snapshotPinnedMessages(ctx, tx, sessionID)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("snapshotPinnedMessages: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		DELETE FROM messages WHERE session_id = '`+sessionID+`';
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES `+replacementValues)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("replace messages: %v", err)
	}
	if err := restorePinnedMessages(ctx, tx, sessionID, pins); err != nil {
		_ = tx.Rollback()
		t.Fatalf("restorePinnedMessages: %v", err)
	}
	require.NoError(t, tx.Commit(), "commit tx")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.ListPinnedMessages(ctx, sessionID, "")
	require.NoError(t, err, "ListPinnedMessages")
	return got
}

func TestRestorePinnedMessagesKeepsPinOnUnchangedIdenticalDuplicates(
	t *testing.T,
) {
	got := restoreIdenticalDuplicatePins(t,
		"agentsview_pin_identical_dup_keep_test",
		"pg-pin-identical-dup-keep", `
		('pg-pin-identical-dup-keep', 0, 'user', 'same',
		 '2026-05-01T00:00:00Z'::timestamptz, 4, 'dup'),
		('pg-pin-identical-dup-keep', 1, 'user', 'same',
		 '2026-05-01T00:00:01Z'::timestamptz, 4, 'dup')`)
	require.Len(t, got, 1,
		"unchanged identical duplicates must keep the pin; pins = %v", got)
	assert.Equal(t, 1, got[0].Ordinal, "pin stays at its saved ordinal")
	require.NotNil(t, got[0].Note)
	assert.Equal(t, "pinned duplicate", *got[0].Note)
}

func TestRestorePinnedMessagesDropsPinOnChangedDuplicateMultiplicity(
	t *testing.T,
) {
	got := restoreIdenticalDuplicatePins(t,
		"agentsview_pin_identical_dup_change_test",
		"pg-pin-identical-dup-change", `
		('pg-pin-identical-dup-change', 0, 'user', 'same',
		 '2026-05-01T00:00:00Z'::timestamptz, 4, 'dup'),
		('pg-pin-identical-dup-change', 1, 'user', 'same',
		 '2026-05-01T00:00:01Z'::timestamptz, 4, 'dup'),
		('pg-pin-identical-dup-change', 2, 'user', 'same',
		 '2026-05-01T00:00:02Z'::timestamptz, 4, 'dup')`)
	assert.Empty(t, got,
		"changed duplicate multiplicity must drop the ambiguous pin")
}

func TestRestorePinnedMessagesFollowsShiftedIdenticalDuplicates(
	t *testing.T,
) {
	// A context row inserted before the duplicates shifts both while
	// their multiplicity stays equal: the pin must follow its
	// occurrence rank instead of staying on the saved ordinal where
	// the first duplicate now sits.
	got := restoreIdenticalDuplicatePins(t,
		"agentsview_pin_identical_dup_shift_test",
		"pg-pin-identical-dup-shift", `
		('pg-pin-identical-dup-shift', 0, 'user', 'context',
		 '2026-05-01T00:00:00Z'::timestamptz, 7, 'ctx'),
		('pg-pin-identical-dup-shift', 1, 'user', 'same',
		 '2026-05-01T00:00:01Z'::timestamptz, 4, 'dup'),
		('pg-pin-identical-dup-shift', 2, 'user', 'same',
		 '2026-05-01T00:00:02Z'::timestamptz, 4, 'dup')`)
	require.Len(t, got, 1,
		"shifted duplicates must keep the pin; pins = %v", got)
	assert.Equal(t, 2, got[0].Ordinal,
		"pin follows the second occurrence, not the saved ordinal")
}

func TestRestorePinnedMessagesFollowsShiftedEqualLegacyMessages(
	t *testing.T,
) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_legacy_shift_test"
	const sessionID = "pg-pin-legacy-shift"
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
			('`+sessionID+`', 'machine-a', 'proj-curation',
			 'claude', 'equal legacy messages',
			 '2026-05-01T00:00:00Z'::timestamptz, 3, 3);
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('`+sessionID+`', 0, 'user', 'intro',
			 '2026-05-01T00:00:00Z'::timestamptz, 5, ''),
			('`+sessionID+`', 1, 'user', 'x',
			 '2026-05-01T00:00:01Z'::timestamptz, 1, ''),
			('`+sessionID+`', 2, 'user', 'x',
			 '2026-05-01T00:00:02Z'::timestamptz, 1, '');
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('`+sessionID+`', 2, 2, '',
			 'legacy pin on second x',
			 '2026-05-01T00:01:00Z'::timestamptz)`)
	require.NoError(t, err, "seed legacy pin")

	tx, err := bun.NewDB(pg, pgdialect.New()).BeginTx(ctx, nil)
	require.NoError(t, err, "begin tx")
	pins, err := snapshotPinnedMessages(ctx, tx, sessionID)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("snapshotPinnedMessages: %v", err)
	}
	// A hidden row inserted at the front shifts two equal visible
	// messages; the pin on the second "x" must follow its occurrence
	// rank to the shifted ordinal.
	_, err = tx.ExecContext(ctx, `
		DELETE FROM messages WHERE session_id = '`+sessionID+`';
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid, is_system)
		VALUES
			('`+sessionID+`', 0, 'user', 'context',
			 '2026-05-01T00:00:00Z'::timestamptz, 7, '', TRUE),
			('`+sessionID+`', 1, 'user', 'intro',
			 '2026-05-01T00:00:01Z'::timestamptz, 5, '', FALSE),
			('`+sessionID+`', 2, 'user', 'x',
			 '2026-05-01T00:00:02Z'::timestamptz, 1, '', FALSE),
			('`+sessionID+`', 3, 'user', 'x',
			 '2026-05-01T00:00:03Z'::timestamptz, 1, '', FALSE)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("replace messages: %v", err)
	}
	if err := restorePinnedMessages(ctx, tx, sessionID, pins); err != nil {
		_ = tx.Rollback()
		t.Fatalf("restorePinnedMessages: %v", err)
	}
	require.NoError(t, tx.Commit(), "commit tx")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.ListPinnedMessages(ctx, sessionID, "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, got, 1,
		"shifted equal messages must keep the pin; pins = %v", got)
	assert.Equal(t, 3, got[0].Ordinal,
		"pin follows the second occurrence, not the saved ordinal")
	require.NotNil(t, got[0].Note)
	assert.Equal(t, "legacy pin on second x", *got[0].Note)
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

	tx, err := bun.NewDB(pg, pgdialect.New()).BeginTx(ctx, nil)
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

	tx, err := bun.NewDB(pg, pgdialect.New()).BeginTx(ctx, nil)
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
