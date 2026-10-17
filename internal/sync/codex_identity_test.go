package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	aMessages = []string{"first question", "first answer", "second question", "second answer", "third question", "third answer", "fourth question", "fourth answer", "fifth question", "fifth answer"}
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
		testjsonl.CodexMsgJSON("user", "third question", ts),
		testjsonl.CodexMsgJSON("assistant", "third answer", ts),
		testjsonl.CodexMsgJSON("user", "fourth question", ts),
		testjsonl.CodexMsgJSON("assistant", "fourth answer", ts),
		testjsonl.CodexMsgJSON("user", "fifth question", ts),
		testjsonl.CodexMsgJSON("assistant", "fifth answer", ts),
	))
	pathB = writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-09-52-"+identityThreadB+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(identityThreadB, "/work/project-b", "codex_cli_rs", ts),
		metaA,
		testjsonl.CodexMsgJSON("user", "other question", ts),
	))
	return root, pathA, pathB
}

func TestCodexForeignMetadataKeepsSessionsSeparate(t *testing.T) {
	for _, bFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("b_first_%t", bFirst), func(t *testing.T) {
			root, pathA, pathB := writeCodexIdentityRollouts(t)
			database := openTestDB(t)
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			paths := []string{pathA, pathB}
			if bFirst {
				paths = []string{pathB, pathA}
			}
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
		})
	}
}

func TestCodexThreadIdentityUpgradePreservesTranscriptMetadata(t *testing.T) {
	const (
		a       = "codex:" + identityThreadA
		b       = "codex:" + identityThreadB
		threadC = "33333333-3333-7333-8333-333333333333"
		c       = "codex:" + threadC
		ts      = "2025-10-20T19:00:00Z"
	)
	for _, version := range []int{135, 136} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			for _, name := range []string{"both_present", "b_missing", "b_moved", "trashed", "chain"} {
				t.Run(name, func(t *testing.T) {
					ctx := t.Context()
					root, _, pathB := writeCodexIdentityRollouts(t)
					storedPath := pathB
					dirs := []string{root}
					var pathC string
					switch name {
					case "b_missing":
						require.NoError(t, os.Remove(pathB))
					case "b_moved":
						pathB = filepath.Join(root, "archived_sessions", filepath.Base(pathB))
						require.NoError(t, os.MkdirAll(filepath.Dir(pathB), 0o700))
						require.NoError(t, os.Rename(storedPath, pathB))
						dirs = append(dirs, filepath.Dir(pathB))
					case "chain":
						pathC = writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-10-00-"+threadC+".jsonl", testjsonl.JoinJSONL(
							testjsonl.CodexSessionMetaJSON(threadC, "/work/project-c", "codex_cli_rs", ts),
							testjsonl.CodexSessionMetaJSON(identityThreadB, "/work/project-b", "codex_cli_rs", ts),
							testjsonl.CodexMsgJSON("user", "third thread", ts),
						))
					}
					database := openTestDB(t)
					require.NoError(t, database.UpsertSession(ctx, db.Session{
						ID: a, Agent: "codex", Project: "project-b", Machine: "local",
						FilePath: &storedPath, MessageCount: 1,
					}))
					require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: a, Role: "user", Content: "other question", Timestamp: ts}}))
					if name == "chain" {
						require.NoError(t, database.UpsertSession(ctx, db.Session{
							ID: b, Agent: "codex", Project: "project-c", Machine: "local",
							FilePath: &pathC, MessageCount: 1,
						}))
						require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: b, Role: "user", Content: "third thread", Timestamp: ts}}))
						require.NoError(t, database.RenameSession(ctx, b, new("second saved name")))
						messages, err := database.GetAllMessages(ctx, b)
						require.NoError(t, err)
						require.Len(t, messages, 1)
						_, err = database.PinMessage(ctx, b, messages[0].ID, new("second saved note"))
						require.NoError(t, err)
					}
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
					var exportedID string
					for _, change := range before.Changes {
						if change.SessionID == a && change.MessageID != "" {
							exportedID = change.MessageID
						}
					}
					require.NotEmpty(t, exportedID)
					if name == "trashed" {
						require.NoError(t, database.SoftDeleteSession(ctx, a))
					}
					path := database.Path()
					require.NoError(t, database.Close())
					raw, err := sql.Open("sqlite3", path)
					require.NoError(t, err)
					_, err = raw.ExecContext(ctx, `UPDATE sessions SET data_version=?`, version)
					require.NoError(t, err)
					_, err = raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", version))
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
					wantA := aMessages
					if name == "trashed" {
						wantA = bMessages
					}
					assert.Equal(t, wantA, messageContents(t, reopened, a))
					if name == "b_missing" || name == "trashed" {
						session, err := reopened.GetSessionFull(ctx, b)
						require.NoError(t, err)
						assert.Nil(t, session)
					} else {
						assert.Equal(t, bMessages, messageContents(t, reopened, b))
					}
					if name == "chain" {
						assert.Equal(t, []string{"third thread"}, messageContents(t, reopened, c))
						second, err := reopened.GetSessionFull(ctx, b)
						require.NoError(t, err)
						require.NotNil(t, second)
						assert.Equal(t, new("second saved name"), second.DisplayName)
						pins, err := reopened.ListPinnedMessages(ctx, "", "")
						require.NoError(t, err)
						assert.Empty(t, pins, "neither row in a chain moves its pins to another thread")
					}
					owner, err := reopened.GetSessionFull(ctx, a)
					require.NoError(t, err)
					require.NotNil(t, owner)
					assert.Equal(t, new("saved name"), owner.DisplayName)
					stars, err := reopened.ListStarredSessionIDs(ctx)
					require.NoError(t, err)
					assert.Equal(t, []string{a}, stars)
					assert.Equal(t, name == "trashed", reopened.IsSessionTrashed(ctx, a))
					pinOwner := a
					if name == "both_present" {
						pinOwner = b
					}
					pins, err := reopened.ListPinnedMessages(ctx, pinOwner, "")
					require.NoError(t, err)
					if name == "both_present" || name == "trashed" {
						require.Len(t, pins, 1)
						assert.Equal(t, pinOwner, pins[0].SessionID)
						assert.Equal(t, new("saved note"), pins[0].Note)
					} else {
						assert.Empty(t, pins, "the ordinary rebuild replaces the old transcript")
					}
					recall, err := reopened.GetRecallEntry(ctx, "saved-fact")
					require.NoError(t, err)
					require.NotNil(t, recall)
					assert.Equal(t, pinOwner, recall.SourceSessionID)
					require.Len(t, recall.Evidence, 1)
					assert.Equal(t, pinOwner, recall.Evidence[0].SessionID)
					if name == "both_present" {
						after, err := reopened.ExportConversationChanges(ctx, db.ConversationExportOptions{})
						require.NoError(t, err)
						var final db.ConversationChange
						for _, change := range after.Changes {
							if change.MessageID == exportedID {
								final = change
							}
						}
						assert.Equal(t, b, final.SessionID, "exported message identity follows the corrected transcript")
						assert.False(t, final.Deleted)
					}
				})
			}
		})
	}
}
