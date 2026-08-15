package sync_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestDeepSeekHarnessSyncReplacesPartialResponseAndDeduplicatesSeedUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:  "deepseek-chat",
			InputPerMTok:  money.MustParseDollars("1"),
			OutputPerMTok: money.MustParseDollars("2"),
		},
		{
			ModelPattern:  "deepseek-summary",
			InputPerMTok:  money.MustParseDollars("1"),
			OutputPerMTok: money.MustParseDollars("2"),
		},
	}))
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentDeepSeekHarness: {root},
		},
		Machine: "local",
	})

	parentEvents := []any{
		harnessSyncEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		harnessSyncEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		harnessSyncEvent(2, "user/message", harnessSyncUser("start work"), "append"),
		harnessSyncEvent(3, "request/header", harnessSyncRequest(), nil),
		harnessSyncEvent(4, "assistant/chunk", map[string]any{
			"turn": 1, "step": 1,
			"chunk": map[string]any{"type": "block-start", "index": 0, "blockType": "text"},
		}, nil),
		harnessSyncEvent(5, "assistant/chunk", map[string]any{
			"turn": 1, "step": 1,
			"chunk": map[string]any{"type": "text-delta", "index": 0, "text": "draft reply"},
		}, nil),
	}
	parentPath := harnessSyncWriteLog(t, root, "parent", nil, parentEvents)
	siblingPath := harnessSyncWriteLog(t, root, "untouched", nil, parentEvents)

	engine.SyncPaths([]string{parentPath})

	parentID := "deepseek-harness:parent"
	messages, err := database.GetMessages(t.Context(), parentID, 0, 20, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "draft reply", messages[1].Content)
	untouched, err := database.GetSession(t.Context(), "deepseek-harness:untouched")
	require.NoError(t, err)
	assert.Nil(t, untouched, "single-path sync must not scan a sibling session")

	finalEvents := []any{
		harnessSyncEvent(6, "assistant/message", harnessSyncAssistant(
			1, 1, "final reply", 5,
		), "append"),
		harnessSyncEvent(7, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		harnessSyncEvent(8, "turn/end", map[string]any{
			"turn": 1, "reason": map[string]any{"kind": "completed"},
		}, nil),
		harnessSyncEvent(9, "compaction/start", map[string]any{
			"compactionId": "compact-parent", "turn": nil,
		}, nil),
		harnessSyncEvent(10, "compaction/summary", harnessSyncCompactionSummary(), nil),
		harnessSyncEvent(11, "user/message", harnessSyncUser(
			"compacted parent",
		), map[string]any{"op": "replace", "start": 2, "end": 6}),
		harnessSyncEvent(12, "compaction/end", map[string]any{
			"compactionId": "compact-parent", "turn": nil,
		}, nil),
	}
	harnessSyncAppendFrame(t, parentPath, finalEvents)
	parentEvents = append(parentEvents, finalEvents...)

	engine.SyncPaths([]string{parentPath})

	messages, err = database.GetMessages(t.Context(), parentID, 0, 20, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "final reply", messages[1].Content)
	assert.NotContains(t, messages[1].Content, "draft")
	assert.Empty(t, messages[1].TokenUsage,
		"the usage event must be the only persisted analytics row")
	assert.Equal(t, 5, messages[1].OutputTokens)
	daily, err := database.GetDailyUsage(t.Context(), db.UsageFilter{
		From: "2023-11-14", To: "2023-11-14",
		Agent: "deepseek-harness", Timezone: "UTC",
	})
	require.NoError(t, err)
	require.NotNil(t, daily.Pricing)
	require.Contains(t, daily.Pricing.Models, "deepseek-chat")
	chatPricing := daily.Pricing.Models["deepseek-chat"]
	require.Len(t, chatPricing.Resolutions, 1)
	assert.Equal(t, 1,
		chatPricing.Resolutions[0].Application.BaseRequestCount,
		"one model response must produce one priced request")
	assert.Equal(t, 30, daily.Totals.InputTokens)
	assert.Equal(t, 8, daily.Totals.OutputTokens)

	childEvents := append([]any(nil), parentEvents...)
	childEvents = append(childEvents,
		harnessSyncEvent(13, "turn/start", map[string]any{"turn": 2}, nil),
		harnessSyncEvent(14, "step/start", map[string]any{"turn": 2, "step": 1}, nil),
		harnessSyncEvent(15, "user/message", harnessSyncUser("child work"), "append"),
		harnessSyncEvent(16, "request/header", harnessSyncRequest(), nil),
		harnessSyncEvent(17, "assistant/message", harnessSyncAssistant(
			2, 1, "child reply", 2,
		), "append"),
		harnessSyncEvent(18, "step/end", map[string]any{"turn": 2, "step": 1}, nil),
		harnessSyncEvent(19, "turn/end", map[string]any{
			"turn": 2, "reason": map[string]any{"kind": "completed"},
		}, nil),
	)
	childPath := harnessSyncWriteLog(t, root, "child", map[string]any{
		"parentSession": "parent", "seedLength": 13, "origin": "subagent",
	}, childEvents)
	engine.SyncPaths([]string{childPath})

	parentUsage, err := database.GetSessionUsage(t.Context(), parentID, true)
	require.NoError(t, err)
	require.NotNil(t, parentUsage)
	assert.Equal(t, 8, parentUsage.TotalOutputTokens)
	childUsage, err := database.GetSessionUsage(
		t.Context(), "deepseek-harness:child", true,
	)
	require.NoError(t, err)
	require.NotNil(t, childUsage)
	assert.Equal(t, 2, childUsage.TotalOutputTokens)

	require.NoError(t, os.Remove(parentPath))
	engine.SyncPaths([]string{parentPath})
	removed, err := database.GetSession(t.Context(), parentID)
	require.NoError(t, err)
	assert.Nil(t, removed)

	require.NoError(t, os.Remove(siblingPath))
}

func TestDeepSeekHarnessSyncRetainsMixedEncodingSessionAndSwitchesAfterDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	root := t.TempDir()
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentDeepSeekHarness: {root},
		},
		Machine: "local",
	})
	zstdPath := harnessSyncWriteLogEncoding(
		t, root, "encoding-switch", "zstd", nil,
		harnessSyncCompleteTurn("zstd transcript"),
	)
	engine.SyncPaths([]string{zstdPath})

	const sessionID = "deepseek-harness:encoding-switch"
	messages, err := database.GetMessages(t.Context(), sessionID, 0, 20, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "zstd transcript", messages[1].Content)

	plainPath := harnessSyncWriteLogEncoding(
		t, root, "encoding-switch", "plain", nil,
		harnessSyncCompleteTurn("plain transcript"),
	)
	engine.SyncPaths([]string{plainPath})
	messages, err = database.GetMessages(t.Context(), sessionID, 0, 20, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "zstd transcript", messages[1].Content,
		"a mixed-encoding directory must retain the last valid parse")

	require.NoError(t, os.Remove(zstdPath))
	engine.SyncPaths([]string{zstdPath})
	messages, err = database.GetMessages(t.Context(), sessionID, 0, 20, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "plain transcript", messages[1].Content)
	assert.Equal(t, plainPath, database.GetSessionFilePath(sessionID))
}

func harnessSyncWriteLog(
	t *testing.T, root, id string, headerExtra map[string]any, events []any,
) string {
	t.Helper()
	return harnessSyncWriteLogEncoding(
		t, root, id, "zstd", headerExtra, events,
	)
}

func harnessSyncWriteLogEncoding(
	t *testing.T, root, id, compression string,
	headerExtra map[string]any, events []any,
) string {
	t.Helper()
	dir := filepath.Join(root, "--workspace-example--", id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	name := "session.jsonl"
	if compression == "zstd" {
		name += ".zstd"
	}
	path := filepath.Join(dir, name)
	header := map[string]any{
		"type": "session", "version": 0, "id": id,
		"createdAt": 1700000000000, "cwd": "/workspace/example",
		"delegationDepth": 0,
	}
	maps.Copy(header, headerExtra)
	if compression == "zstd" {
		harnessSyncWriteFrames(t, path, false, []any{header}, events)
		return path
	}
	require.Equal(t, "plain", compression)
	var content bytes.Buffer
	for _, record := range append([]any{header}, events...) {
		line, err := json.Marshal(record)
		require.NoError(t, err)
		content.Write(line)
		content.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(path, content.Bytes(), 0o600))
	return path
}

func harnessSyncCompleteTurn(assistantText string) []any {
	return []any{
		harnessSyncEvent(0, "turn/start", map[string]any{"turn": 1}, nil),
		harnessSyncEvent(1, "step/start", map[string]any{"turn": 1, "step": 1}, nil),
		harnessSyncEvent(2, "user/message", harnessSyncUser("start work"), "append"),
		harnessSyncEvent(3, "request/header", harnessSyncRequest(), nil),
		harnessSyncEvent(4, "assistant/message", harnessSyncAssistant(
			1, 1, assistantText, 2,
		), "append"),
		harnessSyncEvent(5, "step/end", map[string]any{"turn": 1, "step": 1}, nil),
		harnessSyncEvent(6, "turn/end", map[string]any{
			"turn": 1, "reason": map[string]any{"kind": "completed"},
		}, nil),
	}
}

func harnessSyncAppendFrame(t *testing.T, path string, events []any) {
	t.Helper()
	harnessSyncWriteFrames(t, path, true, events)
}

func harnessSyncWriteFrames(t *testing.T, path string, appendFile bool, frames ...[]any) {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	defer encoder.Close()
	var encoded []byte
	for _, records := range frames {
		var plain bytes.Buffer
		for _, record := range records {
			line, err := json.Marshal(record)
			require.NoError(t, err)
			plain.Write(line)
			plain.WriteByte('\n')
		}
		encoded = encoder.EncodeAll(plain.Bytes(), encoded)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendFile {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o600)
	require.NoError(t, err)
	_, err = f.Write(encoded)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func harnessSyncEvent(seq int, eventType string, data, surface any) map[string]any {
	event := map[string]any{
		"type": eventType, "seq": seq, "time": 1700000000001 + seq, "data": data,
	}
	if surface != nil {
		event["surfaceOp"] = surface
	}
	return event
}

func harnessSyncUser(text string) map[string]any {
	return map[string]any{
		"id": "user", "role": "user", "source": map[string]any{"kind": "user"},
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

func harnessSyncRequest() map[string]any {
	return map[string]any{
		"header": map[string]any{
			"config": map[string]any{"provider": "deepseek", "model": "deepseek-chat"},
		},
		"reason": "initial",
	}
}

func harnessSyncAssistant(turn, step int, text string, output int) map[string]any {
	return map[string]any{
		"turn": turn, "step": step,
		"message": map[string]any{
			"id": "assistant", "role": "assistant",
			"source": map[string]any{
				"kind": "model", "provider": "deepseek", "model": "deepseek-chat",
			},
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
		"usage": map[string]any{
			"inputTokens": 10, "outputTokens": output,
			"cacheReadTokens": 1, "cacheWriteTokens": 1, "reasoningTokens": 1,
		},
	}
}

func harnessSyncCompactionSummary() map[string]any {
	return map[string]any{
		"compactionId": "compact-parent",
		"summary": []any{
			map[string]any{"type": "text", "text": "safe summary"},
		},
		"shadowedRange":      map[string]any{"start": 2, "end": 6},
		"shadowedSeqs":       []int{2, 6},
		"shadowedTokenCount": 11,
		"provider":           "deepseek",
		"model":              "deepseek-summary",
		"rawOutput": []any{
			map[string]any{"type": "text", "text": "raw summary"},
		},
		"llmStreamCall": true,
		"usage": map[string]any{
			"inputTokens": 20, "outputTokens": 3,
			"cacheReadTokens": 2, "cacheWriteTokens": 1, "reasoningTokens": 1,
		},
	}
}
