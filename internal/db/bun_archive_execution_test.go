package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

type archiveWriteHook struct{ sawVersionWrite bool }

func (h *archiveWriteHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	if strings.Contains(event.Query, "UPDATE sessions SET") && strings.Contains(event.Query, "data_version =") {
		h.sawVersionWrite = true
	}
	return ctx
}
func (*archiveWriteHook) AfterQuery(context.Context, *bun.QueryEvent) {}

func TestArchiveBatchQueriesReachBunHooks(t *testing.T) {
	database := testDB(t)
	hook := new(archiveWriteHook)
	database.bunWriter = database.bunWriter.WithQueryHook(hook)
	_, err := database.WriteSessionAtomic(SessionBatchWrite{
		Session:  Session{ID: "bun-batch", Project: "before", Agent: "claude", MessageCount: 1},
		Messages: []Message{userMsg("bun-batch", 0, "hello")}, DataVersion: 100,
	})
	require.NoError(t, err)
	require.True(t, hook.sawVersionWrite, "archive data-version writes must pass through Bun hooks")
	expected := errors.New("rollback fixture")
	err = database.Update(func(tx bun.Tx) error {
		_, err := tx.NewRaw("UPDATE sessions SET project = ? WHERE id = ?", "after", "bun-batch").Exec(t.Context())
		require.NoError(t, err)
		return expected
	})
	require.ErrorIs(t, err, expected)
	session, err := database.GetSession(t.Context(), "bun-batch")
	require.NoError(t, err)
	require.Equal(t, "before", session.Project)
}

func TestArchiveBunPreservesInternalTextKeys(t *testing.T) {
	database := testDB(t)
	key := "pricing\x00policy"
	require.NoError(t, database.Update(func(tx bun.Tx) error {
		_, err := tx.NewRaw("INSERT INTO pg_sync_state(key, value) VALUES (?, ?)", key, "stored").Exec(t.Context())
		return err
	}))
	var storedKey string
	require.NoError(t, database.Reader().QueryRow("SELECT key FROM pg_sync_state WHERE value = ?", "stored").Scan(&storedKey))
	require.Equal(t, key, storedKey)
}
