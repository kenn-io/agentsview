//go:build pgtest

package postgres

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// TestCodexLegacyHeaderPageMovesToCanonicalPostgresRow publishes a page under
// its bare rollout id, as data version 133 did, then pushes the same page under
// <thread>_<rollout>. The remote title, star and pin follow the page and the
// bare-rollout duplicate disappears.
func TestCodexLegacyHeaderPageMovesToCanonicalPostgresRow(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const rollout = "codex:22222222-2222-4222-8222-222222222222"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	const schema = "agentsview_codex_legacy_page_test"
	ctx := t.Context()
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	source := testDB(t)
	sourcePath := source.Path()
	pagePath := filepath.Join(t.TempDir(), "rollout-2026-09-25T12-00-00-"+page[len("codex:"):]+".jsonl")
	sess := db.Session{
		ID: rollout, Agent: "codex", Project: "sample", Machine: "machine",
		FilePath: &pagePath, MessageCount: 1, CreatedAt: "2026-09-25T12:00:00Z",
		StartedAt: new("2026-09-25T12:00:00Z"),
	}
	require.NoError(t, source.UpsertSession(ctx, sess))
	message := db.Message{
		SessionID: rollout, Role: "assistant", Content: "legacy page answer",
		Timestamp: "2026-09-25T12:00:00Z",
	}
	require.NoError(t, source.InsertMessages(ctx, []db.Message{message}))
	require.NoError(t, source.SetSessionDataVersion(ctx, rollout, 133))
	syncer, err := New(pgURL, schema, source, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	pushed, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, pushed.Errors)

	store := &Store{pg: syncer.pg}
	require.NoError(t, store.RenameSession(ctx, rollout, new("remote page title")))
	starred, err := store.StarSession(ctx, rollout)
	require.NoError(t, err)
	require.True(t, starred)
	_, err = store.PinMessage(ctx, rollout, 0, new("remote note"))
	require.NoError(t, err)

	require.NoError(t, source.Close())
	raw, err := sql.Open("sqlite3", sourcePath)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, "PRAGMA user_version=133")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	rebuilt := testDB(t)
	require.NoError(t, rebuilt.CopyArchiveIdentityFrom(sourcePath))
	require.NoError(t, rebuilt.CopySyncStateFrom(sourcePath))
	sess.ID = page
	sess.ParentSessionID = new(thread)
	sess.RelationshipType = "continuation"
	require.NoError(t, rebuilt.UpsertSession(ctx, sess))
	message.SessionID = page
	require.NoError(t, rebuilt.InsertMessages(ctx, []db.Message{message}))
	require.NoError(t, rebuilt.SetSessionDataVersion(ctx, page, db.CurrentDataVersion()))

	upgraded, err := New(pgURL, schema, rebuilt, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	for range 2 {
		pushed, err = upgraded.Push(ctx, true, nil)
		require.NoError(t, err)
		require.Zero(t, pushed.Errors)
	}

	old, err := store.GetSessionFull(ctx, rollout)
	require.NoError(t, err)
	assert.Nil(t, old, "the bare-rollout duplicate is retired")
	pageRow, err := store.GetSessionFull(ctx, page)
	require.NoError(t, err)
	require.NotNil(t, pageRow)
	require.NotNil(t, pageRow.DisplayName)
	assert.Equal(t, "remote page title", *pageRow.DisplayName)
	stars, err := store.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{page}, stars)
	pins, err := store.ListPinnedMessages(ctx, "", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, page, pins[0].SessionID)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, "remote note", *pins[0].Note)
	pageMessages, err := store.GetMessages(ctx, page, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, pageMessages, 1)
}

// TestCodexRejectedHeaderPageStaysPlainPostgresRow covers a page the parser
// rejected because its header thread differs from the filename thread. The
// rebuilt archive keeps it under the bare rollout id with no canonical page, so
// the remote row and its curation must stay put instead of failing the push or
// moving to a duplicate.
func TestCodexRejectedHeaderPageStaysPlainPostgresRow(t *testing.T) {
	const rollout = "codex:22222222-2222-4222-8222-222222222222"
	const fileKey = "11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"
	const schema = "agentsview_codex_rejected_page_test"
	ctx := t.Context()
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	source := testDB(t)
	sourcePath := source.Path()
	pagePath := filepath.Join(t.TempDir(), "rollout-2026-09-25T12-00-00-"+fileKey+".jsonl")
	sess := db.Session{
		ID: rollout, Agent: "codex", Project: "sample", Machine: "machine",
		FilePath: &pagePath, MessageCount: 1, CreatedAt: "2026-09-25T12:00:00Z",
		StartedAt: new("2026-09-25T12:00:00Z"),
	}
	require.NoError(t, source.UpsertSession(ctx, sess))
	message := db.Message{
		SessionID: rollout, Role: "assistant", Content: "plain answer",
		Timestamp: "2026-09-25T12:00:00Z",
	}
	require.NoError(t, source.InsertMessages(ctx, []db.Message{message}))
	require.NoError(t, source.SetSessionDataVersion(ctx, rollout, 133))
	syncer, err := New(pgURL, schema, source, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	pushed, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, pushed.Errors)

	store := &Store{pg: syncer.pg}
	require.NoError(t, store.RenameSession(ctx, rollout, new("remote title")))
	_, err = store.PinMessage(ctx, rollout, 0, new("remote note"))
	require.NoError(t, err)

	require.NoError(t, source.Close())
	raw, err := sql.Open("sqlite3", sourcePath)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, "PRAGMA user_version=133")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	rebuilt := testDB(t)
	require.NoError(t, rebuilt.CopyArchiveIdentityFrom(sourcePath))
	require.NoError(t, rebuilt.CopySyncStateFrom(sourcePath))
	require.NoError(t, rebuilt.UpsertSession(ctx, sess))
	require.NoError(t, rebuilt.InsertMessages(ctx, []db.Message{message}))
	require.NoError(t, rebuilt.SetSessionDataVersion(ctx, rollout, db.CurrentDataVersion()))

	upgraded, err := New(pgURL, schema, rebuilt, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	pushed, err = upgraded.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, pushed.Errors)

	row, err := store.GetSessionFull(ctx, rollout)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NotNil(t, row.DisplayName)
	assert.Equal(t, "remote title", *row.DisplayName)
	dup, err := store.GetSessionFull(ctx, "codex:"+fileKey)
	require.NoError(t, err)
	assert.Nil(t, dup, "no duplicate canonical row")
	pins, err := store.ListPinnedMessages(ctx, "", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, rollout, pins[0].SessionID)
}
