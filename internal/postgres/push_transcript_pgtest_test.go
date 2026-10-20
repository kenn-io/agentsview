//go:build pgtest

package postgres

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// Local sync can rewrite a transcript while a push copies it page by page.
// A pinned message that moves into a page the push already copied must keep
// its pin.
func TestPushKeepsPinWhenTranscriptShiftsDuringCopy(t *testing.T) {
	const id = "claude:pin-shift"
	const schema = "agentsview_pin_shift_test"
	const total = db.MaxMessageLimit + 500
	const dropped = 300
	ctx := t.Context()
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	local := testDB(t)
	transcript := func(from int) []db.Message {
		msgs := make([]db.Message, 0, total-from)
		for i := from; i < total; i++ {
			msgs = append(msgs, db.Message{
				SessionID: id, Ordinal: i - from, Role: "user",
				Content: fmt.Sprintf("message %d", i), SourceUUID: fmt.Sprintf("uuid-%d", i),
				Timestamp: "2026-09-25T12:00:00Z",
			})
		}
		return msgs
	}
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: id, Agent: "claude", Project: "sample", Machine: "machine",
		MessageCount: total, CreatedAt: "2026-09-25T12:00:00Z",
	}))
	require.NoError(t, local.InsertMessages(ctx, transcript(0)))
	syncer, err := New(pgURL, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	store := &Store{pg: syncer.pg}
	pinned := db.MaxMessageLimit + 200
	_, err = store.PinMessage(ctx, id, int64(pinned), new("remote note"))
	require.NoError(t, err)

	// Drop the first messages once the first page is copied, so the pinned
	// message moves into the range the push has already read.
	syncer.afterMessagePage = sync.OnceFunc(func() {
		require.NoError(t, local.ReplaceSessionMessages(ctx, id, transcript(dropped)))
	})
	result, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)

	pins, err := store.ListPinnedMessages(ctx, id, "")
	require.NoError(t, err)
	require.Len(t, pins, 1, "the push dropped a pin whose message moved during the copy")
	assert.Equal(t, pinned, pins[0].Ordinal, "the push copied a transcript that changed after it started")

	// The rewrite landed after the push's cutoff, so the next incremental push
	// copies it, and the pin follows its message.
	syncer.afterMessagePage = nil
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	pins, err = store.ListPinnedMessages(ctx, id, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, pinned-dropped, pins[0].Ordinal)
}

// An agent that keeps writing to a long session adds messages while the push
// copies it. The copy must stop at the transcript it started with, keep the
// pin whose message is still there, and drop only the pin whose message is
// gone, as it would without the writes.
func TestPushCopiesSessionThatGrowsDuringTheCopy(t *testing.T) {
	const id = "claude:pin-grow"
	const schema = "agentsview_pin_grow_test"
	const total = db.MaxMessageLimit + 500
	const orphaned = 5
	ctx := t.Context()
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	local := testDB(t)
	message := func(ordinal int) db.Message {
		return db.Message{
			SessionID: id, Ordinal: ordinal, Role: "user",
			Content: fmt.Sprintf("message %d", ordinal), SourceUUID: fmt.Sprintf("uuid-%d", ordinal),
			Timestamp: "2026-09-25T12:00:00Z",
		}
	}
	transcript := func() []db.Message {
		msgs := make([]db.Message, 0, total)
		for i := range total {
			msgs = append(msgs, message(i))
		}
		return msgs
	}
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: id, Agent: "claude", Project: "sample", Machine: "machine",
		MessageCount: total, CreatedAt: "2026-09-25T12:00:00Z",
	}))
	require.NoError(t, local.InsertMessages(ctx, transcript()))
	syncer, err := New(pgURL, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	store := &Store{pg: syncer.pg}
	pinned := db.MaxMessageLimit + 200
	_, err = store.PinMessage(ctx, id, int64(pinned), new("remote note"))
	require.NoError(t, err)
	_, err = store.PinMessage(ctx, id, orphaned, new("orphaned note"))
	require.NoError(t, err)
	// Replace the orphaned pin's message, so that pin has nothing to match.
	rewritten := transcript()
	rewritten[orphaned].SourceUUID = "uuid-replaced"
	rewritten[orphaned].Content = "replaced message"
	require.NoError(t, local.ReplaceSessionMessages(ctx, id, rewritten))

	next, pages := total, 0
	syncer.afterMessagePage = func() {
		pages++
		require.LessOrEqual(t, pages, 3, "the copy kept following new messages")
		require.NoError(t, local.InsertMessages(ctx, []db.Message{message(next)}))
		next++
	}
	result, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors, "the push gave up on a session that only grew")

	pins, err := store.ListPinnedMessages(ctx, id, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, pinned, pins[0].Ordinal)
	tail, err := store.GetMessages(ctx, id, total-1, 10, true)
	require.NoError(t, err)
	assert.Len(t, tail, 1, "the copy included messages written after it started")
}
