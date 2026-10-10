package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
)

func TestCodexRevertPageSessionIDs(t *testing.T) {
	const (
		thread  = "codex:11111111-1111-4111-8111-111111111111"
		other   = "codex:66666666-6666-4666-8666-666666666666"
		pageA   = thread + "_22222222-2222-4222-8222-222222222222"
		pageB   = thread + "_33333333-3333-4333-8333-333333333333"
		trashed = thread + "_44444444-4444-4444-8444-444444444444"
	)
	d := testDB(t)
	for _, id := range []string{
		thread, pageA, pageB, trashed,
		thread + "x",
		other, other + "_22222222-2222-4222-8222-222222222222",
		"host~" + pageA,
	} {
		insertSession(t, d, id, "project")
	}
	require.NoError(t, d.SoftDeleteSession(t.Context(), trashed))

	ids, err := d.CodexRevertPageSessionIDs(t.Context(), thread)
	require.NoError(t, err)
	assert.Equal(t, []string{pageA, pageB}, ids)

	ids, err = d.CodexRevertPageSessionIDs(t.Context(), "host~"+thread)
	require.NoError(t, err)
	assert.Equal(t, []string{"host~" + pageA}, ids)

	ids, err = d.CodexRevertPageSessionIDs(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestCodexRevertPageSessionIDsReportsQueryError(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.Close())
	_, err := d.CodexRevertPageSessionIDs(t.Context(), "codex:11111111-1111-4111-8111-111111111111")
	assert.Error(t, err)
}

// Before dataVersion 130 a revert page's parse overwrote its thread's row. On
// resync that stale row must go wherever the page's file now lives and for
// every Codex-format agent, while a row whose file names no live session stays.
func TestCopyOrphanedDataFromDropsStaleCodexFormatThreadRows(t *testing.T) {
	const (
		thread = "11111111-1111-4111-8111-111111111111"
		page   = "22222222-2222-4222-8222-222222222222"
		gone   = "33333333-3333-4333-8333-333333333333"
	)
	dir := t.TempDir()
	pageName := "rollout-2026-09-25T12-00-00-" + thread + "_" + page + ".jsonl"
	datedPath := filepath.Join(dir, "sessions", "2026", "09", "25", pageName)
	archivedPath := filepath.Join(dir, "archived_sessions", pageName)
	gonePath := filepath.Join(dir, "sessions", "2026", "09", "25", "rollout-2026-09-25T13-00-00-"+thread+"_"+gone+".jsonl")
	withFile := func(agent string, path *string) func(*Session) {
		return func(s *Session) { s.Agent, s.FilePath = agent, path }
	}

	srcPath := filepath.Join(dir, "old.db")
	src := testDBAtPath(t, srcPath, "src")
	insertSession(t, src, "codex:"+thread, "proj", withFile("codex", &datedPath))
	insertSession(t, src, "traex:"+thread, "proj", withFile("traex", &datedPath))
	insertSession(t, src, "augure-code:"+thread, "proj", withFile("augure-code", &gonePath))
	src.Close()

	dstPath := filepath.Join(dir, "new.db")
	dst := testDBAtPath(t, dstPath, "dst")
	defer dst.Close()
	insertSession(t, dst, "codex:"+thread+"_"+page, "proj", withFile("codex", &archivedPath))
	insertSession(t, dst, "traex:"+thread+"_"+page, "proj", withFile("traex", &datedPath))

	ids, err := dst.CopyOrphanedDataFromExcluding(srcPath, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"augure-code:" + thread}, ids,
		"only the row whose page file names no live session is an orphan")
}

func TestCopyOrphanedCodexPagePreservesArchivedContent(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name   string
		policy config.ArchiveContent
		active bool
	}{
		{"full_active", config.ArchiveContentFull, true},
		{"full_cold", config.ArchiveContentFull, false},
		{"transcripts", config.ArchiveContentTranscripts, true},
		{"usage", config.ArchiveContentUsage, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			srcPath := filepath.Join(dir, "old.db")
			src := testDBAtPath(t, srcPath, "src")
			insertSession(t, src, thread, "archived-project", func(s *Session) {
				s.Agent = "codex"
				s.FilePath = new(filepath.Join(dir, "rollout-2026-09-25T12-00-00-"+page[len("codex:"):]+".jsonl"))
			})
			insertMessages(t, src, Message{
				SessionID: thread, Role: "assistant", Content: "Saved answer", SourceUUID: "reply",
				OutputTokens: 30, HasOutputTokens: true, HasToolUse: true,
				ToolCalls: []ToolCall{{
					SessionID: thread, ToolName: "Read", Category: "Read", ToolUseID: "call-read",
					InputJSON: `{"path":"sample.go"}`, ResultContent: "Saved result", ResultContentLength: 12,
					ResultEvents: []ToolResultEvent{{ToolUseID: "call-read", Source: "function_call_output", Content: "Saved result", ContentLength: 12}},
				}},
			})
			require.NoError(t, src.ReplaceSessionUsageEvents(ctx, thread, []UsageEvent{{
				SessionID: thread, Source: "codex", Model: "gpt-5", OutputTokens: 30, DedupKey: "saved-usage",
			}}))
			_, err := src.getWriter().Exec(ctx, `UPDATE session_project_identity_snapshots SET root_path='/work/saved-project' WHERE session_id=?`, thread)
			require.NoError(t, err)
			var oldMessageID string
			if tc.active {
				changes, err := src.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				require.Len(t, changes.Changes, 1)
				oldMessageID = changes.Changes[0].MessageID
			}
			_, err = src.getWriter().Exec(ctx, "PRAGMA user_version = 129")
			require.NoError(t, err)
			require.NoError(t, src.Close())

			dst := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
			defer dst.Close()
			dst.SetArchiveContent(tc.policy)
			require.NoError(t, dst.CopyArchiveIdentityFrom(srcPath))
			insertSession(t, dst, thread, "original-project")
			insertMessages(t, dst, Message{SessionID: thread, Role: "assistant", Content: "Original answer", SourceUUID: "reply"})
			ids, err := dst.CopyOrphanedDataFromExcluding(srcPath, nil)
			require.NoError(t, err)
			assert.Equal(t, []string{page}, ids)
			require.NoError(t, dst.CopySessionMetadataFrom(srcPath))
			messages, err := dst.GetAllMessages(ctx, page)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, 30, messages[0].OutputTokens)
			if tc.policy == config.ArchiveContentUsage {
				assert.Empty(t, messages[0].Content)
			} else {
				assert.Equal(t, "Saved answer", messages[0].Content)
			}
			switch tc.policy {
			case config.ArchiveContentFull:
				require.Len(t, messages[0].ToolCalls, 1)
				assert.Equal(t, "Saved result", messages[0].ToolCalls[0].ResultContent)
				var result string
				require.NoError(t, dst.Reader().QueryRow(ctx, `SELECT content FROM tool_result_events WHERE session_id=?`, page).Scan(&result))
				assert.Equal(t, "Saved result", result)
			case config.ArchiveContentTranscripts:
				var result string
				require.NoError(t, dst.Reader().QueryRow(ctx, `SELECT content FROM tool_result_events WHERE session_id=?`, page).Scan(&result))
				assert.Empty(t, result)
			default:
				var resultCount int
				require.NoError(t, dst.Reader().QueryRow(ctx, `SELECT COUNT(*) FROM tool_result_events WHERE session_id=?`, page).Scan(&resultCount))
				assert.Zero(t, resultCount)
			}
			usage, err := dst.GetUsageEvents(ctx, page)
			require.NoError(t, err)
			require.Len(t, usage, 1)
			assert.Equal(t, 30, usage[0].OutputTokens)
			var rootPath string
			require.NoError(t, dst.Reader().QueryRow(ctx, `SELECT root_path FROM session_project_identity_snapshots WHERE session_id=?`, page).Scan(&rootPath))
			assert.Equal(t, "/work/saved-project", rootPath)
			if tc.policy != config.ArchiveContentUsage {
				changes, err := dst.ExportConversationChanges(ctx, ConversationExportOptions{})
				require.NoError(t, err)
				var contents []string
				for _, change := range changes.Changes {
					if change.MessageID == "" {
						continue // Session-level project changes have no message body.
					}
					if change.SessionID != page {
						assert.NotEqual(t, oldMessageID, change.MessageID)
						continue
					}
					if tc.active {
						assert.Equal(t, oldMessageID, change.MessageID)
					}
					body, err := dst.GetConversationMessage(ctx, ConversationMessageOptions{DatabaseID: changes.DatabaseID, SessionID: page, MessageID: change.MessageID, Revision: change.Revision})
					require.NoError(t, err)
					require.NotNil(t, body.Text)
					contents = append(contents, *body.Text)
				}
				assert.Equal(t, []string{"Saved answer"}, contents)
			}
		})
	}
}

func TestCopyOrphanedCodexPageHonorsExclusionsAndUpgradeVersion(t *testing.T) {
	const thread = "host_a~traex:11111111-1111-4111-8111-111111111111"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name           string
		version        int
		excluded       string
		parserExcluded bool
		trash          bool
		headMissing    bool
		wantPage       bool
		archiveOnly    bool
	}{
		{name: "upgrade", version: 129, wantPage: true},
		{name: "current", version: 130},
		{name: "archive_only_upgrade", version: 129, archiveOnly: true, wantPage: true},
		{name: "archive_only_current", version: 130, archiveOnly: true},
		{name: "deleted_thread", version: 129, excluded: thread},
		{name: "deleted_page", version: 129, excluded: page},
		{name: "parser_excluded", version: 129, excluded: page, parserExcluded: true},
		{name: "trash_already_copied", version: 129, trash: true},
		{name: "deleted_page_without_head", version: 129, excluded: page, headMissing: true},
		{name: "parser_excluded_without_head", version: 129, excluded: page, parserExcluded: true, headMissing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			srcPath := filepath.Join(dir, "old.db")
			src := testDBAtPath(t, srcPath, "src")
			insertSession(t, src, thread, "project", func(s *Session) {
				s.Agent = "traex"
				s.FilePath = new(`C:\sessions\rollout-2026-09-25T12-00-00-11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222.jsonl`)
			})
			insertMessages(t, src, Message{SessionID: thread, Role: "assistant", Content: "Archived page"})
			_, err := src.getWriter().Exec(ctx, fmt.Sprintf("PRAGMA user_version = %d", tc.version))
			require.NoError(t, err)
			if tc.archiveOnly {
				require.NoError(t, src.EnableArchiveOnly(ctx))
			}
			require.NoError(t, src.Close())
			dst := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
			defer dst.Close()
			if !tc.headMissing {
				insertSession(t, dst, thread, "project")
			}
			var excluded []string
			if tc.excluded != "" {
				if tc.parserExcluded {
					excluded = []string{tc.excluded}
				} else {
					_, err = dst.getWriter().Exec(ctx, `INSERT INTO excluded_sessions(id) VALUES (?)`, tc.excluded)
					require.NoError(t, err)
				}
			}
			if tc.trash {
				require.NoError(t, dst.SoftDeleteSession(ctx, thread))
			}
			ids, err := dst.CopyOrphanedDataFromExcluding(srcPath, excluded)
			require.NoError(t, err)
			if tc.wantPage {
				assert.Equal(t, []string{page}, ids)
			} else {
				assert.Empty(t, ids)
			}
		})
	}
}

func TestCopySessionMetadataFromCodexPageProjectSnapshot(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name        string
		version     int
		headMissing bool
	}{
		{name: "upgrade", version: 129},
		{name: "upgrade_without_head", version: 129, headMissing: true},
		{name: "already_upgraded", version: 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			cwd := filepath.Join(dir, "sample-feature")
			srcPath := filepath.Join(dir, "old.db")
			src := testDBAtPath(t, srcPath, "src")
			// Codex retains cwd across undo but records the current branch.
			// With an absent checkout, the parser removes a matching branch
			// suffix: feature gives sample, while other gives sample_feature.
			insertSession(t, src, thread, "sample_feature", func(s *Session) {
				s.Agent, s.Cwd = "codex", cwd
				s.FilePath = new(filepath.Join(dir, "rollout-2026-09-25T12-00-00-"+page[len("codex:"):]+".jsonl"))
			})
			oldObserved := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
			require.NoError(t, src.UpsertProjectIdentityObservation(ctx, export.ProjectIdentityObservation{
				SessionID: thread, Project: "sample_feature", Machine: "local", RootPath: cwd,
				GitBranch: "other", ObservedAt: oldObserved,
			}))
			_, err := src.getWriter().Exec(ctx, fmt.Sprintf("PRAGMA user_version = %d", tc.version))
			require.NoError(t, err)
			require.NoError(t, src.Close())

			dst := testDB(t)
			require.NoError(t, dst.CopyArchiveIdentityFrom(srcPath))
			for _, session := range []struct{ id, project, branch string }{
				{thread, "sample", "feature"}, {page, "sample_feature", "other"},
			} {
				if tc.headMissing && session.id == thread {
					continue
				}
				insertSession(t, dst, session.id, session.project)
				require.NoError(t, dst.UpsertProjectIdentityObservation(ctx, export.ProjectIdentityObservation{
					SessionID: session.id, Project: session.project, Machine: "local", RootPath: cwd,
					GitBranch: session.branch, ObservedAt: oldObserved.Add(time.Hour),
				}))
			}
			before, err := dst.ListSessionProjectIdentitySnapshotsByID(ctx, []string{thread, page})
			require.NoError(t, err)
			require.NoError(t, dst.CopySessionMetadataFrom(srcPath))
			after, err := dst.ListSessionProjectIdentitySnapshotsByID(ctx, []string{thread, page})
			require.NoError(t, err)
			if tc.version < 130 {
				assert.Equal(t, before[thread], after[thread], "page evidence must not overwrite the head")
				assert.Equal(t, "sample_feature", after[page].Project)
				assert.Equal(t, "other", after[page].GitBranch)
				assert.Equal(t, oldObserved, after[page].ObservedAt)
			} else {
				assert.Equal(t, "sample_feature", after[thread].Project)
				assert.Equal(t, oldObserved, after[thread].ObservedAt)
				assert.Equal(t, before[page], after[page], "later rebuilds retain same-ID snapshot copying")
			}
		})
	}
}

// Old deletion records retain only their thread ID. Pages need not be present
// when that scope is migrated or when a later rebuild copies it again.
func TestCopyExcludedSessionsFromRetainsCodexThreadScope(t *testing.T) {
	for _, migrateFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("migrate_first_%t", migrateFirst), func(t *testing.T) {
			ctx := t.Context()
			sourcePath := filepath.Join(t.TempDir(), "old.db")
			conn, err := sql.Open("sqlite3", sourcePath)
			require.NoError(t, err)
			_, err = conn.ExecContext(ctx, v06LegacySchema+`
				CREATE TABLE excluded_sessions (
					id TEXT PRIMARY KEY,
					created_at TEXT NOT NULL DEFAULT '2026-09-01T00:00:00Z'
				);
				PRAGMA user_version = 129;`)
			require.NoError(t, err)
			const thread = "11111111-1111-4111-8111-111111111111"
			const suffix = "_22222222-2222-4222-8222-222222222222"
			for _, prefix := range []string{"codex:", "traex:", "host_a~augure-code:", "other:"} {
				_, err = conn.ExecContext(ctx, "INSERT INTO excluded_sessions(id) VALUES (?)", prefix+thread)
				require.NoError(t, err)
			}
			require.NoError(t, conn.Close())
			if migrateFirst {
				source, err := OpenIsolated(ctx, sourcePath)
				require.NoError(t, err)
				assert.True(t, source.IsSessionExcluded(ctx, "codex:"+thread), "schema migration preserves the deletion")
				require.NoError(t, source.Close())
			}
			for _, phase := range []string{"upgrade", "later_rebuild"} {
				dstPath := filepath.Join(t.TempDir(), phase+".db")
				dst := testDBAtPath(t, dstPath, phase)
				require.NoError(t, dst.CopyExcludedSessionsFrom(sourcePath))
				for _, tc := range []struct {
					prefix       string
					wantExcluded bool
				}{
					{"codex:", true},
					{"traex:", true},
					{"host_a~augure-code:", true},
					{"host_b~augure-code:", false},
					{"other:", false},
				} {
					page := Session{ID: tc.prefix + thread + suffix, Agent: "codex", Project: "sample", Machine: "local"}
					assert.Equal(t, tc.wantExcluded, dst.IsSessionExcluded(ctx, page.ID), phase+" "+page.ID)
					err := dst.UpsertSession(ctx, page)
					if tc.wantExcluded {
						require.ErrorIs(t, err, ErrSessionExcluded)
						require.ErrorIs(t, dst.insertSessionIfAbsent(ctx, page), ErrSessionExcluded)
					} else {
						require.NoError(t, err)
					}
				}
				require.NoError(t, dst.Close())
				sourcePath = dstPath
			}
		})
	}
}

func TestCopyExcludedCodexThreadFromArchiveOnly(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	for _, version := range []int{129, 130} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx := t.Context()
			source := testDB(t)
			insertSession(t, source, thread, "project")
			require.NoError(t, source.DeleteSession(ctx, thread))
			_, err := source.getWriter().Exec(ctx, fmt.Sprintf("PRAGMA user_version = %d", version))
			require.NoError(t, err)
			require.NoError(t, source.EnableArchiveOnly(ctx))
			path := source.Path()
			require.NoError(t, source.Close())
			destination := testDB(t)
			require.NoError(t, destination.CopyExcludedSessionsFrom(path))
			err = destination.UpsertSession(ctx, Session{ID: page, Agent: "codex", Project: "project", Machine: "local"})
			if version < 130 {
				require.ErrorIs(t, err, ErrSessionExcluded)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
