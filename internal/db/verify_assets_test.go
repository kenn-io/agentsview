package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/fileevidence"
)

func TestVerifyAssets(t *testing.T) {
	for _, location := range []string{"message", "tool", "event", "quoted-code", "example-reference", "unsupported extension", "directory"} {
		t.Run(location, func(t *testing.T) {
			database := testDB(t)
			ctx := t.Context()
			dir := t.TempDir()
			ref, _, err := assets.Put(dir, "image/png", []byte("synthetic image"))
			require.NoError(t, err)
			switch location {
			case "unsupported extension":
				ref = strings.TrimSuffix(ref, ".png") + ".svg"
				require.NoError(t, os.WriteFile(filepath.Join(dir, strings.TrimPrefix(ref, "asset://")), []byte("synthetic image"), 0o600))
			case "directory":
				path := filepath.Join(dir, strings.TrimPrefix(ref, "asset://"))
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0o700))
			}
			content := "![image](" + ref + ")"
			toolContent := `[{"type":"agentsview_image","media_type":"image/png","byte_size":15,"image_ref":"` + ref + `"}]`
			message := Message{SessionID: "example", Ordinal: 0, Role: "assistant", Content: "reply"}
			switch location {
			case "message", "unsupported extension", "directory":
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
			if location == "unsupported extension" || location == "directory" {
				assert.ErrorContains(t, database.VerifyAssets(ctx, dir), "unsupported file type")
				return
			}
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

func TestVerifyAssetsNativeMessageEvidence(t *testing.T) {
	database := testDB(t)
	database.rawReader().SetMaxOpenConns(1)
	dir := t.TempDir()
	database.SetAssetsDir(dir)
	require.NoError(t, database.UpsertSession(t.Context(), Session{ID: "evidence", Agent: "claude"}))
	require.NoError(t, database.InsertMessages(t.Context(), []Message{messageAttachmentEvidence("evidence")}))
	snapshot, err := database.ReadFileEvidenceSnapshot(t.Context(), "evidence", fileevidence.Window{})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 2)
	ref := snapshot.Rows[0].Record.AssetRef
	// This image has no message or tool-result placeholder: only its descriptor owns it.
	require.NotEmpty(t, ref)
	require.NoError(t, database.VerifyAssets(t.Context(), dir))
	path := filepath.Join(dir, strings.TrimPrefix(ref, "asset://"))
	require.NoError(t, os.WriteFile(path, []byte("corrupt"), 0o600))
	require.ErrorContains(t, database.VerifyAssets(t.Context(), dir), "asset")
	require.NoError(t, os.Remove(path))
	require.ErrorContains(t, database.VerifyAssets(t.Context(), dir), "asset")
}

func TestVerifyAssetsBeforeFileEvidenceSchema(t *testing.T) {
	// Keep the archive columns this reader used before native evidence existed.
	conn, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "legacy.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	conn.SetMaxOpenConns(1)
	_, err = conn.ExecContext(t.Context(), `
        CREATE TABLE messages(content TEXT NOT NULL);
        CREATE TABLE tool_calls(result_content TEXT NOT NULL);
        CREATE TABLE tool_result_events(content TEXT NOT NULL);
        INSERT INTO messages VALUES ('archived answer');`)
	require.NoError(t, err)
	database := &DB{}
	database.reader.Store(conn)
	assert.NoError(t, database.VerifyAssets(t.Context(), t.TempDir()))
}
