package sync

import (
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

func TestMissingParentForkPreservesShortenedHistoryAcrossReopen(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	const id = "codex:" + child
	const ts = "2026-09-01T10:00:00Z"
	for _, knownParent := range []bool{false, true} {
		for _, staging := range []int64{1, 1 << 30} {
			t.Run(fmt.Sprintf("known=%v/staging=%d", knownParent, staging), func(t *testing.T) {
				root := t.TempDir()
				parentPath := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+parent+".jsonl")
				childPath := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+child+".jsonl")
				parentBody := testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
					testjsonl.CodexMsgJSON("assistant", "same answer", ts),
					testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
				)
				prefix := testjsonl.JoinJSONL(
					testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
					testjsonl.CodexMsgJSON("assistant", "same answer", ts),
					testjsonl.CodexTokenCountJSON(ts, 100, 40, 0),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
					testjsonl.CodexMsgJSON("assistant", "same answer", ts),
					testjsonl.CodexTokenCountJSON(ts, 20, 5, 0),
				)
				retained := testjsonl.JoinJSONL(
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "saved-turn", ts),
					testjsonl.CodexFunctionCallJSON("exec_command", "", ts),
					testjsonl.CodexFunctionCallOutputJSON("call_test", "retained tool output", ts),
					testjsonl.CodexMsgJSON("assistant", "archived-only answer", ts),
					testjsonl.CodexTokenCountJSON(ts, 10, 3, 0),
				)
				if knownParent {
					require.NoError(t, os.WriteFile(parentPath, []byte(parentBody), 0o600))
				}
				require.NoError(t, os.WriteFile(childPath, []byte(prefix+retained), 0o600))
				database := openTestDB(t)
				cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: staging}
				engine := NewEngine(t.Context(), database, cfg)
				require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
				if knownParent {
					require.NoError(t, os.Remove(parentPath))
				}
				// A source copy can be shorter than the archived transcript.
				// The new live turn reuses the tool call ID from the absent turn.
				later := testjsonl.JoinJSONL(
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "later-turn", ts),
					testjsonl.CodexFunctionCallJSON("exec_command", "", ts),
					testjsonl.CodexFunctionCallOutputJSON("call_test", "new tool output", ts),
					testjsonl.CodexMsgJSON("assistant", "new answer", ts),
					testjsonl.CodexTokenCountJSON(ts, 20, 7, 0),
				)
				require.NoError(t, os.WriteFile(childPath, []byte(prefix+later), 0o600))
				require.NoError(t, engine.TrySyncSessionWithSubagentsContext(t.Context(), id))
				engine.Close()
				path := database.Path()
				require.NoError(t, database.Close())
				var err error
				database, err = db.Open(t.Context(), path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, database.Close()) })
				engine = NewEngine(t.Context(), database, cfg)
				t.Cleanup(engine.Close)
				rebuilt := engine.ResyncAll(t.Context(), nil)
				require.True(t, rebuilt.ArchiveRebuilt)
				assert.Equal(t, 1, rebuilt.Deferred)
				for range 2 {
					assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Deferred)
				}
				messages, err := database.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				var answers, outputs []string
				for _, message := range messages {
					if !message.HasToolUse {
						answers = append(answers, message.Content)
					}
					for _, call := range message.ToolCalls {
						outputs = append(outputs, call.ResultContent)
					}
				}
				want := []string{"same answer", "archived-only answer", "new answer"}
				if !knownParent {
					want = append([]string{"same answer"}, want...)
				}
				assert.Equal(t, want, answers)
				assert.Equal(t, []string{"retained tool output", "new tool output"}, outputs)
				usage, err := database.GetSessionUsage(t.Context(), id, true)
				require.NoError(t, err)
				require.NotNil(t, usage)
				if knownParent {
					assert.Equal(t, 15, usage.TotalOutputTokens)
				} else {
					assert.Zero(t, usage.TotalOutputTokens)
				}
				// A permanently missing parent has already received repeated live
				// updates and survived reopen/rebuild. Now exercise delayed arrival.
				require.NoError(t, os.WriteFile(parentPath, []byte(parentBody), 0o600))
				recovered := engine.SyncAll(t.Context(), nil)
				assert.Zero(t, recovered.Deferred)
				messages, err = database.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				answers = nil
				for _, message := range messages {
					if !message.HasToolUse {
						answers = append(answers, message.Content)
					}
				}
				assert.Equal(t, []string{"same answer", "archived-only answer", "new answer"}, answers)
				usage, err = database.GetSessionUsage(t.Context(), id, true)
				require.NoError(t, err)
				require.NotNil(t, usage)
				assert.Equal(t, 15, usage.TotalOutputTokens)
			})
		}
	}
}

func TestMissingParentForkPreservesLegacySourceHistory(t *testing.T) {
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const child = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	root := t.TempDir()
	path := writeOrphanCodexFork(t, root, child, parent)
	database := openTestDB(t)
	// Before data version 40, a fork could be stored under its parent's ID.
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "codex:" + parent, Agent: "codex", Project: "project", Machine: "local", FilePath: &path,
	}))
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), "codex:"+parent, []db.Message{{
		SessionID: "codex:" + parent, Role: "assistant", Content: "legacy-only history",
	}}))
	require.NoError(t, database.SetSessionDataVersion(t.Context(), "codex:"+parent, 17))
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", DisableFilesystemProjectDiscovery: true,
	})
	t.Cleanup(engine.Close)
	stats := engine.ResyncAll(t.Context(), nil)
	require.True(t, stats.ArchiveRebuilt)
	assert.Equal(t, 1, stats.Deferred)
	assert.Equal(t, []string{"legacy-only history", "available child history"}, messageContents(t, database, "codex:"+child))
}

func TestMissingParentForkWithholdsUnverifiableArchivedUsage(t *testing.T) {
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const child = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	const id = "codex:" + child
	const ts = "2026-09-01T10:00:00Z"
	root := t.TempDir()
	path := writeOrphanCodexFork(t, root, child, parent)
	database := openTestDB(t)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: id, Agent: "codex", Project: "project", Machine: "local", FilePath: &path}))
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), id, []db.Message{{
		SessionID: id, Role: "assistant", Content: "archived answer", Timestamp: ts, Model: "gpt-5",
		TokenUsage: []byte(`{"input_tokens":70,"output_tokens":30}`), OutputTokens: 30, HasOutputTokens: true,
	}}))
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", DisableFilesystemProjectDiscovery: true})
	t.Cleanup(engine.Close)
	assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Deferred)
	assert.Equal(t, []string{"archived answer", "available child history"}, messageContents(t, database, id))
	usage, err := database.GetSessionUsage(t.Context(), id, true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Zero(t, usage.TotalOutputTokens, "unverifiable old fallback usage must not be counted")
	parentPath := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+parent+".jsonl")
	require.NoError(t, os.WriteFile(parentPath, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts))), 0o600))
	// Returning source bytes prove the archived answer belongs to a child turn.
	// Its missing token record can then use the retained, uncounted metadata.
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "archive-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "archived answer", ts),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts),
		testjsonl.CodexMsgJSON("assistant", "available child history", ts),
	)), 0o600))
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Deferred)
	assert.Equal(t, []string{"archived answer", "available child history"}, messageContents(t, database, id))
	usage, err = database.GetSessionUsage(t.Context(), id, true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 30, usage.TotalOutputTokens)
}

func TestMissingParentForkKeepsPartialRecords(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	const ts = "2026-09-01T10:00:00Z"
	for _, stage := range []int64{1, 1 << 30} {
		for _, cut := range []string{"tool-output", "usage"} {
			t.Run(fmt.Sprintf("stage=%d/cut=%s", stage, cut), func(t *testing.T) {
				dir := t.TempDir()
				pp := filepath.Join(dir, "rollout-2026-09-01T10-00-00-"+parent+".jsonl")
				cp := filepath.Join(dir, "rollout-2026-09-01T11-00-00-"+child+".jsonl")
				parentBody := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts), testjsonl.CodexMsgJSON("assistant", "parent answer", ts))
				prefix := testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts), testjsonl.CodexMsgJSON("assistant", "parent answer", ts), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts), testjsonl.CodexFunctionCallJSON("exec_command", "", ts))
				output := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("call_test", "saved tool output", ts), testjsonl.CodexMsgJSON("assistant", "saved answer", ts))
				usage := testjsonl.JoinJSONL(testjsonl.CodexTokenCountJSON(ts, 20, 5, 0))
				require.NoError(t, os.WriteFile(pp, []byte(parentBody), 0o600))
				require.NoError(t, os.WriteFile(cp, []byte(prefix+output+usage), 0o600))
				database := openTestDB(t)
				engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {dir}}, Machine: "local", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: stage})
				t.Cleanup(engine.Close)
				require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
				u, err := database.GetSessionUsage(t.Context(), "codex:"+child, true)
				require.NoError(t, err)
				require.NotNil(t, u)
				require.Equal(t, 5, u.TotalOutputTokens)
				require.NoError(t, os.Remove(pp))
				shortened := prefix
				if cut == "usage" {
					shortened += output
				}
				require.NoError(t, os.WriteFile(cp, []byte(shortened), 0o600))
				require.NoError(t, engine.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+child))
				ms, err := database.GetAllMessages(t.Context(), "codex:"+child)
				require.NoError(t, err)
				require.Len(t, ms, 2)
				require.Len(t, ms[0].ToolCalls, 1)
				assert.Equal(t, "saved tool output", ms[0].ToolCalls[0].ResultContent)
				u, err = database.GetSessionUsage(t.Context(), "codex:"+child, true)
				require.NoError(t, err)
				require.NotNil(t, u)
				assert.Equal(t, 5, u.TotalOutputTokens)
			})
		}
	}
}

func TestCodexForkReconcilesLateParentPage(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	const page = "33333333-3333-4333-8333-333333333333"
	const ts = "2026-09-01T10:00:00Z"
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			dir := t.TempDir()
			pp := filepath.Join(dir, "rollout-2026-09-01T10-00-00-"+parent+".jsonl")
			cp := filepath.Join(dir, "rollout-2026-09-01T11-00-00-"+child+".jsonl")
			require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "head-turn", ts), testjsonl.CodexMsgJSON("assistant", "head answer", ts))), 0o600))
			require.NoError(t, os.WriteFile(cp, []byte(testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "head-turn", ts), testjsonl.CodexMsgJSON("assistant", "head answer", ts), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", ts), testjsonl.CodexMsgJSON("assistant", "page answer", ts), testjsonl.CodexTokenCountJSON(ts, 100, 40, 0), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", ts), testjsonl.CodexMsgJSON("assistant", "child answer", ts), testjsonl.CodexTokenCountJSON(ts, 20, 5, 0))), 0o600))
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {dir}}, Machine: "local", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: stage})
			t.Cleanup(engine.Close)
			initial := engine.SyncAll(t.Context(), nil)
			require.Zero(t, initial.Failed)
			require.Zero(t, initial.Deferred)
			require.Equal(t, []string{"page answer", "child answer"}, messageContents(t, database, "codex:"+child))
			pagePath := filepath.Join(dir, "rollout-2026-09-01T09-00-00-"+parent+"_"+page+".jsonl")
			pageBody := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/work/project", "user", ts, map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 3}}), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", ts), testjsonl.CodexMsgJSON("assistant", "page answer", ts), testjsonl.CodexTokenCountJSON(ts, 100, 40, 0))
			require.NoError(t, os.WriteFile(pagePath, []byte(pageBody), 0o600))
			require.NoError(t, engine.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+child))
			assert.Equal(t, []string{"child answer"}, messageContents(t, database, "codex:"+child))
			u, err := database.GetSessionUsage(t.Context(), "codex:"+child, true)
			require.NoError(t, err)
			require.NotNil(t, u)
			assert.Equal(t, 5, u.TotalOutputTokens)
		})
	}
}

func TestMissingParentForkRetainedCallSignals(t *testing.T) {
	const parent = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const child = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	const ts = "2026-09-01T10:00:00Z"
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			dir := t.TempDir()
			cp := filepath.Join(dir, "rollout-2026-09-01T11-00-00-"+child+".jsonl")
			meta := testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts))
			old := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "old-turn", ts), testjsonl.CodexFunctionCallJSON("exec_command", "", ts), testjsonl.CodexFunctionCallOutputJSON("call_test", "command succeeded", ts))
			fresh := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "new-turn", ts), testjsonl.CodexFunctionCallJSON("exec_command", "", ts), testjsonl.CodexFunctionCallOutputJSON("call_test", "command not found", ts))
			require.NoError(t, os.WriteFile(cp, []byte(meta+old), 0o600))
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {dir}}, Machine: "local", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: stage})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			require.NoError(t, os.WriteFile(cp, []byte(meta+fresh), 0o600))
			require.NoError(t, engine.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+child))
			ms, err := database.GetAllMessages(t.Context(), "codex:"+child)
			require.NoError(t, err)
			require.Len(t, ms, 2)
			require.Len(t, ms[0].ToolCalls, 1)
			require.Len(t, ms[1].ToolCalls, 1)
			require.Equal(t, "command succeeded", ms[0].ToolCalls[0].ResultContent)
			require.Equal(t, "command not found", ms[1].ToolCalls[0].ResultContent)
			s, err := database.GetSessionFull(t.Context(), "codex:"+child)
			require.NoError(t, err)
			require.NotNil(t, s)
			assert.Equal(t, 1, s.FinalFailureStreak)
		})
	}
}
