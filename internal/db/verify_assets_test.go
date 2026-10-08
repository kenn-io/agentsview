package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
)

func TestVerifyAssets(t *testing.T) {
	for _, location := range []string{"message", "tool", "event", "quoted-code", "example-reference"} {
		t.Run(location, func(t *testing.T) {
			database := testDB(t)
			ctx := t.Context()
			dir := t.TempDir()
			ref, _, err := assets.Put(dir, "image/png", []byte("synthetic image"))
			require.NoError(t, err)
			content := "![image](" + ref + ")"
			toolContent := `[{"type":"agentsview_image","media_type":"image/png","byte_size":15,"image_ref":"` + ref + `"}]`
			message := Message{SessionID: "example", Ordinal: 0, Role: "assistant", Content: "reply"}
			switch location {
			case "message":
				message.Content = content
			case "quoted-code":
				message.Content = "`" + content + "`\n\n```\n" + content + "\n```"
			case "example-reference":
				message.Content = "![example](asset://nested/example)"
			case "tool":
				message.ToolCalls = []ToolCall{{ToolName: "Read", ResultContent: "agent-a:\n" + toolContent}}
			case "event":
				message.ToolCalls = []ToolCall{{ToolName: "Read", ResultEvents: []ToolResultEvent{{Source: "tool_result", Status: "success", Content: toolContent}}}}
			}
			require.NoError(t, database.UpsertSession(ctx, Session{ID: "example", Agent: "claude", Project: "example"}))
			require.NoError(t, database.InsertMessages(ctx, []Message{message}))
			require.NoError(t, database.VerifyAssets(ctx, dir))
			path := filepath.Join(dir, strings.TrimPrefix(ref, "asset://"))
			require.NoError(t, os.WriteFile(path, []byte("corrupt"), 0o600))
			if location != "quoted-code" && location != "example-reference" {
				require.ErrorContains(t, database.VerifyAssets(ctx, dir), "asset")
			}
			require.NoError(t, os.Remove(path))
			if location == "quoted-code" || location == "example-reference" {
				assert.NoError(t, database.VerifyAssets(ctx, dir))
			} else {
				assert.ErrorContains(t, database.VerifyAssets(ctx, dir), "asset")
			}
		})
	}
}
