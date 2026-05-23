//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreStarsAndPins(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_curation_test"
	pg, err := Open(pgURL, schema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	if _, err := pg.ExecContext(
		ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
	); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

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
	if err != nil {
		t.Fatalf("insert sessions: %v", err)
	}
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
	if err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	store, err := NewStore(pgURL, schema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ok, err := store.StarSession("cur-star-1")
	if err != nil || !ok {
		t.Fatalf("StarSession existing: ok=%v err=%v", ok, err)
	}
	ok, err = store.StarSession("missing")
	if err != nil {
		t.Fatalf("StarSession missing: %v", err)
	}
	if ok {
		t.Fatal("StarSession missing = true, want false")
	}
	if err := store.BulkStarSessions(
		[]string{"cur-star-2", "missing"},
	); err != nil {
		t.Fatalf("BulkStarSessions: %v", err)
	}

	ids, err := store.ListStarredSessionIDs(ctx)
	if err != nil {
		t.Fatalf("ListStarredSessionIDs: %v", err)
	}
	wantStars := map[string]bool{
		"cur-star-1": true,
		"cur-star-2": true,
	}
	if len(ids) != len(wantStars) {
		t.Fatalf("starred ids = %v, want both stars", ids)
	}
	for _, id := range ids {
		if !wantStars[id] {
			t.Fatalf("unexpected starred id %q in %v", id, ids)
		}
	}
	if err := store.UnstarSession("cur-star-1"); err != nil {
		t.Fatalf("UnstarSession: %v", err)
	}
	ids, err = store.ListStarredSessionIDs(ctx)
	if err != nil {
		t.Fatalf("ListStarredSessionIDs after unstar: %v", err)
	}
	if len(ids) != 1 || ids[0] != "cur-star-2" {
		t.Fatalf("starred ids after unstar = %v, want cur-star-2", ids)
	}

	note := "keep this"
	pinID, err := store.PinMessage("cur-pin-1", 1, &note)
	if err != nil {
		t.Fatalf("PinMessage: %v", err)
	}
	if pinID == 0 {
		t.Fatal("PinMessage returned 0, want row id")
	}
	updatedNote := "updated"
	pinID2, err := store.PinMessage("cur-pin-1", 1, &updatedNote)
	if err != nil {
		t.Fatalf("PinMessage update: %v", err)
	}
	if pinID2 != pinID {
		t.Fatalf("updated pin id = %d, want %d", pinID2, pinID)
	}
	missingPin, err := store.PinMessage("cur-pin-1", 99, nil)
	if err != nil {
		t.Fatalf("PinMessage missing message: %v", err)
	}
	if missingPin != 0 {
		t.Fatalf("missing pin id = %d, want 0", missingPin)
	}

	pins, err := store.ListPinnedMessages(ctx, "cur-pin-1", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages session: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("session pins = %d, want 1", len(pins))
	}
	if pins[0].MessageID != 1 || pins[0].Ordinal != 1 {
		t.Fatalf(
			"pin message/ordinal = %d/%d, want 1/1",
			pins[0].MessageID, pins[0].Ordinal,
		)
	}
	if pins[0].Note == nil || *pins[0].Note != updatedNote {
		t.Fatalf("pin note = %v, want %q", pins[0].Note, updatedNote)
	}

	allPins, err := store.ListPinnedMessages(ctx, "", "proj-curation")
	if err != nil {
		t.Fatalf("ListPinnedMessages all: %v", err)
	}
	if len(allPins) != 1 {
		t.Fatalf("all pins = %d, want 1", len(allPins))
	}
	if allPins[0].Content == nil || *allPins[0].Content != "answer" {
		t.Fatalf("pin content = %v, want answer", allPins[0].Content)
	}
	if allPins[0].Role == nil || *allPins[0].Role != "assistant" {
		t.Fatalf("pin role = %v, want assistant", allPins[0].Role)
	}
	if allPins[0].SessionProject == nil ||
		*allPins[0].SessionProject != "proj-curation" {
		t.Fatalf(
			"pin project = %v, want proj-curation",
			allPins[0].SessionProject,
		)
	}

	if err := store.UnpinMessage("cur-pin-1", 1); err != nil {
		t.Fatalf("UnpinMessage: %v", err)
	}
	pins, err = store.ListPinnedMessages(ctx, "cur-pin-1", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages after unpin: %v", err)
	}
	if len(pins) != 0 {
		t.Fatalf("pins after unpin = %d, want 0", len(pins))
	}
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
	if err != nil {
		t.Fatalf("New sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	sess := db.Session{
		ID:           "pg-pin-rewrite",
		Project:      "proj-curation",
		Machine:      "local",
		Agent:        "codex",
		MessageCount: 3,
		CreatedAt:    "2026-05-01T00:00:00Z",
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession first: %v", err)
	}
	if err := local.InsertMessages([]db.Message{
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
	}); err != nil {
		t.Fatalf("InsertMessages first: %v", err)
	}
	if _, err := ps.Push(ctx, false, nil); err != nil {
		t.Fatalf("Push first: %v", err)
	}

	store, err := NewStore(pgURL, "agentsview", true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	noteOne := "important one"
	if _, err := store.PinMessage(
		"pg-pin-rewrite", 1, &noteOne,
	); err != nil {
		t.Fatalf("PinMessage one: %v", err)
	}
	noteTwo := "important two"
	if _, err := store.PinMessage(
		"pg-pin-rewrite", 2, &noteTwo,
	); err != nil {
		t.Fatalf("PinMessage two: %v", err)
	}

	sess.MessageCount = 4
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession second: %v", err)
	}
	if err := local.ReplaceSessionMessages(
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
	); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}

	if _, err := ps.Push(ctx, true, nil); err != nil {
		t.Fatalf("Push rewrite: %v", err)
	}

	pins, err := store.ListPinnedMessages(ctx, "pg-pin-rewrite", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("pins = %d, want 2", len(pins))
	}

	byNote := map[string]db.PinnedMessage{}
	for _, pin := range pins {
		if pin.Note == nil {
			t.Fatalf("pin note = nil, want populated note")
		}
		byNote[*pin.Note] = pin
	}
	if pin, ok := byNote[noteOne]; !ok {
		t.Fatalf("pin for %q missing: %v", noteOne, pins)
	} else if pin.MessageID != 2 || pin.Ordinal != 2 {
		t.Fatalf(
			"pin one message/ordinal = %d/%d, want 2/2",
			pin.MessageID, pin.Ordinal,
		)
	}
	if pin, ok := byNote[noteTwo]; !ok {
		t.Fatalf("pin for %q missing: %v", noteTwo, pins)
	} else if pin.MessageID != 3 || pin.Ordinal != 3 {
		t.Fatalf(
			"pin two message/ordinal = %d/%d, want 3/3",
			pin.MessageID, pin.Ordinal,
		)
	}
}

func TestReconcilePinnedMessagesPrefersCurrentTargetPin(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_duplicate_test"
	pg, err := Open(pgURL, schema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	if _, err := pg.ExecContext(
		ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
	); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	if _, err := pg.ExecContext(ctx, `
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('pg-pin-duplicate', 'machine-a', 'proj-curation',
			 'codex', 'duplicate source repair',
			 '2026-05-01T00:00:00Z'::timestamptz, 3, 1)`,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := pg.ExecContext(ctx, `
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
			 'uuid-answer')`,
	); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	if _, err := pg.ExecContext(ctx, `
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('pg-pin-duplicate', 1, 1, 'uuid-answer',
			 'stale note',
			 '2026-05-01T00:01:00Z'::timestamptz),
			('pg-pin-duplicate', 2, 2, 'uuid-answer',
			 'current note',
			 '2026-05-01T00:02:00Z'::timestamptz)`,
	); err != nil {
		t.Fatalf("insert pins: %v", err)
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := reconcilePinnedMessages(
		ctx, tx, "pg-pin-duplicate",
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("reconcilePinnedMessages: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	store, err := NewStore(pgURL, schema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	pins, err := store.ListPinnedMessages(ctx, "pg-pin-duplicate", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("pins = %d, want 1: %v", len(pins), pins)
	}
	if pins[0].MessageID != 2 || pins[0].Ordinal != 2 {
		t.Fatalf(
			"pin message/ordinal = %d/%d, want 2/2",
			pins[0].MessageID, pins[0].Ordinal,
		)
	}
	if pins[0].Note == nil || *pins[0].Note != "current note" {
		t.Fatalf("pin note = %v, want current note", pins[0].Note)
	}
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	if _, err := pg.ExecContext(
		ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
	); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	if _, err := pg.ExecContext(ctx, `
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('pg-pin-source-gone', 'machine-a', 'proj-curation',
			 'codex', 'source uuid gone',
			 '2026-05-01T00:00:00Z'::timestamptz, 2, 1)`,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := pg.ExecContext(ctx, `
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-source-gone', 0, 'user', 'question',
			 '2026-05-01T00:00:00Z'::timestamptz, 8,
			 'uuid-question'),
			('pg-pin-source-gone', 1, 'assistant', 'new answer',
			 '2026-05-01T00:00:01Z'::timestamptz, 10,
			 'uuid-new-answer')`,
	); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	if _, err := pg.ExecContext(ctx, `
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('pg-pin-source-gone', 1, 1, 'uuid-gone-forever',
			 'stale pin',
			 '2026-05-01T00:01:00Z'::timestamptz)`,
	); err != nil {
		t.Fatalf("insert pin: %v", err)
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := reconcilePinnedMessages(
		ctx, tx, "pg-pin-source-gone",
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("reconcilePinnedMessages: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	store, err := NewStore(pgURL, schema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	pins, err := store.ListPinnedMessages(ctx, "pg-pin-source-gone", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages: %v", err)
	}
	if len(pins) != 0 {
		t.Fatalf(
			"pins = %d, want 0 (stale source_uuid should be pruned): %v",
			len(pins), pins,
		)
	}
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
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	if _, err := pg.ExecContext(
		ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
	); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	if _, err := pg.ExecContext(ctx, `
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('pg-pin-dup-uuid', 'machine-a', 'proj-curation',
			 'claude', 'duplicate source uuid',
			 '2026-05-01T00:00:00Z'::timestamptz, 3, 1)`,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := pg.ExecContext(ctx, `
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
			 'uuid-shared')`,
	); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	if _, err := pg.ExecContext(ctx, `
		INSERT INTO pinned_messages
			(session_id, message_id, ordinal, source_uuid,
			 note, created_at)
		VALUES
			('pg-pin-dup-uuid', 2, 2, 'uuid-shared',
			 'pin on later duplicate',
			 '2026-05-01T00:01:00Z'::timestamptz)`,
	); err != nil {
		t.Fatalf("insert pin: %v", err)
	}

	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := reconcilePinnedMessages(
		ctx, tx, "pg-pin-dup-uuid",
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("reconcilePinnedMessages: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	store, err := NewStore(pgURL, schema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	pins, err := store.ListPinnedMessages(ctx, "pg-pin-dup-uuid", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("pins = %d, want 1: %v", len(pins), pins)
	}
	if pins[0].MessageID != 2 || pins[0].Ordinal != 2 {
		t.Fatalf(
			"pin message/ordinal = %d/%d, want 2/2 (must not "+
				"relocate to lower-ordinal duplicate)",
			pins[0].MessageID, pins[0].Ordinal,
		)
	}
}

// TestPinMessageRepinRefreshesSourceUUID covers re-pinning the same
// (session_id, message_id). The stored source_uuid must reflect the
// message currently at message_id; otherwise the next reconciliation
// would follow the stale uuid away from where the user just pinned.
func TestPinMessageRepinRefreshesSourceUUID(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_pin_repin_test"
	pg, err := Open(pgURL, schema, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pg.Close()
	defer func() {
		_, _ = pg.ExecContext(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
	}()

	ctx := context.Background()
	if _, err := pg.ExecContext(
		ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
	); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if err := EnsureSchema(ctx, pg, schema); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	if _, err := pg.ExecContext(ctx, `
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('pg-pin-repin', 'machine-a', 'proj-curation',
			 'claude', 'repin refreshes uuid',
			 '2026-05-01T00:00:00Z'::timestamptz, 2, 1)`,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := pg.ExecContext(ctx, `
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, source_uuid)
		VALUES
			('pg-pin-repin', 0, 'user', 'question',
			 '2026-05-01T00:00:00Z'::timestamptz, 8,
			 'uuid-question'),
			('pg-pin-repin', 1, 'assistant', 'original',
			 '2026-05-01T00:00:01Z'::timestamptz, 8,
			 'uuid-original')`,
	); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	store, err := NewStore(pgURL, schema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	originalNote := "first"
	if _, err := store.PinMessage("pg-pin-repin", 1, &originalNote); err != nil {
		t.Fatalf("PinMessage initial: %v", err)
	}
	var initialSourceUUID, initialCreatedAt string
	if err := pg.QueryRowContext(ctx, `
		SELECT source_uuid, created_at::text
		FROM pinned_messages
		WHERE session_id = $1 AND message_id = $2`,
		"pg-pin-repin", 1,
	).Scan(&initialSourceUUID, &initialCreatedAt); err != nil {
		t.Fatalf("query initial pin: %v", err)
	}
	if initialSourceUUID != "uuid-original" {
		t.Fatalf(
			"initial source_uuid = %q, want uuid-original",
			initialSourceUUID,
		)
	}

	// Simulate a session rewrite that replaces the message at
	// ordinal 1 with a different message (different source_uuid)
	// while reusing the ordinal.
	if _, err := pg.ExecContext(ctx, `
		UPDATE messages
		SET source_uuid = 'uuid-replacement',
			content = 'replaced'
		WHERE session_id = $1 AND ordinal = $2`,
		"pg-pin-repin", 1,
	); err != nil {
		t.Fatalf("update message source_uuid: %v", err)
	}

	updatedNote := "second"
	if _, err := store.PinMessage("pg-pin-repin", 1, &updatedNote); err != nil {
		t.Fatalf("PinMessage repin: %v", err)
	}

	var gotSourceUUID, gotCreatedAt string
	var gotNote *string
	if err := pg.QueryRowContext(ctx, `
		SELECT source_uuid, note, created_at::text
		FROM pinned_messages
		WHERE session_id = $1 AND message_id = $2`,
		"pg-pin-repin", 1,
	).Scan(&gotSourceUUID, &gotNote, &gotCreatedAt); err != nil {
		t.Fatalf("query repinned pin: %v", err)
	}
	if gotSourceUUID != "uuid-replacement" {
		t.Fatalf(
			"after repin source_uuid = %q, want uuid-replacement "+
				"(stale source_uuid would steer the next reconciliation "+
				"away from the current message)",
			gotSourceUUID,
		)
	}
	if gotNote == nil || *gotNote != updatedNote {
		t.Fatalf("after repin note = %v, want %q", gotNote, updatedNote)
	}
	if gotCreatedAt != initialCreatedAt {
		t.Fatalf(
			"after repin created_at = %q, want %q (must be preserved)",
			gotCreatedAt, initialCreatedAt,
		)
	}
}
