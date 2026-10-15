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

// TestCodexPurgedRolloutExclusionBlocksCanonicalPage covers a bare-rollout
// page the user permanently purged remotely: only its exclusion remains. The
// upgraded push must not republish the page under its canonical id.
func TestCodexPurgedRolloutExclusionBlocksCanonicalPage(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const rollout = "codex:22222222-2222-4222-8222-222222222222"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	const schema = "agentsview_codex_purged_rollout_test"
	ctx := t.Context()
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	local := testDB(t)
	pagePath := filepath.Join(t.TempDir(), "rollout-2026-09-25T12-00-00-"+page[len("codex:"):]+".jsonl")
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: page, Agent: "codex", Project: "sample", Machine: "machine",
		FilePath: &pagePath, MessageCount: 1, CreatedAt: "2026-09-25T12:00:00Z",
		StartedAt: new("2026-09-25T12:00:00Z"),
	}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{
		SessionID: page, Role: "assistant", Content: "page answer",
		Timestamp: "2026-09-25T12:00:00Z",
	}}))
	syncer, err := New(pgURL, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	require.NoError(t, syncer.EnsureSchema(ctx))
	// The remote bare-rollout row is already purged; only its exclusion stays.
	_, err = syncer.pg.ExecContext(ctx, `INSERT INTO excluded_sessions (id) VALUES ($1)`, rollout)
	require.NoError(t, err)

	pushed, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, pushed.Errors)

	store := &Store{pg: syncer.pg}
	row, err := store.GetSessionFull(ctx, page)
	require.NoError(t, err)
	assert.Nil(t, row, "the purged page stays excluded under its canonical id")
	var excluded bool
	require.NoError(t, syncer.pg.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM excluded_sessions WHERE id = $1)`, page).Scan(&excluded))
	assert.True(t, excluded)
}
