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
