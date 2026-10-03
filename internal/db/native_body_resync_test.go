package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestArchiveNativeResyncRevision(t *testing.T) {
	for _, change := range []string{"unchanged", "output", "layout", "rendering"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(root, "source.db")
			source := testDBAtPath(t, sourcePath, "source")
			insertSession(t, source, "native-resync", "project")
			require.NoError(t, source.InsertMessages(t.Context(), []Message{nativeArchiveFixture(t, "native-resync")}))
			_, err := source.getWriter().Exec(t.Context(), `UPDATE sessions SET transcript_revision='7' WHERE id='native-resync'`)
			require.NoError(t, err)
			require.NoError(t, source.Close())
			destination := testDBAtPath(t, filepath.Join(root, "destination.db"), "destination")
			t.Cleanup(func() { require.NoError(t, destination.Close()) })
			insertSession(t, destination, "native-resync", "project")
			message := nativeArchiveFixture(t, "native-resync")
			switch change {
			case "output":
				message.ToolResultText = "newout"
			case "layout":
				message.ContentLayout.Blocks[0], message.ContentLayout.Blocks[2] = message.ContentLayout.Blocks[2], message.ContentLayout.Blocks[0]
			case "rendering":
				message.ToolCalls[0].Rendering = "new"
			}
			require.NoError(t, destination.InsertMessages(t.Context(), []Message{message}))
			count, err := destination.CopyOrphanedDataFrom(sourcePath)
			require.NoError(t, err)
			assert.Zero(t, count)
			session, err := destination.GetSession(t.Context(), "native-resync")
			require.NoError(t, err)
			require.NotNil(t, session)
			want := "8"
			if change == "unchanged" {
				want = "7"
			}
			require.NotNil(t, session.TranscriptRevision)
			assert.Equal(t, want, *session.TranscriptRevision)
		})
	}
}

func TestArchiveNativeCopySanitizesEveryBodyField(t *testing.T) {
	for _, version := range []string{"57", "113"} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(root, "source.db")
			source := testDBAtPath(t, sourcePath, "source")
			insertSession(t, source, "native-copy-controls", "project")
			require.NoError(t, source.InsertMessages(t.Context(), []Message{nativeArchiveFixture(t, "native-copy-controls")}))
			_, err := source.getWriter().Exec(t.Context(), `UPDATE messages SET content=?,thinking_text=?,tool_result_text=?,
				content_layout='{"version":1,"blocks":[{"kind":"thinking","start":0,"end":3,"call_index":0},{"kind":"text","start":0,"end":7,"call_index":0},{"kind":"tool_call","start":0,"end":0,"call_index":0},{"kind":"tool_result","start":0,"end":6,"call_index":0}]}'
				WHERE session_id='native-copy-controls'`, "a\x00𐐀b", "p\x00q", "o\x00𐐀")
			require.NoError(t, err)
			_, err = source.getWriter().Exec(t.Context(), `UPDATE tool_calls SET rendering=? WHERE session_id='native-copy-controls'`, "r\x00𐐀")
			require.NoError(t, err)
			_, err = source.getWriter().Exec(t.Context(), "PRAGMA user_version="+version)
			require.NoError(t, err)
			require.NoError(t, source.Close())
			destination := testDBAtPath(t, filepath.Join(root, "destination.db"), "destination")
			t.Cleanup(func() { require.NoError(t, destination.Close()) })
			count, err := destination.CopyOrphanedDataFrom(sourcePath)
			require.NoError(t, err)
			require.Equal(t, 1, count)
			messages, err := destination.GetAllMessages(t.Context(), "native-copy-controls")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "a𐐀b", messages[0].Content)
			assert.Equal(t, "pq", messages[0].ThinkingText)
			assert.Equal(t, "o𐐀", messages[0].ToolResultText)
			assert.Equal(t, 75, messages[0].ContentLength)
			assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
				{Kind: "thinking", End: 2}, {Kind: "text", End: 6}, {Kind: "tool_call"}, {Kind: "tool_result", End: 5},
			}}, messages[0].ContentLayout)
			require.Len(t, messages[0].ToolCalls, 1)
			assert.Equal(t, "r𐐀", messages[0].ToolCalls[0].Rendering)
		})
	}
}
