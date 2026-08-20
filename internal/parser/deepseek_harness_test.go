package parser

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const deepSeekHarnessFixtureCwd = "/workspace/example"

func TestDeepSeekHarnessPathEncodingMatchesPinnedFormat(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{raw: ".", want: "~002E"},
		{raw: "..", want: "~002E~002E"},
		{raw: "session:one", want: "session~003Aone"},
		{raw: "开发/~agent", want: "~5F00~53D1~002F~007Eagent"},
		{raw: "whale-🐋", want: "whale-~D83D~DC0B"},
	} {
		t.Run(test.want, func(t *testing.T) {
			encoded := encodeDeepSeekHarnessSegment(test.raw)
			assert.Equal(t, test.want, encoded)
			decoded, err := decodeDeepSeekHarnessSegment(test.want)
			require.NoError(t, err)
			assert.Equal(t, test.raw, decoded)
		})
	}

	assert.Equal(t, "--workspace-example--", deepSeekHarnessProjectKey("/workspace/example"))
	assert.Equal(t, "--~5F00~53D1-~007Eagent--", deepSeekHarnessProjectKey("/开发/~agent"))
	assert.Equal(t, "--root--", deepSeekHarnessProjectKey("/"))
}

func TestDeepSeekHarnessSafeIntegerJSONSemantics(t *testing.T) {
	for _, test := range []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "integer", raw: "7", want: 7},
		{name: "integral decimal", raw: "7.0", want: 7},
		{name: "integral exponent", raw: "1e3", want: 1000},
		{name: "fraction", raw: "0.5", wantErr: true},
		{name: "quoted number", raw: `"7"`, wantErr: true},
		{name: "outside safe range", raw: "9007199254740992", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := deepSeekHarnessRequiredSafeInt(
				map[string]jsontext.Value{"value": jsontext.Value(test.raw)},
				"value", false,
			)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, value)
		})
	}
}

func TestBuildDeepSeekHarnessPartialMessageSkipsMissingBlockState(t *testing.T) {
	response := &deepSeekHarnessResponse{
		FirstChunkTime: 1234,
		Blocks: map[int64]*deepSeekHarnessBlockState{
			0: nil,
			1: {BlockType: "text", Text: *new(strings.Builder)},
		},
	}
	response.Blocks[1].Text.WriteString("visible")

	message, err := buildDeepSeekHarnessPartialMessage(response)
	require.NoError(t, err)
	assert.Equal(t, "visible", message.Content)
}

func TestDeepSeekHarnessLineReaderReturnsBlankLine(t *testing.T) {
	reader := newDeepSeekHarnessLineReader(strings.NewReader("\nnext"))

	line, terminated, err := reader.next()
	require.NoError(t, err)
	assert.True(t, terminated)
	assert.Empty(t, line)

	line, terminated, err = reader.next()
	require.NoError(t, err)
	assert.False(t, terminated)
	assert.Equal(t, []byte("next"), line)
}

func TestDeepSeekHarnessPlainAndMultiframeZstdNormalizeTheSameSession(t *testing.T) {
	records := deepSeekHarnessCompleteFixture("session:one", nil)

	for _, compression := range []string{"plain", "zstd"} {
		t.Run(compression, func(t *testing.T) {
			root := t.TempDir()
			path := writeDeepSeekHarnessFixture(
				t, root, "session:one", deepSeekHarnessFixtureCwd,
				compression, records,
			)
			result, err := parseDeepSeekHarnessSession(t.Context(), path, "fixture-host")
			require.NoError(t, err)

			session := result.Session
			assert.Equal(t, "deepseek-harness:session:one", session.ID)
			assert.Equal(t, AgentDeepSeekHarness, session.Agent)
			assert.Equal(t, "example", session.Project)
			assert.Equal(t, deepSeekHarnessFixtureCwd, session.Cwd)
			assert.Equal(t, "coding", session.AgentLabel)
			assert.Equal(t, "Newest title", session.SessionName)
			assert.Equal(t, "open image [image]", session.FirstMessage)
			assert.Equal(t, 1, session.UserMessageCount)
			assert.Equal(t, TerminationAwaitingUser, session.TerminationStatus)
			assert.False(t, session.IsTruncated)
			assert.Zero(t, session.MalformedLines)

			require.Len(t, result.Messages, 4)
			assert.Equal(t, RoleUser, result.Messages[0].Role)
			assert.Equal(t, "open image\n[image]", result.Messages[0].Content)
			assert.False(t, result.Messages[0].IsSystem)
			assert.Equal(t, "user", result.Messages[0].SourceType)
			assert.Equal(t, "injected instructions", result.Messages[1].Content)
			assert.True(t, result.Messages[1].IsSystem)
			assert.Equal(t, "plugin", result.Messages[1].SourceType)

			assistant := result.Messages[2]
			assert.Equal(t, RoleAssistant, assistant.Role)
			assert.Equal(t, "answer final", assistant.Content)
			assert.Equal(t, "thought final", assistant.ThinkingText)
			assert.True(t, assistant.HasThinking)
			assert.Equal(t, "deepseek-chat", assistant.Model)
			assert.Equal(t, "tool-calls", assistant.StopReason)
			assert.Empty(t, assistant.TokenUsage,
				"usage events are the sole analytics source")
			assert.Equal(t, 15, assistant.ContextTokens)
			assert.Equal(t, 5, assistant.OutputTokens)
			require.Len(t, assistant.ToolCalls, 1)
			assert.Equal(t, "call-1", assistant.ToolCalls[0].ToolUseID)
			assert.Equal(t, "read_file", assistant.ToolCalls[0].ToolName)
			assert.Equal(t, `{"path":"x"}`, assistant.ToolCalls[0].InputJSON)

			carrier := result.Messages[3]
			assert.Equal(t, RoleUser, carrier.Role)
			assert.Empty(t, carrier.Content)
			assert.True(t, carrier.IsSystem)
			require.Len(t, carrier.ToolResults, 1)
			assert.Equal(t, "call-1", carrier.ToolResults[0].ToolUseID)
			assert.Contains(t, carrier.ToolResults[0].ContentRaw, "file data")
			assert.Contains(t, carrier.ToolResults[0].ContentRaw, "[image]")

			require.Len(t, result.UsageEvents, 2)
			usage := result.UsageEvents[0]
			if usage.OutputTokens == 0 {
				usage = result.UsageEvents[1]
			}
			assert.Equal(t, 10, usage.InputTokens)
			assert.Equal(t, 5, usage.OutputTokens)
			assert.Equal(t, 3, usage.CacheReadInputTokens)
			assert.Equal(t, 2, usage.CacheCreationInputTokens)
			assert.Equal(t, 4, usage.ReasoningTokens)
			require.NotNil(t, usage.MessageOrdinal)
			assert.Equal(t, 2, *usage.MessageOrdinal)
			assert.Equal(t, 5, session.TotalOutputTokens)
			assert.Equal(t, 15, session.PeakContextTokens)
		})
	}
}

func TestDeepSeekHarnessCanonicalIDEscapesRemoteSeparator(t *testing.T) {
	root := t.TempDir()
	const rawID = "child~branch%7E1%25/part?#"
	records := deepSeekHarnessCompleteFixture(rawID, map[string]any{
		"parentSession": "parent%25~root/path?#",
	})
	path := writeDeepSeekHarnessFixture(
		t, root, rawID, deepSeekHarnessFixtureCwd, "plain", records,
	)
	provider, ok := NewProvider(
		AgentDeepSeekHarness,
		ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: discovered[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	session := outcome.Results[0].Result.Session
	assert.Equal(t, "deepseek-harness:child%7Ebranch%257E1%2525/part?#", session.ID)
	assert.Equal(t, rawID, session.SourceSessionID)
	assert.Equal(t, "deepseek-harness:parent%2525%7Eroot/path?#", session.ParentSessionID)
	host, strippedID := StripHostPrefix(session.ID)
	assert.Empty(t, host)
	assert.Equal(t, session.ID, strippedID)
	def, found := AgentByPrefix(session.ID)
	require.True(t, found)
	assert.Equal(t, AgentDeepSeekHarness, def.Type)

	for _, test := range []struct {
		name string
		req  FindSourceRequest
	}{
		{
			name: "explicit raw ID stays literal",
			req: FindSourceRequest{
				StoredFilePath: filepath.Join(root, "stale", "session.jsonl"),
				RawSessionID:   rawID,
			},
		},
		{
			name: "canonical full ID is decoded",
			req: FindSourceRequest{
				StoredFilePath: filepath.Join(root, "stale", "session.jsonl"),
				FullSessionID:  session.ID,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			foundSource, ok, err := provider.FindSource(t.Context(), test.req)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, path, foundSource.DisplayPath)
		})
	}
}

func TestDeepSeekHarnessAbsentRootDoesNotWalkFilesystem(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "missing")
	provider, ok := NewProvider(
		AgentDeepSeekHarness,
		ProviderConfig{Roots: []string{missingRoot}},
	)
	require.True(t, ok)
	directoryReads := 0
	ctx := withStreamingDirectoryReader(t.Context(), func(
		context.Context, string, func(os.DirEntry) error,
	) error {
		directoryReads++
		return nil
	})

	discovered, err := provider.Discover(ctx)

	require.NoError(t, err)
	assert.Empty(t, discovered)
	assert.Zero(t, directoryReads,
		"a missing default Harness root must not start a directory walk")
}

func BenchmarkDeepSeekHarnessAbsentRootDiscovery(b *testing.B) {
	missingRoot := filepath.Join(b.TempDir(), "missing")
	provider, ok := NewProvider(
		AgentDeepSeekHarness,
		ProviderConfig{Roots: []string{missingRoot}},
	)
	if !ok {
		b.Fatal("DeepSeek Harness provider is not registered")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := provider.Discover(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDeepSeekHarnessRebuildsInterruptedReplyFromPackedChunks(t *testing.T) {
	records := []any{
		deepSeekHarnessFixtureHeader("partial", deepSeekHarnessFixtureCwd, nil),
		deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(2, "user/message", deepSeekHarnessUser("continue", "user"), "append"),
		deepSeekHarnessFixtureEvent(3, "request/header", deepSeekHarnessRequest("deepseek-reasoner"), nil),
		deepSeekHarnessFixtureEvent(4, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "block-start", "index": 0, "blockType": "reasoning",
		}), nil),
		deepSeekHarnessPackedText("reasoning-chunks", 5, 1, 1, 0, []string{"still ", "work", "ing"}),
		deepSeekHarnessFixtureEvent(8, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "block-start", "index": 1, "blockType": "text",
		}), nil),
		deepSeekHarnessPackedText("text-chunks", 9, 1, 1, 1, []string{"inter", "rupt", "ed"}),
		deepSeekHarnessFixtureEvent(12, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "block-end", "index": 1,
			"block": map[string]any{"type": "text", "text": "authoritative interrupted"},
		}), nil),
		deepSeekHarnessFixtureEvent(13, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "usage", "usage": deepSeekHarnessUsageMap(7, 3, 2, 1, 2),
		}), nil),
		deepSeekHarnessFixtureEvent(14, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "finish", "reason": map[string]any{"kind": "aborted", "failure": map[string]any{"message": "cancelled", "code": "ABORTED"}},
		}), nil),
	}
	path := writeDeepSeekHarnessFixture(
		t, t.TempDir(), "partial", deepSeekHarnessFixtureCwd, "zstd", records,
	)

	result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
	require.NoError(t, err)
	require.Len(t, result.Messages, 2)
	partial := result.Messages[1]
	assert.Equal(t, "authoritative interrupted", partial.Content)
	assert.Equal(t, "still working", partial.ThinkingText)
	assert.Equal(t, "deepseek-reasoner", partial.Model)
	assert.Equal(t, "aborted", partial.StopReason)
	assert.Equal(t, 10, partial.ContextTokens)
	assert.Equal(t, 3, partial.OutputTokens)
	assert.Empty(t, result.Session.TerminationStatus)
	assert.False(t, result.Session.IsTruncated)
}

func TestDeepSeekHarnessSurfaceReplacementDoesNotReplaceHumanTranscript(t *testing.T) {
	records := []any{
		deepSeekHarnessFixtureHeader("replace", deepSeekHarnessFixtureCwd, nil),
		deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(2, "user/message", deepSeekHarnessUser("original prompt", "user"), "append"),
		deepSeekHarnessFixtureEvent(3, "request/header", deepSeekHarnessRequest("request-model"), nil),
		deepSeekHarnessFixtureEvent(4, "assistant/message", deepSeekHarnessAssistantDataMapWithModel(
			1, 1, "visible answer", "visible-model",
			deepSeekHarnessUsageMap(8, 2, 1, 1, 1),
		), "append"),
		deepSeekHarnessFixtureEvent(5, "assistant/message", deepSeekHarnessAssistantDataMapWithModel(
			1, 1, "compaction summary", "replacement-model",
			deepSeekHarnessUsageMap(80, 20, 10, 10, 10),
		), map[string]any{"op": "replace", "start": 1, "end": 4}),
		deepSeekHarnessFixtureEvent(6, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(7, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
	}
	path := writeDeepSeekHarnessFixture(
		t, t.TempDir(), "replace", deepSeekHarnessFixtureCwd, "plain", records,
	)

	result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
	require.NoError(t, err)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "original prompt", result.Messages[0].Content)
	assert.Equal(t, "visible answer", result.Messages[1].Content)
	assert.NotContains(t, result.Messages[1].Content, "compaction")
	assert.Equal(t, "visible-model", result.Messages[1].Model)
	assert.Equal(t, 10, result.Messages[1].ContextTokens)
	assert.Equal(t, 2, result.Messages[1].OutputTokens)
	require.Len(t, result.UsageEvents, 1)
	assert.Equal(t, 8, result.UsageEvents[0].InputTokens)
	assert.Equal(t, 2, result.UsageEvents[0].OutputTokens)
	assert.Equal(t, "visible-model", result.UsageEvents[0].Model)
	assert.Equal(t, 2, result.Session.TotalOutputTokens)
	assert.Equal(t, 10, result.Session.PeakContextTokens)
}

func TestDeepSeekHarnessSurfaceReplacementDoesNotSuppressChunkOnlyReply(t *testing.T) {
	records := []any{
		deepSeekHarnessFixtureHeader("replace-partial", deepSeekHarnessFixtureCwd, nil),
		deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(2, "user/message", deepSeekHarnessUser("original prompt", "user"), "append"),
		deepSeekHarnessFixtureEvent(3, "request/header", deepSeekHarnessRequest("chunk-model"), nil),
		deepSeekHarnessFixtureEvent(4, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "text-delta", "index": 0, "text": "chunk reply",
		}), nil),
		deepSeekHarnessFixtureEvent(5, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "usage", "usage": deepSeekHarnessUsageMap(7, 3, 2, 1, 2),
		}), nil),
		deepSeekHarnessFixtureEvent(6, "assistant/message", deepSeekHarnessAssistantDataMapWithModel(
			1, 1, "replacement reply", "replacement-model",
			deepSeekHarnessUsageMap(70, 30, 20, 10, 20),
		), map[string]any{"op": "replace", "start": 2, "end": 5}),
		deepSeekHarnessFixtureEvent(7, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(8, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
	}
	path := writeDeepSeekHarnessFixture(
		t, t.TempDir(), "replace-partial", deepSeekHarnessFixtureCwd, "plain", records,
	)

	result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
	require.NoError(t, err)
	require.Len(t, result.Messages, 2)
	partial := result.Messages[1]
	assert.Equal(t, "chunk reply", partial.Content)
	assert.Equal(t, "chunk-model", partial.Model)
	assert.Equal(t, 10, partial.ContextTokens)
	assert.Equal(t, 3, partial.OutputTokens)
	require.Len(t, result.UsageEvents, 1)
	assert.Equal(t, 7, result.UsageEvents[0].InputTokens)
	assert.Equal(t, 3, result.UsageEvents[0].OutputTokens)
	assert.Equal(t, "chunk-model", result.UsageEvents[0].Model)
}

func TestDeepSeekHarnessPreservesExtensibleCloseReasons(t *testing.T) {
	records := []any{
		deepSeekHarnessFixtureHeader("close-reasons", deepSeekHarnessFixtureCwd, nil),
		deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(2, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "text-delta", "index": 0, "text": "partial",
		}), nil),
		deepSeekHarnessFixtureEvent(3, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "finish", "reason": map[string]any{"kind": "extension-stop"},
		}), nil),
		deepSeekHarnessFixtureEvent(4, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(5, "turn/end", deepSeekHarnessTurnEnd(1, "extension-close"), nil),
	}
	path := writeDeepSeekHarnessFixture(
		t, t.TempDir(), "close-reasons", deepSeekHarnessFixtureCwd, "plain", records,
	)

	result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
	require.NoError(t, err)
	assert.Equal(t, TerminationClean, result.Session.TerminationStatus)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "extension-stop", result.Messages[0].StopReason)
}

func TestDeepSeekHarnessSeedBoundaryOwnsMessagesAndUsage(t *testing.T) {
	for _, origin := range []struct {
		name  string
		value any
		want  RelationshipType
	}{
		{name: "subagent", value: "subagent", want: RelSubagent},
		{name: "fork", value: nil, want: RelFork},
	} {
		t.Run(origin.name, func(t *testing.T) {
			headerExtra := map[string]any{
				"parentSession": "parent", "seedLength": 12,
			}
			if origin.value != nil {
				headerExtra["origin"] = origin.value
			}
			records := []any{
				deepSeekHarnessFixtureHeader("child", deepSeekHarnessFixtureCwd, headerExtra),
				deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
				deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
				deepSeekHarnessFixtureEvent(2, "user/message", deepSeekHarnessUser("parent prompt", "user"), "append"),
				deepSeekHarnessFixtureEvent(3, "request/header", deepSeekHarnessRequest("deepseek-chat"), nil),
				deepSeekHarnessFixtureEvent(4, "assistant/message", deepSeekHarnessAssistantDataMap(
					1, 1, "parent answer", deepSeekHarnessUsageMap(100, 100, 0, 0, 0),
				), "append"),
				deepSeekHarnessFixtureEvent(5, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
				deepSeekHarnessFixtureEvent(6, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
				deepSeekHarnessFixtureEvent(7, "compaction/start", map[string]any{
					"compactionId": "parent-compaction", "turn": nil,
				}, nil),
				deepSeekHarnessFixtureEvent(8, "compaction/summary", deepSeekHarnessCompactionSummary(
					"parent-compaction", "parent-summary-model", 2, 4,
					deepSeekHarnessUsageMap(100, 100, 0, 0, 0),
				), nil),
				deepSeekHarnessFixtureEvent(9, "user/message", deepSeekHarnessUser(
					"parent summary", "plugin",
				), map[string]any{"op": "replace", "start": 2, "end": 4}),
				deepSeekHarnessFixtureEvent(10, "compaction/end", map[string]any{
					"compactionId": "parent-compaction", "turn": nil,
				}, nil),
				deepSeekHarnessFixtureEvent(11, "session/title", map[string]any{
					"title": "Inherited title", "messageSeqs": []int{2}, "source": map[string]any{"kind": "fallback"},
				}, nil),
				deepSeekHarnessFixtureEvent(12, "turn/start", map[string]any{"turn": 2}, nil),
				deepSeekHarnessFixtureEvent(13, "step/start", map[string]any{"turn": 2, "step": 1}, nil),
				deepSeekHarnessFixtureEvent(14, "user/message", deepSeekHarnessUser("child prompt", "user"), "append"),
				deepSeekHarnessFixtureEvent(15, "request/header", deepSeekHarnessRequest("deepseek-chat"), nil),
				deepSeekHarnessFixtureEvent(16, "assistant/message", deepSeekHarnessAssistantDataMap(
					2, 1, "child answer", deepSeekHarnessUsageMap(8, 4, 1, 1, 1),
				), "append"),
				deepSeekHarnessFixtureEvent(17, "step/end", map[string]any{"turn": 2, "step": 1}, nil),
				deepSeekHarnessFixtureEvent(18, "turn/end", deepSeekHarnessTurnEnd(2, "completed"), nil),
			}
			path := writeDeepSeekHarnessFixture(
				t, t.TempDir(), "child", deepSeekHarnessFixtureCwd, "plain", records,
			)

			result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
			require.NoError(t, err)
			assert.Equal(t, "deepseek-harness:parent", result.Session.ParentSessionID)
			assert.Equal(t, origin.want, result.Session.RelationshipType)
			assert.Equal(t, "Inherited title", result.Session.SessionName)
			require.Len(t, result.Messages, 2)
			assert.Equal(t, "child prompt", result.Messages[0].Content)
			assert.Equal(t, "child answer", result.Messages[1].Content)
			require.Len(t, result.UsageEvents, 1)
			assert.Equal(t, 4, result.UsageEvents[0].OutputTokens)
			assert.Equal(t, 4, result.Session.TotalOutputTokens)
		})
	}
}

func TestDeepSeekHarnessEndSeedPreservesOpenLifecycle(t *testing.T) {
	records := []any{
		deepSeekHarnessFixtureHeader("open-seed", deepSeekHarnessFixtureCwd, map[string]any{
			"parentSession": "parent", "seedLength": 2,
		}),
		deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		// A constructor seed may end inside an open step. The boundary is
		// cursor-neutral, so live events continue that inherited lifecycle.
		deepSeekHarnessFixtureEvent(2, "session/end-seed", map[string]any{}, nil),
		deepSeekHarnessFixtureEvent(3, "assistant/message", deepSeekHarnessAssistantDataMap(
			1, 1, "continued inherited step", deepSeekHarnessUsageMap(4, 2, 0, 0, 0),
		), "append"),
		deepSeekHarnessFixtureEvent(4, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(5, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
	}
	path := writeDeepSeekHarnessFixture(
		t, t.TempDir(), "open-seed", deepSeekHarnessFixtureCwd, "plain", records,
	)

	result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
	require.NoError(t, err)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "continued inherited step", result.Messages[0].Content)
	assert.Equal(t, 2, result.Session.TotalOutputTokens)
	assert.Equal(t, TerminationAwaitingUser, result.Session.TerminationStatus)
}

func TestDeepSeekHarnessOpenSeedPartialToolCallIsPending(t *testing.T) {
	records := []any{
		deepSeekHarnessFixtureHeader("open-seed-tool", deepSeekHarnessFixtureCwd, map[string]any{
			"parentSession": "parent", "seedLength": 2,
		}),
		deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(2, "session/end-seed", map[string]any{}, nil),
		deepSeekHarnessFixtureEvent(3, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "tool-call-delta", "index": 0, "id": "call-1",
			"name": "read_file", "argumentsDelta": `{"path":"unfinished"}`,
		}), nil),
	}
	path := writeDeepSeekHarnessFixture(
		t, t.TempDir(), "open-seed-tool", deepSeekHarnessFixtureCwd, "plain", records,
	)

	result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
	require.NoError(t, err)
	require.Len(t, result.Messages, 1)
	require.Len(t, result.Messages[0].ToolCalls, 1)
	assert.Equal(t, "call-1", result.Messages[0].ToolCalls[0].ToolUseID)
	assert.Equal(t, TerminationToolCallPending, result.Session.TerminationStatus)
}

func TestDeepSeekHarnessFoldsPresetAndCompactionUsage(t *testing.T) {
	records := []any{
		deepSeekHarnessFixtureHeader("metadata-usage", deepSeekHarnessFixtureCwd, map[string]any{
			"agentPreset": "coding",
		}),
		deepSeekHarnessFixtureEvent(0, "agent-preset/selected", map[string]any{
			"agentPreset": "minimal",
		}, nil),
		deepSeekHarnessFixtureEvent(1, "turn/start", map[string]any{"turn": 1}, nil),
		deepSeekHarnessFixtureEvent(2, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(3, "user/message", deepSeekHarnessUser("compact this", "user"), "append"),
		deepSeekHarnessFixtureEvent(4, "request/header", deepSeekHarnessRequest("deepseek-chat"), nil),
		deepSeekHarnessFixtureEvent(5, "assistant/message", deepSeekHarnessAssistantDataMap(
			1, 1, "first answer", deepSeekHarnessUsageMap(8, 2, 1, 1, 1),
		), "append"),
		deepSeekHarnessFixtureEvent(6, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(7, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
		deepSeekHarnessFixtureEvent(8, "compaction/start", map[string]any{
			"compactionId": "compact-1", "turn": nil,
		}, nil),
		deepSeekHarnessFixtureEvent(9, "compaction/summary", deepSeekHarnessCompactionSummary(
			"compact-1", "deepseek-summary", 3, 5,
			deepSeekHarnessUsageMap(20, 5, 4, 3, 2),
		), nil),
		deepSeekHarnessFixtureEvent(10, "user/message", deepSeekHarnessUser(
			"compacted checkpoint", "plugin",
		), map[string]any{"op": "replace", "start": 3, "end": 5}),
		deepSeekHarnessFixtureEvent(11, "compaction/end", map[string]any{
			"compactionId": "compact-1", "turn": nil,
		}, nil),
	}
	path := writeDeepSeekHarnessFixture(
		t, t.TempDir(), "metadata-usage", deepSeekHarnessFixtureCwd, "plain", records,
	)

	result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
	require.NoError(t, err)
	assert.Equal(t, "minimal", result.Session.AgentLabel)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "compact this", result.Messages[0].Content)
	assert.Equal(t, "first answer", result.Messages[1].Content)
	require.Len(t, result.UsageEvents, 2)

	var compaction ParsedUsageEvent
	for _, usage := range result.UsageEvents {
		if usage.MessageOrdinal == nil {
			compaction = usage
		}
	}
	assert.Equal(t, "deepseek-harness", compaction.Source)
	assert.Equal(t, "deepseek-summary", compaction.Model)
	assert.Equal(t, 20, compaction.InputTokens)
	assert.Equal(t, 5, compaction.OutputTokens)
	assert.Equal(t, 4, compaction.CacheReadInputTokens)
	assert.Equal(t, 3, compaction.CacheCreationInputTokens)
	assert.Equal(t, 2, compaction.ReasoningTokens)
	assert.Equal(t, 7, result.Session.TotalOutputTokens)
}

func TestDeepSeekHarnessCompactionRangeUsesSurfaceOrder(t *testing.T) {
	records := []any{
		deepSeekHarnessFixtureHeader("compaction-range", deepSeekHarnessFixtureCwd, nil),
		deepSeekHarnessFixtureEvent(0, "compaction/start", map[string]any{
			"compactionId": "compact-1", "turn": nil,
		}, nil),
		deepSeekHarnessFixtureEvent(1, "compaction/summary", deepSeekHarnessCompactionSummary(
			"compact-1", "deepseek-summary", 9, 3,
			deepSeekHarnessUsageMap(4, 1, 0, 0, 0),
		), nil),
		deepSeekHarnessFixtureEvent(2, "user/message", deepSeekHarnessUser(
			"compacted checkpoint", "plugin",
		), map[string]any{"op": "replace", "start": 9, "end": 3}),
		deepSeekHarnessFixtureEvent(3, "compaction/end", map[string]any{
			"compactionId": "compact-1", "turn": nil,
		}, nil),
	}
	path := writeDeepSeekHarnessFixture(
		t, t.TempDir(), "compaction-range", deepSeekHarnessFixtureCwd, "plain", records,
	)

	result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
	require.NoError(t, err)
	require.Len(t, result.UsageEvents, 1)
	assert.Equal(t, 1, result.UsageEvents[0].OutputTokens)
}

func TestDeepSeekHarnessTornZstdFrameRetainsRecoveredEvents(t *testing.T) {
	records := []any{
		deepSeekHarnessFixtureHeader("zstd-recovery", deepSeekHarnessFixtureCwd, nil),
		deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(2, "user/message", deepSeekHarnessUser("committed prompt", "user"), "append"),
		deepSeekHarnessFixtureEvent(3, "assistant/message", deepSeekHarnessAssistantDataMap(
			1, 1, "committed answer", nil,
		), "append"),
		deepSeekHarnessFixtureEvent(4, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(5, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
		deepSeekHarnessFixtureEvent(6, "turn/start", map[string]any{"turn": 2}, nil),
		deepSeekHarnessFixtureEvent(7, "step/start", map[string]any{"turn": 2, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(8, "assistant/chunk", deepSeekHarnessChunk(2, 1, map[string]any{
			"type": "block-start", "index": 0, "blockType": "text",
		}), nil),
		deepSeekHarnessFixtureEvent(9, "assistant/chunk", deepSeekHarnessChunk(2, 1, map[string]any{
			"type": "text-delta", "index": 0, "text": "recovered interrupted reply",
		}), nil),
	}
	// Span multiple zstd blocks so the decoder can expose the complete early
	// records before it reaches the missing checksum at the physical tail.
	padding := deepSeekHarnessFixtureEvent(10, "fixture/padding", map[string]any{
		"text": strings.Repeat("0123456789abcdef", 32*1024),
	}, nil)
	padding["ignorable"] = true
	records = append(records, padding)
	path := writeDeepSeekHarnessFixture(
		t, t.TempDir(), "zstd-recovery", deepSeekHarnessFixtureCwd, "zstd", records,
	)

	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	headerFrame := encoder.EncodeAll(deepSeekHarnessFixtureBytes(t, records[:1]), nil)
	committedFrame := encoder.EncodeAll(deepSeekHarnessFixtureBytes(t, records[1:7]), nil)
	tornFrame := encoder.EncodeAll(deepSeekHarnessFixtureBytes(t, records[7:]), nil)
	encoder.Close()
	require.Greater(t, len(tornFrame), 4)
	// Preserve the full compressed payload but remove its checksum. Harness
	// treats complete JSONL rows recovered from this incomplete frame as work
	// worth retaining until the writer repairs the tail.
	tornFrame = tornFrame[:len(tornFrame)-4]
	encoded := append(headerFrame, committedFrame...)
	encoded = append(encoded, tornFrame...)
	require.NoError(t, os.WriteFile(path, encoded, 0o600))

	result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
	require.NoError(t, err)
	assert.True(t, result.Session.IsTruncated)
	assert.Equal(t, TerminationTruncated, result.Session.TerminationStatus)
	require.Len(t, result.Messages, 3)
	assert.Equal(t, "committed prompt", result.Messages[0].Content)
	assert.Equal(t, "committed answer", result.Messages[1].Content)
	assert.Equal(t, "recovered interrupted reply", result.Messages[2].Content)
}

func TestDeepSeekHarnessFormatErrorsAndCrashTails(t *testing.T) {
	t.Run("foreign version", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("foreign", nil)
		records[0].(map[string]any)["version"] = 1
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "foreign", deepSeekHarnessFixtureCwd, "plain", records)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported")
	})

	t.Run("fractional foreign version precedes current header validation", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("foreign-fractional", nil)
		header := records[0].(map[string]any)
		header["version"] = 0.5
		delete(header, "delegationDepth")
		path := writeDeepSeekHarnessFixture(
			t, t.TempDir(), "foreign-fractional", deepSeekHarnessFixtureCwd, "plain", records,
		)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported")
		assert.NotContains(t, err.Error(), "delegationDepth")
	})

	t.Run("quoted version is not numeric", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("quoted-version", nil)
		records[0].(map[string]any)["version"] = "0"
		path := writeDeepSeekHarnessFixture(
			t, t.TempDir(), "quoted-version", deepSeekHarnessFixtureCwd, "plain", records,
		)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version is not a number")
	})

	t.Run("bad header", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("bad-header", nil)
		delete(records[0].(map[string]any), "delegationDepth")
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "bad-header", deepSeekHarnessFixtureCwd, "plain", records)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "delegationDepth")
	})

	t.Run("path identity", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("header-id", nil)
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "directory-id", deepSeekHarnessFixtureCwd, "plain", records)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "header id")
	})

	t.Run("cwd identity", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("cwd-id", nil)
		records[0].(map[string]any)["cwd"] = "/workspace/other"
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "cwd-id", deepSeekHarnessFixtureCwd, "plain", records)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "header cwd")
	})

	t.Run("unknown required", func(t *testing.T) {
		records := []any{
			deepSeekHarnessFixtureHeader("unknown", deepSeekHarnessFixtureCwd, nil),
			deepSeekHarnessFixtureEvent(0, "future/required", map[string]any{}, nil),
		}
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "unknown", deepSeekHarnessFixtureCwd, "plain", records)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported required event")
	})

	t.Run("unknown ignorable", func(t *testing.T) {
		records := []any{
			deepSeekHarnessFixtureHeader("ignorable", deepSeekHarnessFixtureCwd, nil),
			map[string]any{"type": "future/info", "seq": 0, "time": 1700000000001, "data": map[string]any{}, "ignorable": true},
		}
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "ignorable", deepSeekHarnessFixtureCwd, "plain", records)
		result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.NoError(t, err)
		assert.Empty(t, result.Messages)
	})

	t.Run("unsafe event integer", func(t *testing.T) {
		records := []any{
			deepSeekHarnessFixtureHeader("unsafe-int", deepSeekHarnessFixtureCwd, nil),
			map[string]any{
				"type": "turn/start", "seq": 9007199254740992,
				"time": 1700000000001, "data": map[string]any{"turn": 1},
			},
			deepSeekHarnessFixtureEvent(0, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
		}
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "unsafe-int", deepSeekHarnessFixtureCwd, "plain", records)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "safe integer")
	})

	t.Run("committed seq gap", func(t *testing.T) {
		records := []any{
			deepSeekHarnessFixtureHeader("gap", deepSeekHarnessFixtureCwd, nil),
			deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
			deepSeekHarnessFixtureEvent(2, "user/message", deepSeekHarnessUser("lost", "user"), "append"),
			deepSeekHarnessFixtureEvent(3, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
		}
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "gap", deepSeekHarnessFixtureCwd, "plain", records)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "seq gap")
	})

	t.Run("committed lifecycle mismatch", func(t *testing.T) {
		records := []any{
			deepSeekHarnessFixtureHeader("lifecycle", deepSeekHarnessFixtureCwd, nil),
			deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
			deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
			deepSeekHarnessFixtureEvent(2, "step/end", map[string]any{"turn": 1, "step": 2}, nil),
			deepSeekHarnessFixtureEvent(3, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
		}
		path := writeDeepSeekHarnessFixture(
			t, t.TempDir(), "lifecycle", deepSeekHarnessFixtureCwd, "plain", records,
		)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "corrupt committed")
		assert.Contains(t, err.Error(), "step/end names turn 1/step 2")
	})

	t.Run("committed tool result without call", func(t *testing.T) {
		records := []any{
			deepSeekHarnessFixtureHeader("tool-lifecycle", deepSeekHarnessFixtureCwd, nil),
			deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
			deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
			deepSeekHarnessFixtureEvent(2, "tool/result", map[string]any{
				"turn": 1, "step": 1,
				"message": map[string]any{
					"id": "result-1", "role": "user",
					"source": map[string]any{"kind": "tool", "callId": "call-1"},
					"content": []any{map[string]any{
						"type": "tool-result", "toolCallId": "call-1", "isError": false,
						"content": []any{map[string]any{"type": "text", "text": "result"}},
					}},
				},
			}, "append"),
			deepSeekHarnessFixtureEvent(3, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
			deepSeekHarnessFixtureEvent(4, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
		}
		path := writeDeepSeekHarnessFixture(
			t, t.TempDir(), "tool-lifecycle", deepSeekHarnessFixtureCwd, "plain", records,
		)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "corrupt committed")
		assert.Contains(t, err.Error(), "no prior tool/call")
	})

	t.Run("damaged packed row in committed region", func(t *testing.T) {
		records := []any{
			deepSeekHarnessFixtureHeader("packed-damage", deepSeekHarnessFixtureCwd, nil),
			deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
			map[string]any{
				"type": "text-chunks", "seq0": 1, "time0": 1700000000002,
				"data": map[string]any{
					"turn": 1, "step": 1, "index": 0,
					"dt": []int{}, "texts": []string{"a", "b"},
				},
			},
			deepSeekHarnessFixtureEvent(3, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
		}
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "packed-damage", deepSeekHarnessFixtureCwd, "plain", records)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "packed chunk")
	})

	t.Run("unsupported content block", func(t *testing.T) {
		records := []any{
			deepSeekHarnessFixtureHeader("unknown-block", deepSeekHarnessFixtureCwd, nil),
			deepSeekHarnessFixtureEvent(0, "user/message", map[string]any{
				"id": "u", "role": "user", "source": map[string]any{"kind": "user"},
				"content": []any{map[string]any{"type": "future-block"}},
			}, "append"),
		}
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "unknown-block", deepSeekHarnessFixtureCwd, "plain", records)
		_, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported content block")
	})

	t.Run("raw torn line", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("raw-torn", nil)
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "raw-torn", deepSeekHarnessFixtureCwd, "plain", records)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteString(`{"type":`)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.NoError(t, err)
		assert.True(t, result.Session.IsTruncated)
		assert.Equal(t, TerminationTruncated, result.Session.TerminationStatus)
	})

	t.Run("bad complete row after committed turn", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("bad-tail-row", nil)
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "bad-tail-row", deepSeekHarnessFixtureCwd, "plain", records)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteString("not-json\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.NoError(t, err)
		assert.True(t, result.Session.IsTruncated)
		assert.Equal(t, 1, result.Session.MalformedLines)
	})

	t.Run("bad semantic row after committed turn", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("bad-tail-payload", nil)
		records = append(records, deepSeekHarnessFixtureEvent(35, "user/message", map[string]any{
			"id": "bad-tail", "role": "user",
			"content": []any{map[string]any{"type": "text", "text": "not committed"}},
		}, "append"))
		path := writeDeepSeekHarnessFixture(
			t, t.TempDir(), "bad-tail-payload", deepSeekHarnessFixtureCwd, "plain", records,
		)
		result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.NoError(t, err)
		assert.True(t, result.Session.IsTruncated)
		assert.Equal(t, 1, result.Session.MalformedLines)
		assert.Equal(t, 1, result.Session.UserMessageCount)
	})

	t.Run("seq gap after committed turn", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("tail-gap", nil)
		records = append(records, deepSeekHarnessFixtureEvent(
			36, "user/message", deepSeekHarnessUser("uncommitted", "user"), "append",
		))
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "tail-gap", deepSeekHarnessFixtureCwd, "plain", records)
		result, err := parseDeepSeekHarnessSession(t.Context(), path, "")
		require.NoError(t, err)
		assert.True(t, result.Session.IsTruncated)
		assert.Equal(t, 1, result.Session.UserMessageCount)
	})

	t.Run("zstd torn frame", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("zstd-torn", nil)
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "zstd-torn", deepSeekHarnessFixtureCwd, "zstd", records)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Greater(t, len(content), 16)
		for removed := 1; removed <= 12; removed++ {
			require.NoError(t, os.WriteFile(path, content[:len(content)-removed], 0o600))
			result, parseErr := parseDeepSeekHarnessSession(t.Context(), path, "")
			require.NoError(t, parseErr, "removed %d trailing bytes", removed)
			assert.True(t, result.Session.IsTruncated)
		}
	})

	t.Run("zstd frame without checksum", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("zstd-no-crc", nil)
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "zstd-no-crc", deepSeekHarnessFixtureCwd, "zstd", records)
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(false))
		require.NoError(t, err)
		encoded := encoder.EncodeAll(deepSeekHarnessFixtureBytes(t, records), nil)
		encoder.Close()
		require.NoError(t, os.WriteFile(path, encoded, 0o600))
		_, err = parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "without checksum")
	})

	t.Run("zstd header frame has an event", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("zstd-header-frame", nil)
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "zstd-header-frame", deepSeekHarnessFixtureCwd, "zstd", records)
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
		require.NoError(t, err)
		encoded := encoder.EncodeAll(deepSeekHarnessFixtureBytes(t, records), nil)
		encoder.Close()
		require.NoError(t, os.WriteFile(path, encoded, 0o600))
		_, err = parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "first frame is not exactly one header line")
	})

	t.Run("complete zstd frame has torn JSONL", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("zstd-torn-jsonl", nil)
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "zstd-torn-jsonl", deepSeekHarnessFixtureCwd, "zstd", records)
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
		require.NoError(t, err)
		encoded := encoder.EncodeAll(deepSeekHarnessFixtureBytes(t, records[:1]), nil)
		encoded = encoder.EncodeAll([]byte(`{"type":`), encoded)
		encoder.Close()
		require.NoError(t, os.WriteFile(path, encoded, 0o600))
		_, err = parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "complete frame contains a torn JSONL record")
	})

	t.Run("zstd checksum corruption", func(t *testing.T) {
		records := deepSeekHarnessCompleteFixture("zstd-crc", nil)
		path := writeDeepSeekHarnessFixture(t, t.TempDir(), "zstd-crc", deepSeekHarnessFixtureCwd, "zstd", records)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		content[len(content)-1] ^= 0xff
		require.NoError(t, os.WriteFile(path, content, 0o600))
		_, err = parseDeepSeekHarnessSession(t.Context(), path, "")
		require.Error(t, err)
		assert.Contains(t, strings.ToLower(err.Error()), "crc check failed")
	})
}

func TestDeepSeekHarnessProviderDiscoversAndMapsOneChangedPath(t *testing.T) {
	root := t.TempDir()
	records := deepSeekHarnessCompleteFixture("watch-id", nil)
	path := writeDeepSeekHarnessFixture(t, root, "watch-id", deepSeekHarnessFixtureCwd, "plain", records)
	writeDeepSeekHarnessFixture(t, root, "other-id", deepSeekHarnessFixtureCwd, "plain", deepSeekHarnessCompleteFixture("other-id", nil))
	require.NoError(t, os.WriteFile(filepath.Join(root, "unrelated.jsonl"), []byte("{}\n"), 0o600))

	provider, ok := NewProvider(AgentDeepSeekHarness, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	caps := provider.Capabilities()
	assert.Equal(t, CapabilitySupported, caps.Source.ForceReplaceOnParse)
	assert.Equal(t, CapabilitySupported, caps.Content.Relationships)
	assert.Equal(t, CapabilitySupported, caps.Content.Subagents)
	assert.Equal(t, CapabilitySupported, caps.Content.PerMessageTokenUsage)
	assert.Equal(t, CapabilitySupported, caps.Content.AggregateUsageEvents)
	assert.Equal(t, CapabilitySupported, caps.Content.TruncationStatus)
	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	fingerprint, err := provider.Fingerprint(t.Context(), discovered[0])
	require.NoError(t, err)
	assert.Empty(t, fingerprint.Hash, "Harness fingerprints must not hash whole files")

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: path, EventKind: "write", WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].DisplayPath)

	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: changed[0], Machine: "host"})
	require.NoError(t, err)
	assert.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "deepseek-harness:watch-id", outcome.Results[0].Result.Session.ID)

	require.NoError(t, os.Remove(path))
	removed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: path, EventKind: "remove", WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, removed, 1)
	assert.Equal(t, path, removed[0].DisplayPath)
}

func TestDeepSeekHarnessProviderRejectsMixedEncodingAndSwitchesAfterDeletion(t *testing.T) {
	root := t.TempDir()
	records := deepSeekHarnessCompleteFixture("mixed-encoding", nil)
	plainPath := writeDeepSeekHarnessFixture(
		t, root, "mixed-encoding", deepSeekHarnessFixtureCwd, "plain", records,
	)
	zstdPath := writeDeepSeekHarnessFixture(
		t, root, "mixed-encoding", deepSeekHarnessFixtureCwd, "zstd", records,
	)
	provider, ok := NewProvider(
		AgentDeepSeekHarness,
		ProviderConfig{Roots: []string{root}},
	)
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, zstdPath, discovered[0].DisplayPath)
	_, err = provider.Parse(t.Context(), ParseRequest{Source: discovered[0]})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both session.jsonl and session.jsonl.zstd")

	archiveScans := 0
	ctx := withStreamingDirectoryReader(
		t.Context(),
		func(context.Context, string, func(os.DirEntry) error) error {
			archiveScans++
			return nil
		},
	)
	changed, err := provider.SourcesForChangedPath(ctx, ChangedPathRequest{
		Path: plainPath, EventKind: "write", WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, zstdPath, changed[0].DisplayPath)
	assert.Zero(t, archiveScans, "one changed session must not scan the archive")

	require.NoError(t, os.Remove(zstdPath))
	changed, err = provider.SourcesForChangedPath(ctx, ChangedPathRequest{
		Path: zstdPath, EventKind: "remove", WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, plainPath, changed[0].DisplayPath)
	assert.Zero(t, archiveScans, "encoding fallback must remain session-local")
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: changed[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(
		t, "deepseek-harness:mixed-encoding",
		outcome.Results[0].Result.Session.ID,
	)
}

func deepSeekHarnessCompleteFixture(id string, headerExtra map[string]any) []any {
	return []any{
		deepSeekHarnessFixtureHeader(id, deepSeekHarnessFixtureCwd, headerExtra),
		deepSeekHarnessFixtureEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		deepSeekHarnessFixtureEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(2, "user/message", map[string]any{
			"id": "u1", "role": "user", "source": map[string]any{"kind": "user"},
			"content": []any{
				map[string]any{"type": "text", "text": "open image"},
				map[string]any{"type": "image", "attachment": map[string]any{"id": "image-1"}},
			},
		}, "append"),
		deepSeekHarnessFixtureEvent(3, "user/message", deepSeekHarnessUser("injected instructions", "plugin"), "append"),
		deepSeekHarnessFixtureEvent(4, "request/header", deepSeekHarnessRequest("fallback-model"), nil),
		deepSeekHarnessFixtureEvent(5, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "block-start", "index": 0, "blockType": "reasoning",
		}), nil),
		deepSeekHarnessPackedText("reasoning-chunks", 6, 1, 1, 0, []string{"draft ", "thought", " ignored"}),
		deepSeekHarnessFixtureEvent(9, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "block-end", "index": 0, "block": map[string]any{"type": "reasoning", "text": "thought from block-end"},
		}), nil),
		deepSeekHarnessFixtureEvent(10, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "block-start", "index": 1, "blockType": "text",
		}), nil),
		deepSeekHarnessPackedText("text-chunks", 11, 1, 1, 1, []string{"draft ", "answer", " ignored"}),
		deepSeekHarnessFixtureEvent(14, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "block-end", "index": 1, "block": map[string]any{"type": "text", "text": "answer from block-end"},
		}), nil),
		deepSeekHarnessFixtureEvent(15, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "block-start", "index": 2, "blockType": "tool-call",
		}), nil),
		map[string]any{
			"type": "tool-call-chunks", "seq0": 16, "time0": 1700000000017,
			"data": map[string]any{
				"turn": 1, "step": 1, "index": 2, "id": "call-1", "name": "read_file",
				"dt": []int{1, 1}, "args": []string{`{"path"`, `:`, `"x"}`},
			},
		},
		deepSeekHarnessFixtureEvent(19, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "block-end", "index": 2, "block": map[string]any{
				"type": "tool-call", "id": "call-1", "name": "read_file", "arguments": `{"path":"x"}`,
			},
		}), nil),
		deepSeekHarnessFixtureEvent(20, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "usage", "usage": deepSeekHarnessUsageMap(90, 90, 9, 9, 9),
		}), nil),
		deepSeekHarnessFixtureEvent(21, "assistant/chunk", deepSeekHarnessChunk(1, 1, map[string]any{
			"type": "finish", "reason": map[string]any{"kind": "tool-calls"},
		}), nil),
		deepSeekHarnessFixtureEvent(22, "assistant/message", map[string]any{
			"turn": 1, "step": 1,
			"message": map[string]any{
				"id": "a1", "role": "assistant",
				"source": map[string]any{"kind": "model", "provider": "deepseek", "model": "deepseek-chat"},
				"content": []any{
					map[string]any{"type": "reasoning", "text": "thought final"},
					map[string]any{"type": "text", "text": "answer final"},
					map[string]any{"type": "tool-call", "id": "call-1", "name": "read_file", "arguments": `{"path":"x"}`},
				},
			},
			"usage": deepSeekHarnessUsageMap(10, 5, 3, 2, 4),
		}, "append"),
		deepSeekHarnessFixtureEvent(23, "tool/call", map[string]any{
			"turn": 1, "step": 1, "callId": "call-1",
			"name": "read_file", "arguments": `{"path":"x"}`,
		}, nil),
		deepSeekHarnessFixtureEvent(24, "tool/result", map[string]any{
			"turn": 1, "step": 1,
			"message": map[string]any{
				"id": "tr1", "role": "user", "source": map[string]any{"kind": "tool", "callId": "call-1"},
				"content": []any{map[string]any{
					"type": "tool-result", "toolCallId": "call-1", "isError": false,
					"content": []any{
						map[string]any{"type": "text", "text": "file data"},
						map[string]any{"type": "image", "attachment": map[string]any{"id": "tool-image"}},
					},
				}},
			},
		}, "append"),
		deepSeekHarnessFixtureEvent(25, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(26, "turn/end", deepSeekHarnessTurnEnd(1, "completed"), nil),
		deepSeekHarnessFixtureEvent(27, "session/title", map[string]any{
			"title": "First title", "messageSeqs": []int{2}, "source": map[string]any{"kind": "fallback"},
		}, nil),
		deepSeekHarnessFixtureEvent(28, "turn/start", map[string]any{"turn": 2}, nil),
		deepSeekHarnessFixtureEvent(29, "step/start", map[string]any{"turn": 2, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(30, "request/header", deepSeekHarnessRequest("empty-model"), nil),
		deepSeekHarnessFixtureEvent(31, "assistant/message", map[string]any{
			"turn": 2, "step": 1,
			"message": map[string]any{
				"id": "empty", "role": "assistant", "content": []any{},
				"source": map[string]any{"kind": "model", "provider": "deepseek", "model": "empty-model"},
			},
			"usage": deepSeekHarnessUsageMap(1, 0, 0, 0, 0),
		}, "append"),
		deepSeekHarnessFixtureEvent(32, "step/end", map[string]any{"turn": 2, "step": 1}, nil),
		deepSeekHarnessFixtureEvent(33, "turn/end", deepSeekHarnessTurnEnd(2, "completed"), nil),
		deepSeekHarnessFixtureEvent(34, "session/title", map[string]any{
			"title": "Newest title", "messageSeqs": []int{2}, "source": map[string]any{"kind": "user"},
		}, nil),
	}
}

func deepSeekHarnessFixtureHeader(id, cwd string, extra map[string]any) map[string]any {
	header := map[string]any{
		"type": "session", "version": 0, "id": id,
		"createdAt": 1700000000000, "cwd": cwd,
		"delegationDepth": 0, "agentPreset": "coding",
	}
	maps.Copy(header, extra)
	return header
}

func deepSeekHarnessFixtureEvent(seq int, eventType string, data any, surface any) map[string]any {
	event := map[string]any{
		"type": eventType, "seq": seq, "time": 1700000000001 + seq, "data": data,
	}
	if surface != nil {
		event["surfaceOp"] = surface
	}
	return event
}

func deepSeekHarnessUser(text, kind string) map[string]any {
	source := map[string]any{"kind": kind}
	if kind == "plugin" {
		source["plugin"] = "fixture"
	}
	return map[string]any{
		"id": "user-" + kind, "role": "user", "source": source,
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

func deepSeekHarnessAssistantDataMap(turn, step int, text string, usage any) map[string]any {
	return deepSeekHarnessAssistantDataMapWithModel(
		turn, step, text, "deepseek-chat", usage,
	)
}

func deepSeekHarnessAssistantDataMapWithModel(
	turn, step int, text, model string, usage any,
) map[string]any {
	data := map[string]any{
		"turn": turn, "step": step,
		"message": map[string]any{
			"id": "assistant", "role": "assistant",
			"source":  map[string]any{"kind": "model", "provider": "deepseek", "model": model},
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}
	if usage != nil {
		data["usage"] = usage
	}
	return data
}

func deepSeekHarnessRequest(model string) map[string]any {
	return map[string]any{
		"header": map[string]any{"config": map[string]any{"provider": "deepseek", "model": model}},
		"reason": "initial",
	}
}

func deepSeekHarnessChunk(turn, step int, chunk any) map[string]any {
	return map[string]any{"turn": turn, "step": step, "chunk": chunk}
}

func deepSeekHarnessPackedText(
	typeName string, seq0, turn, step, index int, texts []string,
) map[string]any {
	dt := make([]int, len(texts)-1)
	for i := range dt {
		dt[i] = 1
	}
	return map[string]any{
		"type": typeName, "seq0": seq0, "time0": 1700000000001 + seq0,
		"data": map[string]any{
			"turn": turn, "step": step, "index": index, "dt": dt, "texts": texts,
		},
	}
}

func deepSeekHarnessUsageMap(input, output, cacheRead, cacheWrite, reasoning int) map[string]any {
	return map[string]any{
		"inputTokens": input, "outputTokens": output,
		"cacheReadTokens": cacheRead, "cacheWriteTokens": cacheWrite,
		"reasoningTokens": reasoning,
	}
}

func deepSeekHarnessCompactionSummary(
	id, model string, start, end int, usage any,
) map[string]any {
	shadowedSeqs := []int{start, end}
	if start == end {
		shadowedSeqs = []int{start}
	}
	return map[string]any{
		"compactionId": id,
		"summary": []any{
			map[string]any{"type": "text", "text": "safe summary"},
		},
		"shadowedRange":      map[string]any{"start": start, "end": end},
		"shadowedSeqs":       shadowedSeqs,
		"shadowedTokenCount": 12,
		"provider":           "deepseek",
		"model":              model,
		"rawOutput": []any{
			map[string]any{"type": "text", "text": "raw summary"},
		},
		"llmStreamCall": true,
		"usage":         usage,
	}
}

func deepSeekHarnessTurnEnd(turn int, kind string) map[string]any {
	return map[string]any{"turn": turn, "reason": map[string]any{"kind": kind}}
}

func deepSeekHarnessFixtureBytes(t *testing.T, records []any) []byte {
	t.Helper()
	var content bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		require.NoError(t, err)
		content.Write(line)
		content.WriteByte('\n')
	}
	return content.Bytes()
}

func writeDeepSeekHarnessFixture(
	t *testing.T, root, id, cwd, compression string, records []any,
) string {
	t.Helper()
	require.Equal(t, deepSeekHarnessFixtureCwd, cwd)
	encodedID := encodeDeepSeekHarnessSegment(id)
	dir := filepath.Join(root, "--workspace-example--", encodedID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	name := "session.jsonl"
	if compression == "zstd" {
		name += ".zstd"
	}
	path := filepath.Join(dir, name)
	lines := make([][]byte, 0, len(records))
	for _, record := range records {
		line, err := json.Marshal(record)
		require.NoError(t, err)
		lines = append(lines, append(line, '\n'))
	}
	if compression == "plain" {
		require.NoError(t, os.WriteFile(path, bytes.Join(lines, nil), 0o600))
		return path
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	defer encoder.Close()
	var encoded []byte
	for start := 0; start < len(lines); {
		end := start + 4
		if start == 0 {
			end = 1
		}
		if end > len(lines) {
			end = len(lines)
		}
		encoded = encoder.EncodeAll(bytes.Join(lines[start:end], nil), encoded)
		start = end
	}
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	return path
}
