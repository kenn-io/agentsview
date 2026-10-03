package db

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func nativeArchiveFixture(t *testing.T, sessionID string) Message {
	t.Helper()
	var message Message
	require.NoError(t, json.Unmarshal([]byte(`{"ordinal":0,"role":"assistant","content":"answer","tool_result_text":"output","content_length":77,"source_uuid":"native-answer","content_layout":{"version":1,"blocks":[{"kind":"text","start":0,"end":6,"call_index":0},{"kind":"tool_call","start":0,"end":0,"call_index":0},{"kind":"tool_result","start":0,"end":6,"call_index":0}]},"tool_calls":[{"tool_name":"read","category":"file","rendering":"old"}]}`), &message))
	message.SessionID = sessionID
	return message
}

func TestArchiveNativeCopyRetainsBodiesAndLegacy(t *testing.T) {
	for _, trashed := range []bool{false, true} {
		t.Run(map[bool]string{false: "orphan", true: "trash"}[trashed], func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(root, "source.db")
			source := testDBAtPath(t, sourcePath, "source")
			insertSession(t, source, "native-copy", "project")
			require.NoError(t, source.InsertMessages(t.Context(), []Message{nativeArchiveFixture(t, "native-copy")}))
			_, err := source.getWriter().Exec(t.Context(), `INSERT INTO messages
				(session_id,ordinal,role,content,content_length)
				VALUES ('native-copy',1,'assistant','[Thinking] legacy body',22)`)
			require.NoError(t, err)
			before, err := source.GetAllMessages(t.Context(), "native-copy")
			require.NoError(t, err)
			require.Len(t, before, 2)
			expectedLayout := &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
				{Kind: "text", End: 6}, {Kind: "tool_call"}, {Kind: "tool_result", End: 6},
			}}
			require.Equal(t, expectedLayout, before[0].ContentLayout)
			require.Len(t, before[0].ToolCalls, 1)
			require.Equal(t, "old", before[0].ToolCalls[0].Rendering)
			if trashed {
				require.NoError(t, source.SoftDeleteSession(t.Context(), "native-copy"))
			}
			require.NoError(t, source.Close())
			destination := testDBAtPath(t, filepath.Join(root, "destination.db"), "destination")
			t.Cleanup(func() { require.NoError(t, destination.Close()) })
			if trashed {
				copied, err := destination.CopyTrashedDataFrom(sourcePath)
				require.NoError(t, err)
				assert.Equal(t, []string{"native-copy"}, copied)
			} else {
				copied, err := destination.CopyOrphanedDataFrom(sourcePath)
				require.NoError(t, err)
				assert.Equal(t, 1, copied)
			}
			stored, err := destination.GetAllMessages(t.Context(), "native-copy")
			require.NoError(t, err)
			require.Len(t, stored, 2)
			assert.Equal(t, "answer", stored[0].Content)
			assert.Equal(t, "native-answer", stored[0].SourceUUID)
			assert.Equal(t, 77, stored[0].ContentLength)
			assert.Equal(t, "output", stored[0].ToolResultText)
			assert.Equal(t, expectedLayout, stored[0].ContentLayout)
			require.Len(t, stored[0].ToolCalls, 1)
			assert.Equal(t, "old", stored[0].ToolCalls[0].Rendering)
			assert.Nil(t, stored[1].ContentLayout)
			assert.Equal(t, "[Thinking] legacy body", stored[1].Content)
		})
	}
}

func TestArchiveNativeFingerprintIncludesBodyChanges(t *testing.T) {
	cases := []struct{ name, update string }{
		{"output", `UPDATE messages SET tool_result_text='newout' WHERE session_id='native-fingerprint'`},
		{"layout", `UPDATE messages SET content_layout='{"version":1,"blocks":[{"kind":"tool_result","start":0,"end":6,"call_index":0},{"kind":"text","start":0,"end":6,"call_index":0},{"kind":"tool_call","start":0,"end":0,"call_index":0}]}' WHERE session_id='native-fingerprint'`},
		{"rendering", `UPDATE tool_calls SET rendering='new' WHERE session_id='native-fingerprint'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "native-fingerprint", "project")
			require.NoError(t, d.InsertMessages(t.Context(), []Message{nativeArchiveFixture(t, "native-fingerprint")}))
			fingerprint := d.MessageFlagsFingerprint
			if tc.name == "rendering" {
				fingerprint = d.ToolCallParseDiffFingerprint
			}
			before, err := fingerprint(t.Context(), "native-fingerprint")
			require.NoError(t, err)
			batchBefore, err := d.MessageFlagsFingerprints(t.Context(), []string{"native-fingerprint"})
			require.NoError(t, err)
			_, err = d.getWriter().Exec(t.Context(), tc.update)
			require.NoError(t, err)
			after, err := fingerprint(t.Context(), "native-fingerprint")
			require.NoError(t, err)
			assert.NotEqual(t, before, after)
			if tc.name != "rendering" {
				batchAfter, err := d.MessageFlagsFingerprints(t.Context(), []string{"native-fingerprint"})
				require.NoError(t, err)
				assert.NotEqual(t, batchBefore["native-fingerprint"], batchAfter["native-fingerprint"])
				assert.Equal(t, after, batchAfter["native-fingerprint"])
			}
		})
	}
}
