package sync

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	forkReviewParent = "11111111-1111-4111-8111-111111111111"
	forkReviewChild  = "22222222-2222-4222-8222-222222222222"
	forkReviewTime   = "2026-09-01T10:00:00Z"
)

func forkReviewEngine(t *testing.T, stage int64, agent parser.AgentType) (*Engine, *db.DB, string, string, string) {
	t.Helper()
	root := t.TempDir()
	cp := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+forkReviewChild+".jsonl")
	pp := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+forkReviewParent+".jsonl")
	meta := testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(forkReviewChild, forkReviewParent, "/work/project", "user", forkReviewTime))
	d := openTestDB(t)
	e := NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: stage})
	t.Cleanup(e.Close)
	return e, d, cp, pp, meta
}

func TestCodexForkReviewContentIdentity(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			turn := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime))
			require.NoError(t, os.WriteFile(cp, []byte(meta+turn+testjsonl.JoinJSONL(testjsonl.CodexMsgJSON("assistant", "archived answer", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0))), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			// A shorter snapshot has different content at the same source position.
			require.NoError(t, os.WriteFile(cp, []byte(meta+turn+testjsonl.JoinJSONL(testjsonl.CodexMsgJSON("assistant", "replacement answer", forkReviewTime))), 0o600))
			for range 2 {
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
			}
			assert.Equal(t, []string{"archived answer", "replacement answer"}, messageContents(t, d, "codex:"+forkReviewChild))
			require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
			ms, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, ms, 2)
			assert.Equal(t, 5, ms[0].OutputTokens)
			assert.Zero(t, ms[1].OutputTokens, "saved usage belongs only to the archived answer")
		})
	}
}

func TestCodexForkReviewSharedTraeXOrigin(t *testing.T) {
	e, d, cp, _, meta := forkReviewEngine(t, 1, parser.AgentTraeX)
	body := meta + testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime),
		`{"timestamp":"2026-09-01T10:00:00Z","type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Question"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"First"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Second"}]}]}}`,
		`{"type":"token_usage_record","payload":{"response_id":"resp_1","usage":{"input_tokens":20,"output_tokens":5}}}`,
	)
	require.NoError(t, os.WriteFile(cp, []byte(body), 0o600))
	require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
	for range 3 {
		require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "traex:"+forkReviewChild))
		assert.Equal(t, []string{"Question", "First", "Second"}, messageContents(t, d, "traex:"+forkReviewChild))
	}
}

func TestCodexForkReviewNormalizedIdentity(t *testing.T) {
	e, d, cp, _, meta := forkReviewEngine(t, 1, parser.AgentCodex)
	body := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "answer\x00text", forkReviewTime))
	require.NoError(t, os.WriteFile(cp, []byte(body), 0o600))
	for range 3 {
		require.Zero(t, e.SyncAll(t.Context(), nil).Failed)
	}
	assert.Equal(t, []string{"answertext"}, messageContents(t, d, "codex:"+forkReviewChild))
}

func TestCodexForkReviewRecoveryKeepsBorrowedMetadata(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			prefix := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime))
			require.NoError(t, os.WriteFile(cp, []byte(prefix+testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("call_test", "saved output", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0))), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			require.NoError(t, os.WriteFile(cp, []byte(prefix), 0o600))
			require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
			for range 3 {
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				ms, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
				require.NoError(t, err)
				require.Len(t, ms, 1)
				require.Len(t, ms[0].ToolCalls, 1)
				assert.Equal(t, "saved output", ms[0].ToolCalls[0].ResultContent)
				assert.Equal(t, 5, ms[0].OutputTokens)
			}
		})
	}
}

func TestCodexForkReviewMergesResultEvents(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, cp, _, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			prefix := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime))
			for _, output := range []string{"old output", "fresh output", "fresh output", "newest output"} {
				require.NoError(t, os.WriteFile(cp, []byte(prefix+testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("call_test", output, forkReviewTime))), 0o600))
				require.Zero(t, e.SyncAll(t.Context(), nil).Failed)
			}
			ms, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, ms, 1)
			require.Len(t, ms[0].ToolCalls, 1)
			events := ms[0].ToolCalls[0].ResultEvents
			require.Len(t, events, 3)
			assert.Equal(t, "old output", events[0].Content)
			assert.Equal(t, "fresh output", events[1].Content)
			assert.Equal(t, "newest output", events[2].Content)
			assert.Equal(t, "newest output", ms[0].ToolCalls[0].ResultContent)
		})
	}
}

func TestCodexForkReviewRetainedActivityBounds(t *testing.T) {
	e, d, cp, _, meta := forkReviewEngine(t, 1, parser.AgentCodex)
	const later = "2026-09-01T11:00:00Z"
	require.NoError(t, os.WriteFile(cp, []byte(meta+testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", later), testjsonl.CodexMsgJSON("user", "saved prompt", later))), 0o600))
	require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
	require.NoError(t, os.WriteFile(cp, []byte(meta), 0o600))
	require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
	s, err := d.GetSessionFull(t.Context(), "codex:"+forkReviewChild)
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, new(later), s.EndedAt)
	assert.Equal(t, new("saved prompt"), s.FirstMessage)
}

func TestCodexForkReviewHealthySkipsArchivedPayload(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		for _, ordinalUsage := range []bool{false, true} {
			t.Run(strconv.FormatInt(stage, 10)+"/ordinalUsage="+strconv.FormatBool(ordinalUsage), func(t *testing.T) {
				e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
				require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
				require.NoError(t, os.WriteFile(cp, []byte(meta+testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime), testjsonl.CodexFunctionCallOutputJSON("call_test", strings.Repeat("x", 32<<10), forkReviewTime))), 0o600))
				require.Zero(t, e.SyncAll(t.Context(), nil).Failed)
				id := "codex:" + forkReviewChild
				if ordinalUsage {
					require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), id, []db.UsageEvent{{SessionID: id, MessageOrdinal: new(0), Source: "turn", Model: "gpt-5", OutputTokens: 5, OccurredAt: forkReviewTime, DedupKey: "historical"}}))
				}
				before := d.MessagesLoadCount()
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
				assert.Equal(t, before, d.MessagesLoadCount(), "authoritative full parses must not hydrate archived outputs")
				if ordinalUsage {
					events, err := d.GetUsageEvents(t.Context(), id)
					require.NoError(t, err)
					require.Len(t, events, 1)
					assert.Equal(t, new(0), events[0].MessageOrdinal)
				}
			})
		}
	}
}

func TestCodexForkReviewBlockedRetainedResults(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, cp, _, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			call := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime))
			require.NoError(t, os.WriteFile(cp, []byte(meta+call+testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("call_test", "saved output", forkReviewTime))), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			e.blockedResultCategories = map[string]bool{"Bash": true}
			for _, remaining := range []string{call, ""} {
				require.NoError(t, os.WriteFile(cp, []byte(meta+remaining), 0o600))
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				ms, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
				require.NoError(t, err)
				require.Len(t, ms, 1)
				require.Len(t, ms[0].ToolCalls, 1)
				tc := ms[0].ToolCalls[0]
				assert.Empty(t, tc.ResultContent)
				assert.Equal(t, len("saved output"), tc.ResultContentLength)
				require.Len(t, tc.ResultEvents, 1)
				assert.Empty(t, tc.ResultEvents[0].Content)
				assert.Equal(t, len("saved output"), tc.ResultEvents[0].ContentLength)
			}
			require.NoError(t, os.WriteFile(cp, []byte(meta+call+testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("call_test", "new", forkReviewTime))), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
			ms, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, ms, 1)
			require.Len(t, ms[0].ToolCalls, 1)
			assert.Empty(t, ms[0].ToolCalls[0].ResultContent)
			assert.Equal(t, len("new"), ms[0].ToolCalls[0].ResultContentLength)
			require.Len(t, ms[0].ToolCalls[0].ResultEvents, 2)
			assert.Empty(t, ms[0].ToolCalls[0].ResultEvents[1].Content)
			assert.Equal(t, len("new"), ms[0].ToolCalls[0].ResultEvents[1].ContentLength)
		})
	}
}

func TestCodexForkReviewTurnWithoutIDAndPreview(t *testing.T) {
	e, d, cp, pp, meta := forkReviewEngine(t, 1, parser.AgentCodex)
	parentBody := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime), testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime), testjsonl.CodexMsgJSON("user", "parent prompt", forkReviewTime))
	require.NoError(t, os.WriteFile(pp, []byte(parentBody), 0o600))
	childBody := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime), testjsonl.CodexMsgJSON("user", "parent prompt", forkReviewTime), `{"type":"turn_context","payload":{"model":"gpt-5"}}`, testjsonl.CodexMsgJSON("user", "child prompt", forkReviewTime))
	require.NoError(t, os.WriteFile(cp, []byte(childBody), 0o600))
	require.Zero(t, e.SyncAll(t.Context(), nil).Failed)
	assert.Equal(t, []string{"child prompt"}, messageContents(t, d, "codex:"+forkReviewChild))
	require.NoError(t, os.Remove(pp))
	require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
	assert.Equal(t, []string{"child prompt"}, messageContents(t, d, "codex:"+forkReviewChild))
	s, err := d.GetSessionFull(t.Context(), "codex:"+forkReviewChild)
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, new("child prompt"), s.FirstMessage)
}

func TestCodexForkReviewLegacyUsage(t *testing.T) {
	e, d, cp, _, meta := forkReviewEngine(t, 1, parser.AgentCodex)
	oldID := "codex:" + forkReviewParent
	require.NoError(t, os.WriteFile(cp, []byte(meta+testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "live answer", forkReviewTime))), 0o600))
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: oldID, Agent: "codex", Project: "project", Machine: "local", FilePath: &cp}))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), oldID, []db.Message{{SessionID: oldID, Role: "assistant", Content: "archived answer"}}))
	require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), oldID, []db.UsageEvent{{SessionID: oldID, MessageOrdinal: new(0), Source: "turn", Model: "gpt-5", InputTokens: 20, OutputTokens: 5, OccurredAt: forkReviewTime}}))
	require.NoError(t, d.SetSessionDataVersion(t.Context(), oldID, 17))
	stats := e.ResyncAll(t.Context(), nil)
	require.True(t, stats.ArchiveRebuilt)
	assert.Zero(t, stats.Failed)
	assert.Contains(t, stats.Warnings, "resync: 1 sessions need retry")
	events, err := d.GetUsageEvents(t.Context(), "codex:"+forkReviewChild)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "codex:"+forkReviewChild, events[0].SessionID)
	assert.Equal(t, new(0), events[0].MessageOrdinal)
	u, err := d.GetSessionUsage(t.Context(), "codex:"+forkReviewChild, true)
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, 5, u.TotalOutputTokens)
}

func TestCodexForkReviewUsageOrdinalsAfterReplayRemoval(t *testing.T) {
	e, d, cp, pp, meta := forkReviewEngine(t, 1, parser.AgentCodex)
	id := "codex:" + forkReviewChild
	replay := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "parent answer", forkReviewTime))
	body := meta + replay + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "child answer", forkReviewTime))
	require.NoError(t, os.WriteFile(cp, []byte(body), 0o600))
	require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
	require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), id, []db.UsageEvent{
		{SessionID: id, MessageOrdinal: new(0), Source: "turn", Model: "gpt-5", OutputTokens: 40},
		{SessionID: id, MessageOrdinal: new(1), Source: "turn", Model: "gpt-5", OutputTokens: 5},
	}))
	require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))+replay), 0o600))
	for range 3 {
		require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
		events, err := d.GetUsageEvents(t.Context(), id)
		require.NoError(t, err)
		require.Len(t, events, 1)
		assert.Equal(t, new(0), events[0].MessageOrdinal)
		assert.Equal(t, 5, events[0].OutputTokens)
		u, err := d.GetSessionUsage(t.Context(), id, true)
		require.NoError(t, err)
		require.NotNil(t, u)
		assert.Equal(t, 5, u.TotalOutputTokens)
	}
}

func TestCodexForkReviewCopiedProjection(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			e, d, cp, pp, meta := forkReviewEngine(t, 1, parser.AgentCodex)
			body := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallArgsJSON("exec_command", `{"cmd":"echo fixture"}`, forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0))
			require.NoError(t, os.WriteFile(cp, []byte(body), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			require.NoError(t, os.Remove(cp))
			d.SetArchiveContent(policy)
			require.True(t, e.ResyncAll(t.Context(), nil).ArchiveRebuilt)
			require.NoError(t, os.WriteFile(cp, []byte(meta), 0o600))
			require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
			u, err := d.GetSessionUsage(t.Context(), "codex:"+forkReviewChild, true)
			require.NoError(t, err)
			require.NotNil(t, u)
			assert.Equal(t, 5, u.TotalOutputTokens, "copied provenance must still restore the saved child usage")
		})
	}
}

func TestCodexForkReviewStagedFindingsOnlyPublishedCalls(t *testing.T) {
	s, err := newCodexStagingSink(t.Context(), t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{ToolUseID: "discarded", ToolName: "exec_command", Category: "Bash"}}})
	s.AppendToolResultEvent(t.Context(), "discarded", nil, parser.ParsedToolResultEvent{ToolUseID: "discarded", Source: "function_call_output", Content: "AKIA7QHWN2DKR4FYPLJM"})
	require.NoError(t, s.Err())
	assert.Empty(t, s.Findings("child", map[string]db.StagedToolCallPosition{}))
}

func TestCodexForkReviewRetainedTranscriptLength(t *testing.T) {
	for _, arguments := range []string{"", `{"cmd":"echo fixture"}`} {
		t.Run(arguments, func(t *testing.T) {
			e, d, cp, _, meta := forkReviewEngine(t, 1<<30, parser.AgentCodex)
			d.SetArchiveContent(config.ArchiveContentTranscripts)
			prefix := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallArgsJSON("exec_command", arguments, forkReviewTime))
			require.NoError(t, os.WriteFile(cp, []byte(prefix+testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("call_test", "saved output", forkReviewTime))), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			first, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, first, 1)
			require.Len(t, first[0].ToolCalls, 1)
			require.Equal(t, 12, first[0].ToolCalls[0].ResultContentLength)
			require.NoError(t, os.WriteFile(cp, []byte(prefix), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			ms, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, ms, 1)
			require.Len(t, ms[0].ToolCalls, 1)
			assert.Empty(t, ms[0].ToolCalls[0].ResultContent)
			assert.Equal(t, 12, ms[0].ToolCalls[0].ResultContentLength)
			require.Len(t, ms[0].ToolCalls[0].ResultEvents, 1)
			assert.Empty(t, ms[0].ToolCalls[0].ResultEvents[0].Content)
			assert.Equal(t, 12, ms[0].ToolCalls[0].ResultEvents[0].ContentLength)
		})
	}
}

func TestCodexForkReviewSavedParentPage(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			pagePath := filepath.Join(filepath.Dir(pp), "rollout-2026-09-01T09-00-00-"+forkReviewParent+"_33333333-3333-4333-8333-333333333333.jsonl")
			head := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "head-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "head answer", forkReviewTime))
			page := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "page answer", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 100, 40, 0))
			child := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "child answer", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0))
			require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))+head), 0o600))
			require.NoError(t, os.WriteFile(pagePath, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(forkReviewParent, "/work/project", "user", forkReviewTime, map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": forkReviewParent, "end_ordinal_exclusive": 3}}))+page), 0o600))
			require.NoError(t, os.WriteFile(cp, []byte(meta+head+page+child), 0o600))
			require.Zero(t, e.SyncAll(t.Context(), nil).Failed)
			require.Equal(t, []string{"child answer"}, messageContents(t, d, "codex:"+forkReviewChild))
			require.NoError(t, os.Remove(pagePath))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
			assert.Equal(t, []string{"child answer"}, messageContents(t, d, "codex:"+forkReviewChild))
			u, err := d.GetSessionUsage(t.Context(), "codex:"+forkReviewChild, true)
			require.NoError(t, err)
			require.NotNil(t, u)
			assert.Equal(t, 5, u.TotalOutputTokens)
		})
	}
}

func TestCodexForkReviewStagedResultPrefix(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			root := t.TempDir()
			cp := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+forkReviewChild+".jsonl")
			body := testjsonl.JoinJSONL(
				testjsonl.CodexForkedSessionMetaJSON(forkReviewChild, forkReviewParent, "/work/project", "user", forkReviewTime),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime),
				testjsonl.CodexFunctionCallWithCallIDJSON("wait", "call_wait", map[string]any{"ids": []string{"agent-1"}}, forkReviewTime),
				testjsonl.CodexFunctionCallOutputJSON("call_wait", map[string]any{"status": map[string]any{"agent-1": map[string]any{"completed": "subagent finished"}}}, forkReviewTime),
			)
			require.NoError(t, os.WriteFile(cp, []byte(body), 0o600))
			d := openTestDB(t)
			e := NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "remote", IDPrefix: "remote-host~", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: stage})
			t.Cleanup(e.Close)
			id := "remote-host~codex:" + forkReviewChild
			require.Zero(t, e.SyncAll(t.Context(), nil).Failed)
			first, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, first, 1)
			require.Len(t, first[0].ToolCalls, 1)
			require.Len(t, first[0].ToolCalls[0].ResultEvents, 1)
			require.Equal(t, "remote-host~codex:agent-1", first[0].ToolCalls[0].ResultEvents[0].SubagentSessionID)
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			ms, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, ms, 1)
			require.Len(t, ms[0].ToolCalls, 1)
			require.Len(t, ms[0].ToolCalls[0].ResultEvents, 1)
			assert.Equal(t, "remote-host~codex:agent-1", ms[0].ToolCalls[0].ResultEvents[0].SubagentSessionID)
		})
	}
}
