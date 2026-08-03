package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

type replayingRecallBackend struct {
	first  bun.IDB
	second bun.IDB
}

func (*replayingRecallBackend) Name() string { return "replaying-recall" }

func (*replayingRecallBackend) ReadOnly() bool { return true }

func (*replayingRecallBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{Recall: true}
}

func (*replayingRecallBackend) SessionQueryDialect() QueryDialect {
	return PortableBunSessionQueryDialect()
}

func (*replayingRecallBackend) SessionVersion(
	context.Context, bun.IDB, string,
) (int, int64, error) {
	return 0, 0, sql.ErrNoRows
}

func (b *replayingRecallBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	return fn(b.second)
}

func (b *replayingRecallBackend) ConsistentView(
	_ context.Context, fn func(bun.IDB) error,
) error {
	if err := fn(b.first); err != nil {
		return err
	}
	return fn(b.second)
}

func (*replayingRecallBackend) Update(
	context.Context, func(bun.IDB) error,
) error {
	return ErrReadOnly
}

func TestGetRecallEntryPublishesOnlyAcceptedReplayAttempt(t *testing.T) {
	first := testDB(t)
	second := testDB(t)
	require.NoError(t, first.UpsertSession(Session{
		ID: "replayed-source", Project: "recall", Machine: "host", Agent: "codex",
	}))
	_, err := first.InsertRecallEntry(RecallEntry{
		ID: "replayed-entry", Type: "fact", Scope: "global",
		Title: "Rejected first attempt", Body: "This row disappears before retry.",
		SourceSessionID: "replayed-source", Transferable: true, ProvenanceOK: true,
	})
	require.NoError(t, err)

	store := NewBunStore(&replayingRecallBackend{
		first: first.bunReader, second: second.bunReader,
	})
	entry, err := store.GetRecallEntry(t.Context(), "replayed-entry")
	require.NoError(t, err)
	assert.Nil(t, entry)
}
