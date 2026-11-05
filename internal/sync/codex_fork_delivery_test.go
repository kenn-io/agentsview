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

func TestCodexForkRetainsSpawnLinkWithoutOutput(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			prefix := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallWithCallIDJSON("spawn_agent", "spawn", map[string]any{"message": "synthetic task"}, forkReviewTime))
			require.NoError(t, os.WriteFile(cp, []byte(prefix+testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("spawn", map[string]any{"agent_id": "agent-1"}, forkReviewTime))), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			require.NoError(t, os.WriteFile(cp, []byte(prefix), 0o600))
			for _, recovered := range []bool{false, true, true} {
				if recovered {
					require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
				}
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				ms, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
				require.NoError(t, err)
				require.Len(t, ms, 1)
				require.Len(t, ms[0].ToolCalls, 1)
				assert.Equal(t, "codex:agent-1", ms[0].ToolCalls[0].SubagentSessionID)
				assert.Empty(t, ms[0].ToolCalls[0].ResultEvents)
			}
		})
	}
}

func TestCodexForkStagedPreservesOptionalResultMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events bool
		images config.ToolResultImages
	}{
		{"summary/keep", false, config.ToolResultImagesKeep},
		{"summary/offload", false, config.ToolResultImagesOffload},
		{"events/keep", true, config.ToolResultImagesKeep},
		{"events/offload", true, config.ToolResultImagesOffload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, d, cp, _, meta := forkReviewEngine(t, 1, parser.AgentCodex)
			e.toolResultImages = tc.images
			require.NoError(t, os.WriteFile(cp, []byte(meta+testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime))), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			id := "codex:" + forkReviewChild
			messages, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 1)
			const text = "AKIA7QHWN2DKR4FYPLJM saved output"
			call := &messages[0].ToolCalls[0]
			call.ResultContent, call.ResultContentLength = text, len(text)
			if tc.events {
				call.ResultEvents = []db.ToolResultEvent{{ToolUseID: call.ToolUseID, Source: "function_call_output", Content: text, ContentLength: len(text)}}
			}
			// The normalized archive accepts summary-only results and optional
			// raw metadata. A fork retry must preserve both supported shapes.
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), id, messages))
			require.NoError(t, os.WriteFile(cp, []byte(meta), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
			stored, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Len(t, stored[0].ToolCalls, 1)
			assert.Equal(t, text, stored[0].ToolCalls[0].ResultContent)
			if tc.events {
				require.Len(t, stored[0].ToolCalls[0].ResultEvents, 1)
				assert.Nil(t, stored[0].ToolCalls[0].ResultEvents[0].RawContentDigest)
				assert.Nil(t, stored[0].ToolCalls[0].ResultEvents[0].SummaryParticipates)
			}
			s, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, s)
			assert.Equal(t, 1, s.SecretLeakCount)
		})
	}
}

func TestCodexForkStagedRetryKeepsPayloadsInScratch(t *testing.T) {
	for _, threshold := range []int64{1, 64 << 10} {
		t.Run(strconv.FormatInt(threshold, 10), func(t *testing.T) {
			e, d, cp, _, meta := forkReviewEngine(t, threshold, parser.AgentCodex)
			var body strings.Builder
			body.WriteString(meta)
			body.WriteString(testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime)))
			for i := range 4 {
				id := "call-" + strconv.Itoa(i)
				body.WriteString(testjsonl.JoinJSONL(testjsonl.CodexFunctionCallWithCallIDJSON("exec_command", id, map[string]any{"cmd": "echo fixture"}, forkReviewTime), testjsonl.CodexFunctionCallOutputJSON(id, strings.Repeat("x", 32<<10), forkReviewTime)))
			}
			require.NoError(t, os.WriteFile(cp, []byte(body.String()), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			// The retry source now falls below the threshold, but its retained
			// archive still contains the four large result bodies.
			require.NoError(t, os.WriteFile(cp, []byte(meta), 0o600))
			e.Close()
			e = NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {filepath.Dir(cp)}}, Machine: "local", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: threshold})
			t.Cleanup(e.Close)
			info, err := os.Stat(cp)
			require.NoError(t, err)
			result := e.processFile(t.Context(), parser.DiscoveredFile{Agent: parser.AgentCodex, Path: cp, SourceSize: info.Size(), ForceParse: true})
			defer result.releaseStaged()
			defer result.retentionLease.Release()
			require.NoError(t, result.err)
			require.Len(t, result.results, 1)
			assert.NotNil(t, result.staged, "retained fork history requires scratch staging even after source shortening")
			before := d.MessagesLoadCount()
			prepared, verdict, err := e.prepareSessionNormalizedContext(t.Context(), pendingWrite{sess: result.results[0].Session, msgs: result.results[0].Messages, staged: result.staged}, nil, false)
			require.NoError(t, err)
			require.Equal(t, sessionWriteOK, verdict)
			assert.Equal(t, before, d.MessagesLoadCount(), "staging must not hydrate all archived results")
			bytes := 0
			for _, m := range prepared.Messages {
				for _, call := range m.ToolCalls {
					bytes += len(call.ResultContent)
					for _, event := range call.ResultEvents {
						bytes += len(event.Content)
					}
				}
			}
			assert.Less(t, bytes, 128, "reconciled staged rows must retain metadata rather than the 128 KiB payload")
			for _, rebuild := range []bool{false, true} {
				before := d.MessagesLoadCount()
				if rebuild {
					stats := e.ResyncAll(t.Context(), nil)
					require.True(t, stats.ArchiveRebuilt)
					require.Zero(t, stats.Failed)
				} else {
					require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
				}
				assert.Equal(t, before, d.MessagesLoadCount(), "retries and rebuilds must not hydrate all archived results")
				stored, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
				require.NoError(t, err)
				require.Len(t, stored, 4)
				for _, m := range stored {
					require.Len(t, m.ToolCalls, 1)
					assert.Equal(t, strings.Repeat("x", 32<<10), m.ToolCalls[0].ResultContent)
				}
			}
		})
	}
}

func TestCodexForkStagedRetainedResultsApplyImagePolicy(t *testing.T) {
	e, d, cp, _, meta := forkReviewEngine(t, 1, parser.AgentCodex)
	e.toolResultImages = config.ToolResultImagesKeep
	prefix := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime))
	const image = `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	require.NoError(t, os.WriteFile(cp, []byte(prefix+testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("call_test", image, forkReviewTime))), 0o600))
	require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
	id := "codex:" + forkReviewChild
	before, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Contains(t, before[0].ToolCalls[0].ResultContent, "data:image/png")
	e.toolResultImages = config.ToolResultImagesDrop
	require.NoError(t, os.WriteFile(cp, []byte(prefix), 0o600))
	require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
	stored, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Len(t, stored[0].ToolCalls, 1)
	assert.NotContains(t, stored[0].ToolCalls[0].ResultContent, "data:image/png")
	assert.Contains(t, stored[0].ToolCalls[0].ResultContent, `"type":"agentsview_image"`)
	require.Len(t, stored[0].ToolCalls[0].ResultEvents, 1)
	assert.NotContains(t, stored[0].ToolCalls[0].ResultEvents[0].Content, "data:image/png")
}
