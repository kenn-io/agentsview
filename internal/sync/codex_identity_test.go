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
		name            string
		originalPresent bool
		renamedFile     bool
		version         int
	}{
		{"original_present", true, false, 134},
		{"original_absent", false, false, 134},
		{"noncanonical_filename", true, true, 134},
		{"version_135", false, false, 135},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, pathA, pathB := writeCodexIdentityRollouts(t)
			if tc.renamedFile {
				restored := filepath.Join(filepath.Dir(pathB), "rollout-restored.jsonl")
				require.NoError(t, os.Rename(pathB, restored))
				pathB = restored
			}
			if !tc.originalPresent {
				require.NoError(t, os.Remove(pathA))
			}
			database := openTestDB(t)
			// Earlier versions could store B's transcript as A and lose A's full history.
			const legacyID = "codex:" + identityThreadA
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: legacyID, Agent: "codex", Project: "project-a", Machine: "local", FilePath: &pathB,
				DataVersion: tc.version, MessageCount: 1, UserMessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: legacyID, Role: "user", Content: "other question"}}))
			name, note := "Saved conversation", "Keep this question"
			require.NoError(t, database.RenameSession(t.Context(), legacyID, &name))
			_, err := database.StarSession(t.Context(), legacyID)
			require.NoError(t, err)
			messages, err := database.GetAllMessages(t.Context(), legacyID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			_, err = database.PinMessage(t.Context(), legacyID, messages[0].ID, &note)
			require.NoError(t, err)
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "retained", Agent: "gemini", Project: "sample", Machine: "local", MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{SessionID: "retained", Role: "user", Content: "Only stored in the archive"}}))
			dbPath := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, raw.Close()) })
			_, err = raw.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", tc.version))
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			reopened, err := db.OpenIsolated(t.Context(), dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
			require.True(t, reopened.NeedsResync())
			engine := NewEngine(t.Context(), reopened, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			stats, err := engine.SyncThenRun(t.Context(), false, nil, func(full bool) error {
				assert.True(t, full)
				return nil
			})
			require.NoError(t, err)
			require.Zero(t, stats.Failed)
			require.False(t, stats.Aborted)
			require.True(t, stats.ArchiveRebuilt)
			assert.False(t, reopened.NeedsResync())
			if tc.originalPresent {
				assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"}, messageContents(t, reopened, legacyID))
				assert.Equal(t, pathA, reopened.GetSessionFilePath(t.Context(), legacyID))
			} else {
				stale, err := reopened.GetSession(t.Context(), legacyID)
				require.NoError(t, err)
				assert.Nil(t, stale, "B's misfiled transcript must not also survive under A")
			}
			assert.Equal(t, []string{"other question"}, messageContents(t, reopened, "codex:"+identityThreadB))
			assert.Equal(t, []string{"Only stored in the archive"}, messageContents(t, reopened, "retained"))
			assert.Equal(t, pathB, reopened.GetSessionFilePath(t.Context(), "codex:"+identityThreadB))
			pins, err := reopened.ListPinnedMessages(t.Context(), "", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, "codex:"+identityThreadB, pins[0].SessionID)
			assert.Equal(t, &note, pins[0].Note)
			corrected, err := reopened.GetAllMessages(t.Context(), "codex:"+identityThreadB)
			require.NoError(t, err)
			require.Len(t, corrected, 1)
			assert.Equal(t, corrected[0].ID, pins[0].MessageID)
			curatedID := "codex:" + identityThreadB
			if tc.originalPresent {
				curatedID = legacyID
			}
			curated, err := reopened.GetSession(t.Context(), curatedID)
			require.NoError(t, err)
			require.NotNil(t, curated)
			assert.Equal(t, &name, curated.DisplayName)
			stars, err := reopened.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []string{curatedID}, stars)
		})
	}
}

func TestCodexForeignMetadataUpgradePreservesPermanentDeletion(t *testing.T) {
	for _, tc := range []struct {
		missing bool
		version int
	}{
		{false, 134},
		{true, 134},
		{false, 135},
		{true, 135},
	} {
		t.Run(fmt.Sprintf("version=%d/corrected_source_missing=%t", tc.version, tc.missing), func(t *testing.T) {
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
				ID: legacyID, Agent: "codex", Project: "sample", Machine: "local", FilePath: &pathB,
				DataVersion: tc.version, MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
				{SessionID: legacyID, Role: "user", Content: "other question"},
			}))
			require.NoError(t, database.DeleteSession(t.Context(), legacyID))
			path := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, raw.Close()) })
			_, err = raw.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version=%d", tc.version))
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			reopened, err := db.OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
			engine := NewEngine(t.Context(), reopened, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			stats, err := engine.SyncThenRun(t.Context(), false, nil, func(bool) error { return nil })
			require.NoError(t, err)
			require.Zero(t, stats.Failed)
			require.False(t, stats.Aborted)
			require.True(t, stats.ArchiveRebuilt)
			assert.True(t, reopened.IsSessionExcluded(t.Context(), legacyID), "the original exclusion remains")
			assert.True(t, reopened.IsSessionExcluded(t.Context(), correctedID), "the corrected identity stays deleted")
			deleted, err := reopened.GetSession(t.Context(), correctedID)
			require.NoError(t, err)
			assert.Nil(t, deleted)
			if tc.missing {
				require.NoError(t, os.WriteFile(pathB, content, 0o600))
			}
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{pathB}))
			deleted, err = reopened.GetSession(t.Context(), correctedID)
			require.NoError(t, err)
			assert.Nil(t, deleted, "a later sync must not resurrect the deleted transcript")
		})
	}
}
