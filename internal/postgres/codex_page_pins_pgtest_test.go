//go:build pgtest

package postgres

import (
	"database/sql"
	"encoding/json/v2"
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
			const schema = "agentsview_codex_page_pin_test"
			ctx := t.Context()
			pgURL := testPGURL(t)
			cleanNamedPGSchema(t, pgURL, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
			source := testDB(t)
			sourcePath := source.Path()
			pagePath := filepath.Join(t.TempDir(), "rollout-2026-09-25T12-00-00-"+threadUUID+"_22222222-2222-4222-8222-222222222222.jsonl")
			headPath := filepath.Join(t.TempDir(), "rollout-2026-09-25T11-00-00-"+threadUUID+".jsonl")
			require.NoError(t, source.UpsertSession(ctx, db.Session{
				ID: thread, Agent: tc.agent, Project: "sample", Machine: "machine",
				FilePath: &pagePath, MessageCount: 1, CreatedAt: "2026-09-25T12:00:00Z",
			}))
			require.NoError(t, source.InsertMessages(ctx, []db.Message{{
				SessionID: thread, Role: "assistant", Content: "answer saved only on page", SourceUUID: "page-answer",
			}}))
			require.NoError(t, source.SetSessionDataVersion(ctx, thread, 130))
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
			_, err = raw.ExecContext(ctx, "PRAGMA user_version = 130")
			require.NoError(t, err)
			require.NoError(t, raw.Close())

			rebuilt := testDB(t)
			require.NoError(t, rebuilt.CopyArchiveIdentityFrom(sourcePath))
			require.NoError(t, rebuilt.CopySyncStateFrom(sourcePath))
			for _, row := range []struct{ id, path, content, uuid string }{
				{thread, headPath, "original answer", "head-answer"},
				{page, pagePath, "answer saved only on page", "page-answer"},
			} {
				if tc.name == "page_only" && row.id == thread {
					continue
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

func TestCodexThreadCorrectionPreservesPostgresPins(t *testing.T) {
	for _, mode := range []string{"full", "original_absent", "original_first", "corrected_first", "noncanonical", "plain_to_compressed", "compressed_to_plain", "excluded_destination"} {
		t.Run(mode, func(t *testing.T) {
			const oldID = "codex:11111111-1111-7111-8111-111111111111"
			const correctedID = "codex:22222222-2222-7222-8222-222222222222"
			const schema = "agentsview_codex_thread_pin_test"
			ctx := t.Context()
			pgURL := testPGURL(t)
			cleanNamedPGSchema(t, pgURL, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
			source := testDB(t)
			path := filepath.Join(t.TempDir(), "rollout-2026-10-16T12-00-00-22222222-2222-7222-8222-222222222222.jsonl")
			if mode == "noncanonical" {
				path = filepath.Join(t.TempDir(), "rollout-restored.jsonl")
			}
			if mode == "compressed_to_plain" {
				path += ".zst"
			}
			require.NoError(t, source.UpsertSession(ctx, db.Session{ID: oldID, Agent: "codex", Project: "sample", Machine: "machine", FilePath: &path, MessageCount: 1}))
			require.NoError(t, source.InsertMessages(ctx, []db.Message{{SessionID: oldID, Role: "user", Content: "saved corrected transcript", SourceUUID: "corrected-message"}}))
			require.NoError(t, source.SetSessionDataVersion(ctx, oldID, 135))
			syncer, err := New(pgURL, schema, source, "machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, syncer.Close()) })
			result, err := syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			store := &Store{pg: syncer.pg}
			pinID, err := store.PinMessage(ctx, oldID, 0, new("remote-only note"))
			require.NoError(t, err)
			localPins, err := source.ListPinnedMessages(ctx, "", "")
			require.NoError(t, err)
			require.Empty(t, localPins)
			require.NoError(t, source.Close())
			rebuilt := testDB(t)
			require.NoError(t, rebuilt.CopyArchiveIdentityFrom(source.Path()))
			require.NoError(t, rebuilt.CopySyncStateFrom(source.Path()))
			for _, id := range []string{oldID, correctedID} {
				if id == oldID && mode == "original_absent" {
					continue
				}
				file, content, uuid := path, "saved corrected transcript", "corrected-message"
				if mode == "plain_to_compressed" {
					file += ".zst"
				}
				if mode == "compressed_to_plain" {
					file = strings.TrimSuffix(file, ".zst")
				}
				if id == oldID {
					file = filepath.Join(t.TempDir(), "rollout-original.jsonl")
					content, uuid = "original transcript", "original-message"
				}
				require.NoError(t, rebuilt.UpsertSession(ctx, db.Session{ID: id, Agent: "codex", Project: "sample", Machine: "machine", FilePath: &file, MessageCount: 1}))
				require.NoError(t, rebuilt.InsertMessages(ctx, []db.Message{{SessionID: id, Role: "user", Content: content, SourceUUID: uuid}}))
				require.NoError(t, rebuilt.SetSessionDataVersion(ctx, id, db.CurrentDataVersion()))
			}
			upgraded, err := New(pgURL, schema, rebuilt, "machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
			if mode == "excluded_destination" {
				_, err = syncer.pg.ExecContext(ctx, `INSERT INTO excluded_sessions(id) VALUES($1)`, correctedID)
				require.NoError(t, err)
			}
			if mode == "original_first" || mode == "corrected_first" {
				upgraded.archiveID, err = rebuilt.GetArchiveID(ctx)
				require.NoError(t, err)
				upgraded.databaseGeneration, err = rebuilt.GetDatabaseID(ctx)
				require.NoError(t, err)
				marker, err := upgraded.pushMarkerID(ctx)
				require.NoError(t, err)
				order := []string{oldID, correctedID}
				if mode == "corrected_first" {
					order = []string{correctedID, oldID}
				}
				var pushed []db.Session
				for _, id := range order {
					sess, err := rebuilt.GetSessionFull(ctx, id)
					require.NoError(t, err)
					require.NotNil(t, sess)
					batch, err := upgraded.pushBatch(ctx, []db.Session{*sess}, true, marker, nil, nil, &pushed)
					require.NoError(t, err)
					require.True(t, batch.ok)
				}
			}
			result, err = upgraded.Push(ctx, true, nil)
			if mode == "excluded_destination" {
				require.Error(t, err)
				pins, err := store.ListPinnedMessages(ctx, "", "")
				require.NoError(t, err)
				require.Len(t, pins, 1)
				assert.Equal(t, oldID, pins[0].SessionID)
				assert.Equal(t, "remote-only note", *pins[0].Note)
				return
			}
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			pins, err := store.ListPinnedMessages(ctx, "", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, correctedID, pins[0].SessionID)
			assert.Equal(t, pinID, pins[0].ID)
			require.NotNil(t, pins[0].Note)
			assert.Equal(t, "remote-only note", *pins[0].Note)
			require.NotNil(t, pins[0].Content)
			assert.Equal(t, "saved corrected transcript", *pins[0].Content)
			if mode == "original_absent" {
				old, err := store.GetSessionFull(ctx, oldID)
				require.NoError(t, err)
				assert.Nil(t, old)
			}
		})
	}
}

func TestCodexCorrectionLookupStaysBounded(t *testing.T) {
	const schema = "agentsview_codex_source_lookup_test"
	_, store := prepareUsageSchema(t, schema)
	ctx := t.Context()
	_, err := store.DB().ExecContext(ctx, `INSERT INTO sessions(id,agent,project,machine,file_path,data_version)
 VALUES ('codex:old','codex','sample','machine','/source/rollout.jsonl',135)`)
	require.NoError(t, err)
	var small float64
	for _, count := range []int{100, 10000} {
		if count == 10000 {
			_, err = store.DB().ExecContext(ctx, "DROP INDEX idx_sessions_codex_repair_source")
			require.NoError(t, err)
			syncer := &Sync{pg: store.DB(), schema: schema}
			require.NoError(t, syncer.EnsureSchema(ctx))
		}
		_, err = store.DB().ExecContext(ctx, `INSERT INTO sessions(id,agent,project,machine,file_path,data_version)
   SELECT 'codex:noise-'||n,'codex','sample','machine','/source/noise-'||n||'.jsonl',135
   FROM generate_series(1,$1::int)n ON CONFLICT(id) DO NOTHING`, count)
		require.NoError(t, err)
		_, err = store.DB().ExecContext(ctx, "ANALYZE sessions")
		require.NoError(t, err)
		tx, err := store.DB().BeginTx(ctx, nil)
		require.NoError(t, err)
		var raw []byte
		require.NoError(t, tx.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+codexIdentityCandidatesSQL,
			[]string{"codex:new"}, []string{"/source/rollout.jsonl"}).Scan(&raw))
		require.NoError(t, tx.Rollback())
		var plans []struct{ Plan map[string]any }
		require.NoError(t, json.Unmarshal(raw, &plans))
		require.Len(t, plans, 1)
		plan := plans[0].Plan
		assert.Equal(t, float64(1), plan["Actual Rows"])
		buffers := plan["Shared Hit Blocks"].(float64) + plan["Shared Read Blocks"].(float64)
		if count == 100 {
			small = buffers
		} else {
			assert.LessOrEqual(t, buffers, small+20)
		}
		t.Logf("sessions=%d buffers=%.0f", count, buffers)
	}
}

func TestCodexOverlappingCorrectionsPreserveRemotePins(t *testing.T) {
	for _, mode := range []string{"chain", "cycle", "chain_first_batch", "cycle_first_batch", "chain_destination_first", "chain_shared", "cycle_shared", "chain_first_batch_shared", "chain_shared_destination"} {
		t.Run(mode, func(t *testing.T) {
			const schema = "agentsview_codex_pin_chain"
			ctx := t.Context()
			pgURL := testPGURL(t)
			cleanNamedPGSchema(t, pgURL, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
			source := testDB(t)
			root := t.TempDir()
			pathA, pathB, pathC := filepath.Join(root, "rollout-a.jsonl"), filepath.Join(root, "rollout-b.jsonl"), filepath.Join(root, "rollout-c.jsonl")
			lastPath, lastContent, lastUUID := pathC, "transcript C", "message-c"
			lastID, lastNote := "codex:c", "note C"
			if strings.HasPrefix(mode, "cycle") {
				lastPath, lastContent, lastUUID = pathA, "transcript A", "message-a"
				lastID, lastNote = "codex:a", "note A"
			}
			for _, old := range []struct{ id, path, content, uuid string }{
				{"codex:a", pathB, "transcript B", "message-b"},
				{"codex:b", lastPath, lastContent, lastUUID},
			} {
				if strings.Contains(mode, "shared") {
					old.content, old.uuid = "shared pinned message", ""
				}
				require.NoError(t, source.UpsertSession(ctx, db.Session{ID: old.id, Agent: "codex", Project: "sample", Machine: "machine", FilePath: &old.path, MessageCount: 1}))
				require.NoError(t, source.InsertMessages(ctx, []db.Message{{SessionID: old.id, Role: "user", Content: old.content, SourceUUID: old.uuid}}))
				require.NoError(t, source.SetSessionDataVersion(ctx, old.id, 135))
			}
			if mode == "chain_shared_destination" {
				require.NoError(t, source.UpsertSession(ctx, db.Session{ID: "codex:c", Agent: "codex", Project: "sample", Machine: "machine", FilePath: &pathC, MessageCount: 2}))
				require.NoError(t, source.InsertMessages(ctx, []db.Message{{SessionID: "codex:c", Role: "user", Content: "shared pinned message"}, {SessionID: "codex:c", Ordinal: 1, Role: "user", Content: "unrelated destination message"}}))
				require.NoError(t, source.SetSessionDataVersion(ctx, "codex:c", db.CurrentDataVersion()))
			}
			syncer, err := New(pgURL, schema, source, "machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, syncer.Close()) })
			result, err := syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			store := &Store{pg: syncer.pg}
			_, err = store.PinMessage(ctx, "codex:a", 0, new("note B"))
			require.NoError(t, err)
			_, err = store.PinMessage(ctx, "codex:b", 0, &lastNote)
			require.NoError(t, err)
			if mode == "chain_shared_destination" {
				_, err = store.PinMessage(ctx, "codex:c", 1, new("destination note"))
				require.NoError(t, err)
			}
			require.NoError(t, source.Close())
			rebuilt := testDB(t)
			require.NoError(t, rebuilt.CopyArchiveIdentityFrom(source.Path()))
			require.NoError(t, rebuilt.CopySyncStateFrom(source.Path()))
			for _, corrected := range []struct{ id, path, content, uuid string }{
				{"codex:a", pathA, "transcript A", "message-a"},
				{"codex:b", pathB, "transcript B", "message-b"},
				{"codex:c", pathC, "transcript C", "message-c"},
			} {
				if strings.HasPrefix(mode, "cycle") && corrected.id == "codex:c" {
					continue
				}
				if strings.Contains(mode, "shared") {
					corrected.content, corrected.uuid = "shared pinned message", ""
				}
				require.NoError(t, rebuilt.UpsertSession(ctx, db.Session{ID: corrected.id, Agent: "codex", Project: "sample", Machine: "machine", FilePath: &corrected.path, MessageCount: 1}))
				require.NoError(t, rebuilt.InsertMessages(ctx, []db.Message{{SessionID: corrected.id, Role: "user", Content: corrected.content, SourceUUID: corrected.uuid}}))
				if mode == "chain_shared_destination" && corrected.id == "codex:c" {
					require.NoError(t, rebuilt.InsertMessages(ctx, []db.Message{{SessionID: "codex:c", Ordinal: 1, Role: "user", Content: "unrelated destination message"}}))
				}
				require.NoError(t, rebuilt.SetSessionDataVersion(ctx, corrected.id, db.CurrentDataVersion()))
			}
			upgraded, err := New(pgURL, schema, rebuilt, "machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
			if strings.Contains(mode, "batch") || mode == "chain_destination_first" {
				upgraded.archiveID, err = rebuilt.GetArchiveID(ctx)
				require.NoError(t, err)
				upgraded.databaseGeneration, err = rebuilt.GetDatabaseID(ctx)
				require.NoError(t, err)
				marker, err := upgraded.pushMarkerID(ctx)
				require.NoError(t, err)
				firstID := "codex:a"
				if mode == "chain_destination_first" {
					firstID = "codex:c"
				}
				first, err := rebuilt.GetSessionFull(ctx, firstID)
				require.NoError(t, err)
				require.NotNil(t, first)
				var pushed []db.Session
				batch, err := upgraded.pushBatch(ctx, []db.Session{*first}, true, marker, nil, nil, &pushed)
				require.NoError(t, err)
				require.True(t, batch.ok)
			}
			result, err = upgraded.Push(ctx, true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			pins, err := store.ListPinnedMessages(ctx, "", "")
			require.NoError(t, err)
			notes := map[string]string{}
			for _, pin := range pins {
				if pin.Note != nil {
					notes[fmt.Sprintf("%s:%d", pin.SessionID, pin.Ordinal)] = *pin.Note
				}
			}
			expected := map[string]string{"codex:b:0": "note B", lastID + ":0": lastNote}
			if mode == "chain_shared_destination" {
				expected["codex:c:1"] = "destination note"
			}
			assert.Equal(t, expected, notes)
		})
	}
}
