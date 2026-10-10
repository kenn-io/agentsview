package remotesync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestAntigravityTitleOnlyRemoteDelta(t *testing.T) {
	for _, mode := range []string{"database", "wal", "corrupt database"} {
		t.Run(mode, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			extracted := t.TempDir()
			const remoteRoot = "/srv/antigravity"
			const bare = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			const id = "remote~antigravity:" + bare
			root := remappedRemotePath(extracted, remoteRoot)
			transcript := filepath.Join(root, "brain", bare, ".system_generated", "logs", "transcript.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o755))
			require.NoError(t, os.WriteFile(transcript, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-20T22:10:00Z","content":"remote title fixture"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T22:10:01Z","content":"answer"}
`), 0o600))
			titlePath := filepath.Join(root, "conversation_summaries.db")
			conn, err := sql.Open("sqlite3", titlePath)
			require.NoError(t, err)
			_, err = conn.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT); INSERT INTO conversation_summaries VALUES (?, 'old')`, bare)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentAntigravity: {remoteRoot}}}
			importer := Importer{Host: "remote", DB: database, Root: extracted, Targets: targets}
			_, err = importer.ImportExtracted(t.Context(), targets, extracted)
			require.NoError(t, err)
			before, err := database.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.NotEmpty(t, before)
			display := "user override"
			require.NoError(t, database.RenameSession(t.Context(), id, &display))
			stored, err := database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			require.NotNil(t, stored.FilePath)
			assert.Equal(t, "remote:/srv/antigravity/brain/"+bare+"/.system_generated/logs/transcript.jsonl", *stored.FilePath)

			changed := titlePath
			if mode == "corrupt database" {
				require.NoError(t, os.WriteFile(titlePath, []byte("not a database"), 0o600))
			} else {
				conn, err = sql.Open("sqlite3", titlePath)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, conn.Close()) })
				if mode == "wal" {
					_, err = conn.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0`)
					require.NoError(t, err)
					changed += "-wal"
				}
				_, err = conn.Exec(`UPDATE conversation_summaries SET title = 'renamed' WHERE conversation_id = ?`, bare)
				require.NoError(t, err)
				require.FileExists(t, changed)
			}
			prepare := func(path string) *PreparedDeltaImport {
				t.Helper()
				relative, err := mirrorRelativeLocalChangePath(extracted, path)
				require.NoError(t, err)
				pending, err := importer.PreparePending(t.Context(), DeltaImportRequest{Journal: MirrorChangeJournal{Version: mirrorJournalVersion, Entries: []MirrorChangeEntry{{Path: relative}}}})
				require.NoError(t, err)
				assert.Empty(t, pending.plan.Files)
				assert.Empty(t, pending.plan.FallbackProviders)
				require.Len(t, pending.plan.SharedTitleTasks, 1)
				return pending
			}
			pending := prepare(changed)
			stats, err := pending.Execute(t.Context())
			if mode == "corrupt database" {
				require.Error(t, err)
				assert.Equal(t, JournalProcessingFailures, stats.JournalOutcome)
			} else {
				require.NoError(t, err)
				assert.Equal(t, JournalRetired, stats.JournalOutcome)
				assert.Equal(t, 1, stats.TitlesUpdated)
			}
			assert.Zero(t, stats.FilesProcessed)
			assert.Zero(t, stats.SessionsSynced)
			stored, err = database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, stored.SessionName)
			if mode == "corrupt database" {
				assert.Equal(t, "old", *stored.SessionName)
			} else {
				assert.Equal(t, "renamed", *stored.SessionName)
			}
			after, err := database.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, before, after, "title-only deltas must preserve message identities and content")
			require.NotNil(t, stored.DisplayName)
			assert.Equal(t, display, *stored.DisplayName)
			if mode != "corrupt database" {
				_, err = conn.Exec(`UPDATE conversation_summaries SET title = '' WHERE conversation_id = ?`, bare)
				require.NoError(t, err)
				stats, err = prepare(changed).Execute(t.Context())
				require.NoError(t, err)
				assert.Equal(t, JournalRetired, stats.JournalOutcome)
				assert.Equal(t, 1, stats.TitlesUpdated)
				stored, err = database.GetSessionFull(t.Context(), id)
				require.NoError(t, err)
				assert.True(t, stored.SessionName == nil || *stored.SessionName == "")
			}
		})
	}
}
