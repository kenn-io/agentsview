package parser

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeFullParseDoesNotAllocateDiscardedProgressBytes(t *testing.T) {
	path := t.TempDir() + "/large.jsonl"
	f, err := os.Create(path)
	require.NoError(t, err)

	const (
		lineCount   = 32
		paddingSize = 1 << 20
	)
	padding := strings.Repeat("x", paddingSize)
	for i := range lineCount {
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("u%d", i-1)
		}
		var record string
		switch i {
		case lineCount - 2:
			record = fmt.Sprintf(
				"{\"type\":\"assistant\",\"uuid\":\"u%d\",\"parentUuid\":\"%s\","+
					"\"timestamp\":\"2026-01-01T00:00:%02dZ\",\"requestId\":\"req-1\","+
					"\"message\":{\"id\":\"msg-1\",\"model\":\"model-1\","+
					"\"usage\":{\"input_tokens\":4,\"output_tokens\":2},"+
					"\"content\":[{\"type\":\"text\",\"text\":\"answer\"},"+
					"{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"Read\","+
					"\"input\":{\"file_path\":\"notes.txt\"}}]},\"ignored\":\"%s\"}\n",
				i, parent, i%60, padding,
			)
		case lineCount - 1:
			record = fmt.Sprintf(
				"{\"type\":\"user\",\"uuid\":\"u%d\",\"parentUuid\":\"%s\","+
					"\"timestamp\":\"2026-01-01T00:00:%02dZ\","+
					"\"message\":{\"content\":[{\"type\":\"tool_result\","+
					"\"tool_use_id\":\"tool-1\",\"content\":\"result\"}]},"+
					"\"ignored\":\"%s\"}\n",
				i, parent, i%60, padding,
			)
		default:
			record = fmt.Sprintf(
				"{\"type\":\"progress\",\"timestamp\":\"2026-01-01T00:00:%02dZ\","+
					"\"data\":{\"type\":\"hook_progress\",\"payload\":\"%s\"}}\n",
				i%60, padding,
			)
		}
		_, err = f.WriteString(record)
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	results, excluded, err := claudeParseWithExclusions(path, "project", "local")
	require.NoError(t, err)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	require.Len(t, results, 1)
	assert.Empty(t, excluded)
	assert.Len(t, results[0].Messages, 2)
	assistant := results[0].Messages[0]
	assert.Equal(t, "model-1", assistant.Model)
	assert.JSONEq(t, `{"input_tokens":4,"output_tokens":2}`, string(assistant.TokenUsage))
	require.Len(t, assistant.ToolCalls, 1)
	assert.Equal(t, `{"file_path":"notes.txt"}`, assistant.ToolCalls[0].InputJSON)
	last := results[0].Messages[1]
	require.Len(t, last.ToolResults, 1)
	assert.Equal(t, `"result"`, last.ToolResults[0].ContentRaw)
	allocated := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, allocated, uint64(lineCount*paddingSize/2),
		"full parse should not copy ignored JSONL payload into the Go heap")
	runtime.KeepAlive(results)
}
