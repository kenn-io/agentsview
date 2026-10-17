package sync

import (
	"database/sql"
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

var (
	aMessages = []string{"first question", "first answer", "second question", "second answer"}
	bMessages = []string{"other question"}
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
	root, pathA, pathB := writeCodexIdentityRollouts(t)
	database := openTestDB(t)
	cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
	engine := NewEngine(t.Context(), database, cfg)
	t.Cleanup(engine.Close)
	paths := []string{pathA, pathB}
	for _, path := range paths {
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	}
	assert.Equal(t, aMessages, messageContents(t, database, "codex:"+identityThreadA))
	assert.Equal(t, bMessages, messageContents(t, database, "codex:"+identityThreadB))

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
	assert.Equal(t, aMessages, messageContents(t, database, "codex:"+identityThreadA))
	assert.Equal(t, []string{"other question", "other answer"}, messageContents(t, database, "codex:"+identityThreadB))
}

func TestCodexThreadIdentityUpgradePreservesTranscriptMetadata(t *testing.T) {
	const a = "codex:" + identityThreadA
	const b = "codex:" + identityThreadB
	for _, tc := range []struct {
		name         string
		absent       []string
		storedPath   string
		disposition  string
		wantA, wantB []string
		pinOwner     string
		nameOwner    string
		returnForm   string
	}{
		{name: "both_present", wantA: aMessages, wantB: bMessages, pinOwner: b, nameOwner: a},
		{name: "a_only", absent: []string{b}, wantA: aMessages, wantB: bMessages, pinOwner: b, nameOwner: a},
		{name: "b_only", absent: []string{a}, wantB: bMessages, pinOwner: b, nameOwner: b},
		{name: "neither", absent: []string{a, b}, wantA: bMessages, pinOwner: a, nameOwner: a},
		{name: "b_moved", storedPath: "moved", wantA: aMessages, wantB: bMessages, pinOwner: b, nameOwner: a},
		{name: "backslash_path", storedPath: "backslash", wantA: aMessages, wantB: bMessages, pinOwner: b, nameOwner: a},
		{name: "trashed", disposition: "trash", wantA: bMessages, pinOwner: a, nameOwner: a},
		{name: "trashed_b_moved", storedPath: "moved", disposition: "trash", wantA: bMessages, pinOwner: a, nameOwner: a},
		{name: "trashed_b_compressed", storedPath: "compressed", disposition: "trash", wantA: bMessages, pinOwner: a, nameOwner: a},
		{name: "deleted_b_present", disposition: "delete"},
		{name: "deleted_b_missing", absent: []string{b}, disposition: "delete", returnForm: "plain"},
		{name: "deleted_b_missing_moved", absent: []string{b}, disposition: "delete", returnForm: "moved"},
		{name: "deleted_b_missing_compressed", absent: []string{b}, disposition: "delete", storedPath: "backslash", returnForm: "compressed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, pathA, pathB := writeCodexIdentityRollouts(t)
			returningB, err := os.ReadFile(pathB)
			require.NoError(t, err)
			// Keep discovery nonempty even when both affected files are gone.
			writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-10-00-33333333-3333-7333-8333-333333333333.jsonl", testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON("33333333-3333-7333-8333-333333333333", "/work/other", "codex_cli_rs", "2025-10-20T19:10:00Z"),
				testjsonl.CodexMsgJSON("user", "unrelated", "2025-10-20T19:10:01Z"),
			))
			storedPath := pathB
			dirs := []string{root}
			switch tc.storedPath {
			case "compressed":
				info, err := os.Stat(pathB)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(pathB+".zst", zstdCompressString(t, string(returningB)), 0o600))
				require.NoError(t, os.Chtimes(pathB+".zst", info.ModTime(), info.ModTime()))
				require.NoError(t, os.Remove(pathB))
			case "moved":
				moved := filepath.Join(root, "archived_sessions", filepath.Base(pathB))
				require.NoError(t, os.MkdirAll(filepath.Dir(moved), 0o700))
				require.NoError(t, os.Rename(pathB, moved))
				dirs = append(dirs, filepath.Dir(moved))
			case "backslash":
				storedPath = `C:\sessions\` + filepath.Base(pathB)
			}
			for _, id := range tc.absent {
				path := pathA
				if id == b {
					path = pathB
				}
				require.NoError(t, os.Remove(path))
			}

			database := openTestDB(t)
			require.NoError(t, database.UpsertSession(ctx, db.Session{
				ID: a, Agent: "codex", Project: "project-b", Machine: "local",
				FilePath: &storedPath, MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(ctx, []db.Message{{
				SessionID: a, Role: "user", Content: "other question",
			}}))
			messages, err := database.GetAllMessages(ctx, a)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			_, err = database.PinMessage(ctx, a, messages[0].ID, new("saved note"))
			require.NoError(t, err)
			require.NoError(t, database.RenameSession(ctx, a, new("saved name")))
			_, err = database.StarSession(ctx, a)
			require.NoError(t, err)
			window, err := database.BuildRecallEvidenceWindow(ctx, a, 0, 0)
			require.NoError(t, err)
			selection, err := window.BindSelection(db.RecallEvidenceSelection{MessageStartOrdinal: 0, MessageEndOrdinal: 0})
			require.NoError(t, err)
			_, err = database.InsertRecallEntry(ctx, db.RecallEntry{
				ID: "saved-fact", Type: "fact", Scope: "project", Status: "accepted",
				Title: "saved fact", Body: "saved evidence", SourceSessionID: a, Agent: "codex",
				ProvenanceOK: true, Evidence: []db.RecallEvidence{{
					SessionID: a, MessageStartOrdinal: 0, MessageEndOrdinal: 0, ContentDigest: selection.ContentDigest,
				}},
			})
			require.NoError(t, err)
			before, err := database.ExportConversationChanges(ctx, db.ConversationExportOptions{})
			require.NoError(t, err)
			require.NotEmpty(t, before.Changes)
			switch tc.disposition {
			case "trash":
				require.NoError(t, database.SoftDeleteSession(ctx, a))
			case "delete":
				require.NoError(t, database.DeleteSession(ctx, a))
			}
			path := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, `UPDATE sessions SET data_version=135`)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, "PRAGMA user_version=135")
			require.NoError(t, err)
			require.NoError(t, raw.Close())

			reopened, err := db.OpenIsolated(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			require.True(t, reopened.NeedsResync())
			engine := NewEngine(ctx, reopened, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: dirs}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			stats, err := engine.SyncThenRun(ctx, false, nil, func(bool) error { return nil })
			require.NoError(t, err)
			require.Zero(t, stats.Failed)
			require.False(t, stats.Aborted)
			if tc.storedPath == "moved" && tc.disposition != "trash" {
				assert.Equal(t, filepath.Join(root, "archived_sessions", filepath.Base(pathB)), reopened.GetSessionFilePath(ctx, b))
			}
			for _, want := range []struct {
				id       string
				contents []string
			}{{a, tc.wantA}, {b, tc.wantB}} {
				session, err := reopened.GetSessionFull(ctx, want.id)
				require.NoError(t, err)
				if want.contents == nil {
					assert.Nil(t, session)
					continue
				}
				require.NotNil(t, session)
				assert.Equal(t, want.contents, messageContents(t, reopened, want.id))
				if want.id != tc.nameOwner {
					assert.Nil(t, session.DisplayName)
				}
			}
			if tc.disposition == "delete" {
				assert.True(t, reopened.IsSessionExcluded(ctx, a))
				assert.False(t, reopened.IsSessionExcluded(ctx, b), "the rollout guard preserves the saved exclusion on A")
				if tc.returnForm != "" {
					returnedPath := pathB
					if tc.returnForm != "plain" {
						returnedPath = filepath.Join(root, "archived_sessions", filepath.Base(pathB))
						require.NoError(t, os.MkdirAll(filepath.Dir(returnedPath), 0o700))
					}
					if tc.returnForm == "compressed" {
						returnedPath += ".zst"
						returningB = zstdCompressString(t, string(returningB))
					}
					require.NoError(t, os.WriteFile(returnedPath, returningB, 0o600))
					for range 2 {
						require.NoError(t, engine.SyncPathsContext(ctx, []string{returnedPath}))
						session, err := reopened.GetSessionFull(ctx, b)
						require.NoError(t, err)
						assert.Nil(t, session, "a returning permanently deleted rollout stays hidden")
						assert.True(t, reopened.IsSessionExcluded(ctx, a))
					}
					stats, err := engine.SyncThenRun(ctx, true, nil, func(bool) error { return nil })
					require.NoError(t, err)
					require.False(t, stats.Aborted)
					session, err := reopened.GetSessionFull(ctx, b)
					require.NoError(t, err)
					assert.Nil(t, session, "a later rebuild also preserves the deletion")
					assert.True(t, reopened.IsSessionExcluded(ctx, a))
				}
				return
			}
			assert.Equal(t, tc.disposition == "trash", reopened.IsSessionTrashed(ctx, a))
			pins, err := reopened.ListPinnedMessages(ctx, tc.pinOwner, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, tc.pinOwner, pins[0].SessionID)
			assert.Equal(t, new("saved note"), pins[0].Note)
			owner, err := reopened.GetSessionFull(ctx, tc.nameOwner)
			require.NoError(t, err)
			require.NotNil(t, owner)
			assert.Equal(t, new("saved name"), owner.DisplayName)
			stars, err := reopened.ListStarredSessionIDs(ctx)
			require.NoError(t, err)
			assert.Equal(t, []string{tc.nameOwner}, stars)
			recall, err := reopened.GetRecallEntry(ctx, "saved-fact")
			require.NoError(t, err)
			require.NotNil(t, recall)
			assert.Equal(t, tc.pinOwner, recall.SourceSessionID)
			require.Len(t, recall.Evidence, 1)
			assert.Equal(t, tc.pinOwner, recall.Evidence[0].SessionID)
			assert.Equal(t, selection.ContentDigest, recall.Evidence[0].ContentDigest)
			assert.True(t, recall.ProvenanceOK)
			if tc.disposition == "trash" {
				return
			}
			after, err := reopened.ExportConversationChanges(ctx, db.ConversationExportOptions{})
			require.NoError(t, err)
			byID := make(map[string]db.ConversationChange)
			for _, change := range after.Changes {
				byID[change.MessageID] = change
			}
			for _, old := range before.Changes {
				if old.MessageID == "" {
					continue
				}
				change, found := byID[old.MessageID]
				require.True(t, found, "exported message ID must survive")
				assert.Equal(t, tc.pinOwner, change.SessionID)
			}
			if tc.name == "neither" {
				require.NoError(t, os.WriteFile(pathB, returningB, 0o600))
				for range 2 {
					require.NoError(t, engine.SyncPathsContext(ctx, []string{pathB}))
					assert.Equal(t, bMessages, messageContents(t, reopened, a))
					assert.Equal(t, bMessages, messageContents(t, reopened, b))
					pins, err := reopened.ListPinnedMessages(ctx, a, "")
					require.NoError(t, err)
					require.Len(t, pins, 1)
					assert.Equal(t, new("saved note"), pins[0].Note)
					assert.False(t, reopened.IsSessionExcluded(ctx, a))
				}
				stats, err := engine.SyncThenRun(ctx, true, nil, func(bool) error { return nil })
				require.NoError(t, err)
				require.Zero(t, stats.Failed)
				require.False(t, stats.Aborted)
				assert.Equal(t, bMessages, messageContents(t, reopened, b))
				pins, err := reopened.ListPinnedMessages(ctx, "", "")
				require.NoError(t, err)
				assert.Len(t, pins, 1, "a later rebuild must preserve the retained transcript's pin")
				recall, err := reopened.GetRecallEntry(ctx, "saved-fact")
				require.NoError(t, err)
				if assert.NotNil(t, recall, "a later rebuild must preserve accepted Recall") {
					assert.Equal(t, b, recall.SourceSessionID)
					require.Len(t, recall.Evidence, 1)
					assert.Equal(t, b, recall.Evidence[0].SessionID)
				}
			}
		})
	}
}
