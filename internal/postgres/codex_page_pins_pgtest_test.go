//go:build pgtest

package postgres

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestCodexPageUpgradePreservesPostgresPins(t *testing.T) {
	for _, tc := range []struct{ name, prefix, agent string }{
		{"full_push", "codex:", "codex"},
		{"misfiled_thread", "codex:", "codex"},
		{"misfiled_thread_only", "codex:", "codex"},
		{"misfiled_thread_only_moved", "codex:", "codex"},
		{"misfiled_thread_only_compressed", "codex:", "codex"},
		{"misfiled_thread_only_uncompressed", "codex:", "codex"},
		{"head_first", "host~traex:", "traex"},
		{"page_first", "augure-code:", "augure-code"},
		{"page_only", "codex:", "codex"},
		{"other_owner", "codex:", "codex"},
		{"changed_page", "codex:", "codex"},
		{"filtered_page", "codex:", "codex"},
		{"filtered_head", "codex:", "codex"},
		{"excluded_head", "codex:", "codex"},
		{"filtered_head_excluded_page", "codex:", "codex"},
		{"excluded_page", "codex:", "codex"},
		{"foreign_page", "codex:", "codex"},
		{"missing_page", "codex:", "codex"},
		{"missing_unpinned_page", "codex:", "codex"},
		{"filtered_unpinned_page", "codex:", "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const threadUUID = "11111111-1111-4111-8111-111111111111"
			thread := tc.prefix + threadUUID
			page := thread + "_22222222-2222-4222-8222-222222222222"
			version := 130
			if strings.HasPrefix(tc.name, "misfiled_thread") {
				page = tc.prefix + "22222222-2222-4222-8222-222222222222"
				version = 135
			}
			const schema = "agentsview_codex_page_pin_test"
			ctx := t.Context()
			pgURL := testPGURL(t)
			cleanNamedPGSchema(t, pgURL, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
			source := testDB(t)
			sourcePath := source.Path()
			pagePath := filepath.Join(t.TempDir(), "rollout-2026-09-25T12-00-00-"+threadUUID+"_22222222-2222-4222-8222-222222222222.jsonl")
			if strings.HasPrefix(tc.name, "misfiled_thread") {
				pagePath = filepath.Join(t.TempDir(), "rollout-2026-09-25T12-00-00-22222222-2222-4222-8222-222222222222.jsonl")
			}
			if tc.name == "misfiled_thread_only_uncompressed" {
				pagePath += ".zst"
			}
			headPath := filepath.Join(t.TempDir(), "rollout-2026-09-25T11-00-00-"+threadUUID+".jsonl")
			require.NoError(t, source.UpsertSession(ctx, db.Session{
				ID: thread, Agent: tc.agent, Project: "sample", Machine: "machine",
				FilePath: &pagePath, MessageCount: 1, CreatedAt: "2026-09-25T12:00:00Z",
			}))
			require.NoError(t, source.InsertMessages(ctx, []db.Message{{
				SessionID: thread, Role: "assistant", Content: "answer saved only on page", SourceUUID: "page-answer",
			}}))
			require.NoError(t, source.SetSessionDataVersion(ctx, thread, version))
			syncer, err := New(pgURL, schema, source, "machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, syncer.Close()) })
			result, err := syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			store := &Store{pg: syncer.pg}
			pinID, err := store.PinMessage(ctx, thread, 0, new("saved remotely"))
			require.NoError(t, err)
			require.NotZero(t, pinID)
			before, err := store.ListPinnedMessages(ctx, thread, "")
			require.NoError(t, err)
			require.Len(t, before, 1)
			if tc.name == "missing_unpinned_page" || tc.name == "filtered_unpinned_page" {
				require.NoError(t, store.UnpinMessage(ctx, thread, before[0].MessageID))
			}
			if tc.name == "other_owner" {
				_, err = syncer.pg.ExecContext(ctx, `UPDATE sessions SET owner_marker='another-archive' WHERE id=$1`, thread)
				require.NoError(t, err)
			}
			require.NoError(t, source.Close())
			raw, err := sql.Open("sqlite3", sourcePath)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version))
			require.NoError(t, err)
			require.NoError(t, raw.Close())

			rebuilt := testDB(t)
			require.NoError(t, rebuilt.CopyArchiveIdentityFrom(sourcePath))
			require.NoError(t, rebuilt.CopySyncStateFrom(sourcePath))
			for _, row := range []struct{ id, path, content, uuid string }{
				{thread, headPath, "original answer", "head-answer"},
				{page, pagePath, "answer saved only on page", "page-answer"},
			} {
				if (tc.name == "page_only" || strings.HasPrefix(tc.name, "misfiled_thread_only")) && row.id == thread {
					continue
				}
				if row.id == page {
					if tc.name == "misfiled_thread_only_moved" {
						row.path = filepath.Join(t.TempDir(), "archived_sessions", filepath.Base(row.path))
					}
					if tc.name == "misfiled_thread_only_compressed" {
						row.path += ".zst"
					}
					if tc.name == "misfiled_thread_only_uncompressed" {
						row.path = strings.TrimSuffix(row.path, ".zst")
					}
				}
				if tc.name == "changed_page" && row.id == page {
					row.content, row.uuid = "unrelated replacement", "replacement-answer"
				}
				require.NoError(t, rebuilt.UpsertSession(ctx, db.Session{
					ID: row.id, Agent: tc.agent, Project: "sample", Machine: "machine",
					FilePath: new(row.path), MessageCount: 1, CreatedAt: "2026-09-25T12:00:00Z",
				}))
				require.NoError(t, rebuilt.InsertMessages(ctx, []db.Message{{
					SessionID: row.id, Role: "assistant", Content: row.content, SourceUUID: row.uuid,
				}}))
				require.NoError(t, rebuilt.SetSessionDataVersion(ctx, row.id, db.CurrentDataVersion()))
			}
			_, err = rebuilt.CopyOrphanedDataFromExcluding(sourcePath, nil)
			require.NoError(t, err)
			require.NoError(t, rebuilt.CopySessionMetadataFrom(sourcePath))
			opts := storage.PusherOptions{}
			blocked := false
			switch tc.name {
			case "filtered_head", "excluded_head", "filtered_head_excluded_page":
				_, err = rebuilt.AssignSessionProject(ctx, thread, "head_project")
				require.NoError(t, err)
				if tc.name == "excluded_head" {
					opts.ExcludeProjects = []string{"head_project"}
				} else {
					opts.Projects = []string{"sample"}
				}
				if tc.name == "filtered_head_excluded_page" {
					_, err = syncer.pg.ExecContext(ctx, `INSERT INTO excluded_sessions(id) VALUES($1)`, page)
					require.NoError(t, err)
					blocked = true
				}
			case "filtered_page", "filtered_unpinned_page":
				_, err = rebuilt.AssignSessionProject(ctx, page, "page_project")
				require.NoError(t, err)
				opts.ExcludeProjects = []string{"page_project"}
				blocked = true
			case "excluded_page":
				_, err = syncer.pg.ExecContext(ctx, `INSERT INTO excluded_sessions(id) VALUES($1)`, page)
				require.NoError(t, err)
				blocked = true
			case "foreign_page":
				_, err = syncer.pg.ExecContext(ctx, `INSERT INTO sessions(id, project, agent, machine, owner_marker) VALUES($1,'sample','codex','another-machine','another-owner')`, page)
				require.NoError(t, err)
				blocked = true
			case "missing_page", "missing_unpinned_page":
				require.NoError(t, rebuilt.DeleteSession(ctx, page))
				blocked = true
			}
			if tc.name == "missing_unpinned_page" || tc.name == "filtered_unpinned_page" {
				blocked = false
			}
			upgraded, err := New(pgURL, schema, rebuilt, "machine", true, opts)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
			if tc.name == "head_first" || tc.name == "page_first" {
				// Drive separate real transactions to exercise both batch orders.
				upgraded.archiveID, err = rebuilt.GetArchiveID(ctx)
				require.NoError(t, err)
				upgraded.databaseGeneration, err = rebuilt.GetDatabaseID(ctx)
				require.NoError(t, err)
				marker, err := upgraded.pushMarkerID(ctx)
				require.NoError(t, err)
				order := []string{thread, page}
				if tc.name == "page_first" {
					order = []string{page, thread}
				}
				var pushed []db.Session
				for _, id := range order {
					sess, err := rebuilt.GetSessionFull(ctx, id)
					require.NoError(t, err)
					require.NotNil(t, sess)
					result, err := upgraded.pushBatch(ctx, []db.Session{*sess}, true, marker, nil, nil, &pushed)
					require.NoError(t, err)
					require.True(t, result.ok)
				}
			}

			result, err = upgraded.Push(ctx, true, nil)
			if blocked {
				require.Error(t, err)
				retained, err := store.ListPinnedMessages(ctx, "", "")
				require.NoError(t, err)
				require.Len(t, retained, 1)
				assert.Equal(t, thread, retained[0].SessionID)
				require.NotNil(t, retained[0].Note)
				assert.Equal(t, "saved remotely", *retained[0].Note)
				require.NotNil(t, retained[0].Content)
				assert.Equal(t, "answer saved only on page", *retained[0].Content)
				return
			}
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			if tc.name == "filtered_head" || tc.name == "excluded_head" {
				head, err := store.GetSessionFull(ctx, thread)
				require.NoError(t, err)
				assert.Nil(t, head, "the original rollout's project is outside the push scope")
				result, err = upgraded.Push(ctx, false, nil)
				require.NoError(t, err)
				require.Zero(t, result.Errors)
			}
			if tc.name == "missing_unpinned_page" || tc.name == "filtered_unpinned_page" {
				messages, err := store.GetMessages(ctx, thread, 0, 10, true)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "original answer", messages[0].Content)
				return
			}
			pageMessages, err := store.GetMessages(ctx, page, 0, 10, true)
			require.NoError(t, err)
			require.Len(t, pageMessages, 1)
			after, err := store.ListPinnedMessages(ctx, "", "")
			require.NoError(t, err)
			if tc.name == "changed_page" {
				assert.Empty(t, after, "a removed message must not pin an unrelated replacement")
				return
			}
			if strings.HasPrefix(tc.name, "misfiled_thread_only") {
				old, err := store.GetSessionFull(ctx, thread)
				require.NoError(t, err)
				assert.Nil(t, old, "the duplicate remote transcript must be retired")
			}
			require.Len(t, after, 1, "the remote-only pin must survive the page split and a later full push")
			wantSession := page
			if tc.name == "other_owner" {
				wantSession = thread
			}
			assert.Equal(t, wantSession, after[0].SessionID)
			assert.Equal(t, pinID, after[0].ID)
			assert.Equal(t, before[0].CreatedAt, after[0].CreatedAt)
			require.NotNil(t, after[0].Note)
			assert.Equal(t, "saved remotely", *after[0].Note)
			require.NotNil(t, after[0].Content)
			assert.Equal(t, "answer saved only on page", *after[0].Content)
		})
	}
}

func TestCodexChainedIdentityUpgradePreservesPostgresPins(t *testing.T) {
	ctx := t.Context()
	const (
		a      = "codex:11111111-1111-4111-8111-111111111111"
		b      = "codex:22222222-2222-4222-8222-222222222222"
		c      = "codex:33333333-3333-4333-8333-333333333333"
		schema = "agentsview_codex_chained_pin_test"
	)
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	root := t.TempDir()
	pathB := filepath.Join(root, "rollout-2026-09-25T12-00-00-22222222-2222-4222-8222-222222222222.jsonl")
	pathC := filepath.Join(root, "rollout-2026-09-25T13-00-00-33333333-3333-4333-8333-333333333333.jsonl")
	source := testDB(t)
	sourcePath := source.Path()
	// Model the old parser's last-header result: rollout B has headers B, A;
	// rollout C has headers C, B. Their corrected identities overlap at B.
	for _, row := range []struct{ id, path, content, uuid string }{
		{a, pathB, "answer for B", "answer-b"},
		{b, pathC, "answer for C", "answer-c"},
	} {
		require.NoError(t, source.UpsertSession(ctx, db.Session{
			ID: row.id, Agent: "codex", Project: "sample", Machine: "machine",
			FilePath: new(row.path), MessageCount: 1, CreatedAt: "2026-09-25T12:00:00Z",
		}))
		require.NoError(t, source.InsertMessages(ctx, []db.Message{{
			SessionID: row.id, Role: "assistant", Content: row.content, SourceUUID: row.uuid,
		}}))
		require.NoError(t, source.SetSessionDataVersion(ctx, row.id, 135))
	}
	syncer, err := New(pgURL, schema, source, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	store := &Store{pg: syncer.pg}
	_, err = store.PinMessage(ctx, a, 0, new("saved B remotely"))
	require.NoError(t, err)
	_, err = store.PinMessage(ctx, b, 0, new("saved C remotely"))
	require.NoError(t, err)
	before, err := store.ListPinnedMessages(ctx, "", "")
	require.NoError(t, err)
	require.Len(t, before, 2)
	require.NoError(t, source.Close())
	raw, err := sql.Open("sqlite3", sourcePath)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, "PRAGMA user_version=135")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	rebuilt := testDB(t)
	require.NoError(t, rebuilt.CopyArchiveIdentityFrom(sourcePath))
	require.NoError(t, rebuilt.CopySyncStateFrom(sourcePath))
	for _, row := range []struct{ id, path, content, uuid string }{
		{b, pathB, "answer for B", "answer-b"},
		{c, pathC, "answer for C", "answer-c"},
	} {
		require.NoError(t, rebuilt.UpsertSession(ctx, db.Session{
			ID: row.id, Agent: "codex", Project: "sample", Machine: "machine",
			FilePath: new(row.path), MessageCount: 1, CreatedAt: "2026-09-25T12:00:00Z",
		}))
		require.NoError(t, rebuilt.InsertMessages(ctx, []db.Message{{
			SessionID: row.id, Role: "assistant", Content: row.content, SourceUUID: row.uuid,
		}}))
		require.NoError(t, rebuilt.SetSessionDataVersion(ctx, row.id, db.CurrentDataVersion()))
	}
	_, err = rebuilt.CopyOrphanedDataFromExcluding(sourcePath, nil)
	require.NoError(t, err)
	require.NoError(t, rebuilt.CopySessionMetadataFrom(sourcePath))
	upgraded, err := New(pgURL, schema, rebuilt, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	result, err = upgraded.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	for _, row := range []struct{ id, content string }{{b, "answer for B"}, {c, "answer for C"}} {
		messages, err := store.GetMessages(ctx, row.id, 0, 10, true)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		require.Equal(t, row.content, messages[0].Content)
	}
	after, err := store.ListPinnedMessages(ctx, "", "")
	require.NoError(t, err)
	assert.Len(t, after, 2, "both remote-only pins must survive overlapping identity corrections")
	for _, old := range before {
		wantSession := b
		if old.SessionID == b {
			wantSession = c
		}
		var found bool
		for _, pin := range after {
			if pin.ID == old.ID {
				found = true
				assert.Equal(t, wantSession, pin.SessionID)
				assert.Equal(t, old.Note, pin.Note)
				assert.Equal(t, old.Content, pin.Content)
				assert.Equal(t, old.CreatedAt, pin.CreatedAt)
			}
		}
		assert.True(t, found, "pin for %s must survive under %s", old.SessionID, wantSession)
	}
}
