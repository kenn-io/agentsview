package db

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing the archive body fields or treating an explicit empty layout as
// legacy must break these public read/write round trips.
func TestArchiveNativeBodyRoundTrip(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "native-body", "project")
	fixtures := []string{
		`{"session_id":"native-body","ordinal":0,"role":"assistant","content":"first𐐀last","thinking_text":"plan","tool_result_text":"output","content_length":77,"has_thinking":true,"has_tool_use":true,"source_uuid":"native-answer","content_layout":{"version":1,"blocks":[{"kind":"text","start":0,"end":9,"call_index":0},{"kind":"thinking","start":0,"end":4,"call_index":0},{"kind":"tool_call","start":0,"end":0,"call_index":0},{"kind":"text","start":9,"end":13,"call_index":0},{"kind":"tool_result","start":0,"end":6,"call_index":0}]},"tool_calls":[{"tool_name":"read","category":"file","tool_use_id":"call-x","input_json":"{\"path\":\"input\"}"}]}`,
		`{"session_id":"native-body","ordinal":1,"role":"assistant","content":"","content_length":0,"has_context_tokens":true,"context_tokens":10,"content_layout":{"version":1,"blocks":[]}}`,
		`{"session_id":"native-body","ordinal":2,"role":"user","content":"[Thinking] is literal","content_length":21,"content_layout":{"version":1,"blocks":[{"kind":"text","start":0,"end":21,"call_index":0}]}}`,
		`{"session_id":"native-body","ordinal":3,"role":"user","content":"","tool_result_text":"output","content_length":6,"source_uuid":"native-orphan","content_layout":{"version":1,"blocks":[{"kind":"tool_result","start":0,"end":6,"call_index":0}]}}`,
	}
	var messages []Message
	for _, fixture := range fixtures {
		var message Message
		require.NoError(t, json.Unmarshal([]byte(fixture), &message))
		messages = append(messages, message)
	}
	messages[0].ToolCalls[0].Rendering = "[Tool: read input]"
	require.NoError(t, d.InsertMessages(t.Context(), messages))
	stored, err := d.GetMessages(t.Context(), "native-body", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, stored, 4)
	for index, fixture := range fixtures {
		var expected, actual map[string]any
		require.NoError(t, json.Unmarshal([]byte(fixture), &expected))
		encoded, err := json.Marshal(stored[index])
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(encoded, &actual))
		assert.Equal(t, expected["content_layout"], actual["content_layout"])
		assert.Equal(t, expected["content"], actual["content"])
		assert.Equal(t, expected["content_length"], actual["content_length"])
		if expected["tool_result_text"] != nil {
			assert.Equal(t, expected["tool_result_text"], actual["tool_result_text"])
		}
	}
	encoded, err := json.Marshal(stored[0])
	require.NoError(t, err)
	var actual map[string]any
	require.NoError(t, json.Unmarshal(encoded, &actual))
	assert.Equal(t, "output", actual["tool_result_text"])
	assert.Equal(t, "plan", stored[0].ThinkingText)
	assert.Equal(t, "native-answer", stored[0].SourceUUID)
	require.Len(t, stored[0].ToolCalls, 1)
	assert.Equal(t, "[Tool: read input]", stored[0].ToolCalls[0].Rendering)
	call, ok := actual["tool_calls"].([]any)
	require.True(t, ok)
	require.Len(t, call, 1)
	assert.Equal(t, "[Tool: read input]", call[0].(map[string]any)["rendering"])
}

func TestArchiveNativeBodyMutationAdvancesRevision(t *testing.T) {
	const initial = `{"session_id":"native-mutation","ordinal":0,"role":"assistant","content":"answer","thinking_text":"before","tool_result_text":"prior","content_length":77,"has_thinking":true,"has_tool_use":true,"source_uuid":"native-answer","content_layout":{"version":1,"blocks":[{"kind":"thinking","start":0,"end":6,"call_index":0},{"kind":"text","start":0,"end":6,"call_index":0},{"kind":"tool_call","start":0,"end":0,"call_index":0},{"kind":"tool_result","start":0,"end":5,"call_index":0}]},"tool_calls":[{"tool_name":"read","category":"file","tool_use_id":"call-x"}]}`
	cases := []struct{ name, old, replacement, rendering string }{
		{"output", `"tool_result_text":"prior"`, `"tool_result_text":"after"`, "old"},
		{"thinking", `"thinking_text":"before"`, `"thinking_text":"after!"`, "old"},
		{"layout", `{"kind":"thinking","start":0,"end":6,"call_index":0},{"kind":"text","start":0,"end":6,"call_index":0}`, `{"kind":"text","start":0,"end":6,"call_index":0},{"kind":"thinking","start":0,"end":6,"call_index":0}`, "old"},
		{"rendering", "", "", "new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "native-mutation", "project")
			var original Message
			require.NoError(t, json.Unmarshal([]byte(initial), &original))
			original.ToolCalls[0].Rendering = "old"
			require.NoError(t, d.InsertMessages(t.Context(), []Message{original}))
			_, err := d.getWriter().Exec(t.Context(), `UPDATE sessions SET transcript_revision='7' WHERE id='native-mutation'`)
			require.NoError(t, err)
			var replacement Message
			body := initial
			if tc.old != "" {
				body = strings.Replace(initial, tc.old, tc.replacement, 1)
			}
			require.NoError(t, json.Unmarshal([]byte(body), &replacement))
			replacement.ToolCalls[0].Rendering = tc.rendering
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "native-mutation", []Message{replacement}))
			session, err := d.GetSession(t.Context(), "native-mutation")
			require.NoError(t, err)
			require.NotNil(t, session)
			require.NotNil(t, session.TranscriptRevision)
			assert.Equal(t, "8", *session.TranscriptRevision)
			stored, err := d.GetMessages(t.Context(), "native-mutation", 0, 10, true)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			assert.Equal(t, "answer", stored[0].Content)
			assert.Equal(t, 77, stored[0].ContentLength)
			assert.Equal(t, "native-answer", stored[0].SourceUUID)
		})
	}
}

func TestArchiveNativeBodySanitizationRemapsRanges(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "native-sanitize", "project")
	var message Message
	require.NoError(t, json.Unmarshal([]byte(`{"session_id":"native-sanitize","ordinal":0,"role":"assistant","content":"a\u0000𐐀b","thinking_text":"p\u0090q","tool_result_text":"o\u0000𐐀","content_length":70,"has_thinking":true,"content_layout":{"version":1,"blocks":[{"kind":"text","start":0,"end":6,"call_index":0},{"kind":"thinking","start":0,"end":1,"call_index":0},{"kind":"text","start":6,"end":7,"call_index":0},{"kind":"thinking","start":1,"end":4,"call_index":0},{"kind":"tool_result","start":0,"end":6,"call_index":0}]}}`), &message))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message}))
	stored, err := d.GetMessages(t.Context(), "native-sanitize", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	encoded, err := json.Marshal(stored[0])
	require.NoError(t, err)
	var actual map[string]any
	require.NoError(t, json.Unmarshal(encoded, &actual))
	assert.Equal(t, "a𐐀b", stored[0].Content)
	assert.Equal(t, "pq", stored[0].ThinkingText)
	assert.Equal(t, "o𐐀", actual["tool_result_text"])
	var expected map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"blocks":[{"kind":"text","start":0,"end":5,"call_index":0},{"kind":"thinking","start":0,"end":1,"call_index":0},{"kind":"text","start":5,"end":6,"call_index":0},{"kind":"thinking","start":1,"end":2,"call_index":0},{"kind":"tool_result","start":0,"end":5,"call_index":0}]}`), &expected))
	assert.Equal(t, expected, actual["content_layout"])
	assert.Equal(t, 67, stored[0].ContentLength)
}

func TestArchiveLegacyBodyKeepsUnknownLayout(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "legacy-body", "project")
	_, err := d.getWriter().Exec(t.Context(), `INSERT INTO messages
		(session_id, ordinal, role, content, content_length)
		VALUES ('legacy-body', 0, 'assistant', '[Thinking] legacy body', 22)`)
	require.NoError(t, err)
	stored, err := d.GetMessages(t.Context(), "legacy-body", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	encoded, err := json.Marshal(stored[0])
	require.NoError(t, err)
	var actual map[string]any
	require.NoError(t, json.Unmarshal(encoded, &actual))
	assert.Nil(t, actual["content_layout"])
	assert.Equal(t, "[Thinking] legacy body", stored[0].Content)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "legacy-body", stored))
	copied, err := d.GetMessages(t.Context(), "legacy-body", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, copied, 1)
	encoded, err = json.Marshal(copied[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &actual))
	assert.Nil(t, actual["content_layout"])
	assert.Equal(t, "[Thinking] legacy body", copied[0].Content)
}

func TestArchiveNativeBodySanitizationRemovesInvalidUTF8(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "native-invalid", "project")
	var message Message
	require.NoError(t, json.Unmarshal([]byte(`{"session_id":"native-invalid","ordinal":0,"role":"assistant","content_length":77,"content_layout":{"version":1,"blocks":[{"kind":"text","start":0,"end":11,"call_index":0},{"kind":"text","start":11,"end":15,"call_index":0}]}}`), &message))
	message.Content = "first" + string([]byte{0xff, 0}) + "𐐀last"
	require.NoError(t, d.InsertMessages(t.Context(), []Message{message}))
	stored, err := d.GetMessages(t.Context(), "native-invalid", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, "first𐐀last", stored[0].Content)
	assert.Equal(t, 75, stored[0].ContentLength)
	encoded, err := json.Marshal(stored[0])
	require.NoError(t, err)
	var actual map[string]any
	require.NoError(t, json.Unmarshal(encoded, &actual))
	var expected map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"blocks":[{"kind":"text","start":0,"end":9,"call_index":0},{"kind":"text","start":9,"end":13,"call_index":0}]}`), &expected))
	assert.Equal(t, expected, actual["content_layout"])
}

func TestArchiveNewCanonicalPlainBodyGetsLayout(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "native-plain", "project")
	require.NoError(t, d.InsertMessages(t.Context(), []Message{userMsg("native-plain", 0, "[Thinking] is literal")}))
	stored, err := d.GetMessages(t.Context(), "native-plain", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	encoded, err := json.Marshal(stored[0])
	require.NoError(t, err)
	var actual map[string]any
	require.NoError(t, json.Unmarshal(encoded, &actual))
	var expected map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"blocks":[{"kind":"text","start":0,"end":21,"call_index":0}]}`), &expected))
	assert.Equal(t, expected, actual["content_layout"])
	assert.Equal(t, "[Thinking] is literal", stored[0].Content)
	assert.False(t, stored[0].HasThinking)
}
