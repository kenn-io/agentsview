package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	identityThreadA = "11111111-1111-7111-8111-111111111111"
	identityThreadB = "22222222-2222-7222-8222-222222222222"
)

func writeCodexIdentityRollouts(t *testing.T) (root, pathA, pathB string) {
	t.Helper()
	root = t.TempDir()
	const ts = "2025-10-20T19:00:00Z"
	metaA := testjsonl.CodexSessionMetaJSON(identityThreadA, "/work/project-a", "codex_cli_rs", ts)
	pathA = writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-00-00-"+identityThreadA+".jsonl", testjsonl.JoinJSONL(
		metaA,
		testjsonl.CodexMsgJSON("user", "first question", ts),
		testjsonl.CodexMsgJSON("assistant", "first answer", ts),
		testjsonl.CodexMsgJSON("user", "second question", ts),
		testjsonl.CodexMsgJSON("assistant", "second answer", ts),
	))
	pathB = writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-09-52-"+identityThreadB+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(identityThreadB, "/work/project-b", "codex_cli_rs", ts),
		metaA,
		testjsonl.CodexMsgJSON("user", "other question", ts),
	))
	return root, pathA, pathB
}

func TestCodexForeignMetadataKeepsSessionsSeparate(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, staged := range []bool{false, true} {
			t.Run(fmt.Sprintf("reverse=%t/staged=%t", reverse, staged), func(t *testing.T) {
				root, pathA, pathB := writeCodexIdentityRollouts(t)
				database := openTestDB(t)
				cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
				if staged {
					cfg.StagedCodexParseMinBytes = 1
				}
				engine := NewEngine(t.Context(), database, cfg)
				t.Cleanup(engine.Close)
				paths := []string{pathA, pathB}
				if reverse {
					paths = []string{pathB, pathA}
				}
				for _, path := range paths {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
				}
				assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"}, messageContents(t, database, "codex:"+identityThreadA))
				assert.Equal(t, []string{"other question"}, messageContents(t, database, "codex:"+identityThreadB))

				// A later foreign header forces a full parse even after a checkpoint.
				f, err := os.OpenFile(pathB, os.O_APPEND|os.O_WRONLY, 0o600)
				require.NoError(t, err)
				_, err = f.WriteString(testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaJSON(identityThreadA, "/work/project-a", "codex_cli_rs", "2025-10-20T19:10:00Z"),
					testjsonl.CodexMsgJSON("assistant", "other answer", "2025-10-20T19:10:01Z"),
				))
				require.NoError(t, err)
				require.NoError(t, f.Close())
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{pathB}))
				assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"}, messageContents(t, database, "codex:"+identityThreadA))
				assert.Equal(t, []string{"other question", "other answer"}, messageContents(t, database, "codex:"+identityThreadB))
			})
		}
	}
}

func TestCodexForeignMetadataUpgradeRepairsMisfiledSession(t *testing.T) {
	for _, tc := range []struct {
		name                                                                             string
		originalPresent, correctedMissing, trash, twoRows, sharedPinText, compressedPath bool
		version                                                                          int
	}{
		{name: "original_present", originalPresent: true, version: 135},
		{name: "original_present_compressed_archive_path", originalPresent: true, compressedPath: true, version: 135},
		{name: "original_absent", version: 135},
		{name: "corrected_source_missing", originalPresent: true, correctedMissing: true, version: 135},
		{name: "both_missing", correctedMissing: true, version: 135},
		{name: "two_rows_one_target", originalPresent: true, correctedMissing: true, twoRows: true, version: 135},
		{name: "trash", originalPresent: true, trash: true, version: 135},
		{name: "pin_matches_live", originalPresent: true, correctedMissing: true, sharedPinText: true, version: 135},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, pathA, pathB := writeCodexIdentityRollouts(t)
			if !tc.originalPresent {
				require.NoError(t, os.Remove(pathA))
			}
			if tc.correctedMissing {
				require.NoError(t, os.Remove(pathB))
			}
			database := openTestDB(t)
			const legacyID = "codex:" + identityThreadA
			const correctedID = "codex:" + identityThreadB
			archived := []string{"other question"}
			if tc.twoRows {
				archived = append(archived, "archived answer")
			}
			if tc.sharedPinText {
				archived = append(archived, "first question")
			}
			storedPath := pathB
			if tc.compressedPath {
				storedPath += ".zst"
			}
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: legacyID, Agent: "codex", Project: "project-a", Machine: "local", FilePath: &storedPath,
				DataVersion: tc.version, MessageCount: len(archived), UserMessageCount: 1,
			}))
			var messages []db.Message
			for ordinal, content := range archived {
				messages = append(messages, db.Message{SessionID: legacyID, Ordinal: ordinal, Role: "user", Content: content})
			}
			require.NoError(t, database.InsertMessages(t.Context(), messages))
			stored, err := database.GetAllMessages(t.Context(), legacyID)
			require.NoError(t, err)
			require.Len(t, stored, len(archived))
			if tc.correctedMissing || tc.originalPresent && !tc.trash {
				_, err = database.PinMessage(t.Context(), legacyID, stored[len(stored)-1].ID, new("saved note"))
				require.NoError(t, err)
			}
			if tc.trash {
				require.NoError(t, database.SoftDeleteSession(t.Context(), legacyID))
			}
			const threadC = "33333333-3333-7333-8333-333333333333"
			if tc.twoRows {
				writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-10-00-"+threadC+".jsonl", testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaJSON(threadC, "/work/project-c", "codex_cli_rs", "2025-10-20T19:10:00Z"),
					testjsonl.CodexMsgJSON("user", "third question", "2025-10-20T19:10:00Z"),
				))
				movedB := filepath.Join(root, "archived_sessions", filepath.Base(pathB))
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{
					ID: "codex:" + threadC, Agent: "codex", Project: "project-c", Machine: "local", FilePath: &movedB,
					DataVersion: tc.version, MessageCount: 1,
				}))
				require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: "codex:" + threadC, Role: "user", Content: "shorter copy"}}))
			}
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "retained", Agent: "gemini", Project: "sample", Machine: "local", MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: "retained", Role: "user", Content: "Only stored in the archive"}}))
			reopened, _ := rebuildCodexIdentityArchive(t, database, root, tc.version)
			assert.Equal(t, []string{"Only stored in the archive"}, messageContents(t, reopened, "retained"))
			if tc.trash {
				assert.True(t, reopened.IsSessionTrashed(t.Context(), legacyID))
				assert.Equal(t, archived, messageContents(t, reopened, legacyID))
				absent, err := reopened.GetSession(t.Context(), correctedID)
				require.NoError(t, err)
				assert.Nil(t, absent)
				return
			}
			if tc.originalPresent {
				assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"}, messageContents(t, reopened, legacyID))
				assert.Equal(t, pathA, reopened.GetSessionFilePath(t.Context(), legacyID))
			} else if !tc.correctedMissing {
				stale, err := reopened.GetSession(t.Context(), legacyID)
				require.NoError(t, err)
				assert.Nil(t, stale)
			}
			if !tc.originalPresent && tc.correctedMissing {
				assert.Equal(t, archived, messageContents(t, reopened, legacyID))
				assert.Equal(t, pathB, reopened.GetSessionFilePath(t.Context(), legacyID))
				absent, err := reopened.GetSession(t.Context(), correctedID)
				require.NoError(t, err)
				assert.Nil(t, absent)
			} else {
				assert.Equal(t, archived, messageContents(t, reopened, correctedID))
				corrected, err := reopened.GetSession(t.Context(), correctedID)
				require.NoError(t, err)
				require.NotNil(t, corrected)
				assert.Equal(t, pathB, reopened.GetSessionFilePath(t.Context(), correctedID))
				assert.Nil(t, corrected.ParentSessionID)
				assert.Nil(t, corrected.ParserParentSessionID)
				assert.Empty(t, corrected.RelationshipType)
				if tc.correctedMissing || tc.originalPresent {
					pins, err := reopened.ListPinnedMessages(t.Context(), "", "")
					require.NoError(t, err)
					require.Len(t, pins, 1)
					assert.Equal(t, correctedID, pins[0].SessionID)
					assert.Equal(t, new("saved note"), pins[0].Note)
					assert.Equal(t, new(archived[len(archived)-1]), pins[0].Content)
				}
			}
			if tc.twoRows {
				assert.Equal(t, []string{"third question"}, messageContents(t, reopened, "codex:"+threadC))
			}
		})
	}
}

func TestCodexForeignMetadataUpgradePreservesPermanentDeletion(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		missing, archivedTarget bool
	}{
		{name: "present"},
		{name: "missing", missing: true},
		{name: "archived_target", missing: true, archivedTarget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, pathB := writeCodexIdentityRollouts(t)
			content, err := os.ReadFile(pathB)
			require.NoError(t, err)
			if tc.missing {
				require.NoError(t, os.Remove(pathB))
			}
			database := openTestDB(t)
			const legacyID = "codex:" + identityThreadA
			const correctedID = "codex:" + identityThreadB
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: legacyID, Agent: "codex", Project: "sample", Machine: "local", FilePath: &pathB, DataVersion: 135, MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: legacyID, Role: "user", Content: "other question"}}))
			require.NoError(t, database.DeleteSession(t.Context(), legacyID))
			if tc.archivedTarget {
				require.NoError(t, database.UpsertSession(t.Context(), db.Session{
					ID: correctedID, Agent: "codex", Project: "sample", Machine: "local", FilePath: &pathB, DataVersion: 135, MessageCount: 1,
				}))
				require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: correctedID, Role: "user", Content: "independent archived thread"}}))
			}
			reopened, engine := rebuildCodexIdentityArchive(t, database, root, 135)
			assert.True(t, reopened.IsSessionExcluded(t.Context(), legacyID))
			absent, err := reopened.GetSession(t.Context(), legacyID)
			require.NoError(t, err)
			assert.Nil(t, absent)
			if tc.archivedTarget {
				assert.False(t, reopened.IsSessionExcluded(t.Context(), correctedID))
				assert.Equal(t, []string{"independent archived thread"}, messageContents(t, reopened, correctedID))
				return
			}
			assert.True(t, reopened.IsSessionExcluded(t.Context(), correctedID))
			absent, err = reopened.GetSession(t.Context(), correctedID)
			require.NoError(t, err)
			assert.Nil(t, absent)
			movedB := filepath.Join(root, "archived_sessions", filepath.Base(pathB))
			require.NoError(t, os.MkdirAll(filepath.Dir(movedB), 0o700))
			require.NoError(t, os.WriteFile(movedB, content, 0o600))
			if !tc.missing {
				require.NoError(t, os.Remove(pathB))
			}
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{movedB}))
			absent, err = reopened.GetSession(t.Context(), correctedID)
			require.NoError(t, err)
			assert.Nil(t, absent, "moving a deleted rollout must keep it excluded")
		})
	}
}

func rebuildCodexIdentityArchive(t *testing.T, database *db.DB, root string, version int) (*db.DB, *Engine) {
	t.Helper()
	// A fresh unrelated source allows a rebuild when both affected files are gone.
	const controlID = "44444444-4444-7444-8444-444444444444"
	writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-20-00-"+controlID+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(controlID, "/work/control", "codex_cli_rs", "2025-10-20T19:20:00Z"),
		testjsonl.CodexMsgJSON("user", "control question", "2025-10-20T19:20:00Z"),
	))
	path := database.Path()
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", version))
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	reopened, err := db.OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	require.True(t, reopened.NeedsResync())
	engine := NewEngine(t.Context(), reopened, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	stats, err := engine.SyncThenRun(t.Context(), false, nil, func(full bool) error { assert.True(t, full); return nil })
	require.NoError(t, err)
	require.Zero(t, stats.Failed)
	require.False(t, stats.Aborted)
	require.True(t, stats.ArchiveRebuilt)
	assert.False(t, reopened.NeedsResync())
	return reopened, engine
}
