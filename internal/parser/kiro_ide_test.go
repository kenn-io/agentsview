package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKiroIDEFilePathNativeSet verifies that replace and create actions in
// a Kiro IDE execution log produce ParsedToolCalls with FilePath set
// natively from a.Input.File.
func TestKiroIDEFilePathNativeSet(t *testing.T) {
	execID := "exec-test-001"
	editFile := "src/server.go"
	writeFile := "src/new.go"

	actions := []kiroIDEExecAction{
		{
			ActionID:   "action-replace",
			ActionType: "replace",
			Input: kiroIDEActionInput{
				File:            editFile,
				OriginalContent: "package main\n",
				ModifiedContent: "package main\n\n// updated\n",
			},
		},
		{
			ActionID:   "action-create",
			ActionType: "create",
			Input: kiroIDEActionInput{
				File:            writeFile,
				ModifiedContent: "package main\n",
			},
		},
	}

	execLog := struct {
		ExecutionID string              `json:"executionId"`
		Actions     []kiroIDEExecAction `json:"actions"`
	}{
		ExecutionID: execID,
		Actions:     actions,
	}

	data, err := json.Marshal(execLog)
	require.NoError(t, err, "marshal exec log")

	logPath := filepath.Join(t.TempDir(), execID+".json")
	require.NoError(t, os.WriteFile(logPath, data, 0o600), "write exec log")

	execIndex := map[string]string{execID: logPath}
	h := kiroIDEHistoryEntry{ExecutionID: execID}

	_, calls := kiroIDEResolveAssistant(h, execIndex)
	require.NotEmpty(t, calls, "expected tool calls from execution log")
	require.Len(t, calls, 2, "expected replace and create tool calls")

	var editCall, writeCall ParsedToolCall
	for _, c := range calls {
		switch c.ToolName {
		case "Edit":
			editCall = c
		case "Write":
			writeCall = c
		}
	}

	assert.Equal(t, "Edit", editCall.ToolName, "edit ToolName")
	assert.Equal(t, editFile, editCall.FilePath, "edit FilePath")

	assert.Equal(t, "Write", writeCall.ToolName, "write ToolName")
	assert.Equal(t, writeFile, writeCall.FilePath, "write FilePath")
}
