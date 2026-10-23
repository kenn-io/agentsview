package parser

import (
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const grokTestChat = "11111111-1111-4111-8111-111111111111"

func grokTestResponse(id, parent, sender, message, at string) map[string]any {
	return map[string]any{
		"responseId": id, "parentResponseId": parent, "sender": sender, "message": message,
		"createTime": at, "model": "grok-4", "partial": false, "steps": []any{}, "metadata": map[string]any{},
	}
}

func grokTestConversation(t *testing.T, title, modified string, inflight []any, responses ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"responses": responses, "inflightResponses": inflight})
	require.NoError(t, err)
	data, err := json.Marshal(GrokWebConversation{ID: grokTestChat, Title: title, ModifyTime: modified, Responses: body})
	require.NoError(t, err)
	return data
}

func TestParseGrokWebConversation(t *testing.T) {
	// r2 was regenerated as r3; the failed retry r4 is newer but empty, and
	// the web app shows the newest branch with text.
	data := grokTestConversation(t, " Trip plan ", "2026-10-21T09:00:00.5Z", []any{},
		grokTestResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z"),
		grokTestResponse("r2", "r1", "assistant", "First answer", "2026-10-21T08:00:05Z"),
		grokTestResponse("r3", "r1", "ASSISTANT", "Second answer", "2026-10-21T08:01:00Z"),
		grokTestResponse("r4", "r1", "assistant", "", "2026-10-21T08:02:00Z"),
		grokTestResponse("r5", "r3", "Human", "Follow up", "2026-10-21T08:03:00Z"),
		grokTestResponse("r6", "r5", "model", "Last answer", "2026-10-21T08:04:00Z"),
	)
	result, err := ParseGrokWebConversation(data)
	require.NoError(t, err)

	assert.Equal(t, "grok-web:"+grokTestChat, result.Session.ID)
	assert.Equal(t, AgentGrokWeb, result.Session.Agent)
	assert.Equal(t, "grok.com", result.Session.Project)
	assert.Equal(t, "Trip plan", result.Session.SessionName)
	assert.Equal(t, "Question", result.Session.FirstMessage)
	assert.Equal(t, 4, result.Session.MessageCount)
	assert.Equal(t, 2, result.Session.UserMessageCount)
	assert.True(t, time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC).Equal(result.Session.StartedAt))
	assert.True(t, time.Date(2026, 10, 21, 9, 0, 0, 500_000_000, time.UTC).Equal(result.Session.EndedAt), "the list update time ends the session")

	want := []struct {
		role         RoleType
		content, src string
		model        string
	}{
		{RoleUser, "Question", "r1", ""},
		{RoleAssistant, "Second answer", "r3", "grok-4"},
		{RoleUser, "Follow up", "r5", ""},
		{RoleAssistant, "Last answer", "r6", "grok-4"},
	}
	require.Len(t, result.Messages, len(want))
	for i, w := range want {
		m := result.Messages[i]
		assert.Equal(t, i, m.Ordinal)
		assert.Equal(t, w.role, m.Role)
		assert.Equal(t, w.content, m.Content)
		assert.Equal(t, len(w.content), m.ContentLength)
		assert.Equal(t, w.src, m.SourceUUID)
		assert.Equal(t, w.model, m.Model)
	}
}

func TestParseGrokWebConversationEdges(t *testing.T) {
	const at = "2026-10-21T08:00:00Z"
	tests := []struct {
		name     string
		data     func(t *testing.T) []byte
		wantErr  error
		wantMsgs []string
	}{
		{
			name: "prompt still waiting for a reply",
			data: func(t *testing.T) []byte {
				t.Helper()
				return grokTestConversation(t, "", at, []any{map[string]any{"responseId": "r2"}},
					grokTestResponse("r1", "", "human", "Pending question", at))
			},
			wantMsgs: []string{"Pending question"},
		},
		{
			name: "unreadable times keep zero times",
			data: func(t *testing.T) []byte {
				t.Helper()
				return grokTestConversation(t, "t", "not a time", nil,
					grokTestResponse("r1", "", "human", "Q", "yesterday"),
					grokTestResponse("r2", "r1", "assistant", "A", ""))
			},
			wantMsgs: []string{"Q", "A"},
		},
		{
			name: "a failed last reply keeps the text before it",
			data: func(t *testing.T) []byte {
				t.Helper()
				return grokTestConversation(t, "t", at, nil,
					grokTestResponse("r1", "", "human", "Q", at),
					grokTestResponse("r2", "r1", "assistant", "A", at),
					grokTestResponse("r3", "r2", "human", "Again", at),
					grokTestResponse("r4", "r3", "assistant", "", "2026-10-21T08:05:00Z"))
			},
			wantMsgs: []string{"Q", "A", "Again"},
		},
		{
			name: "a parent cycle stops",
			data: func(t *testing.T) []byte {
				t.Helper()
				return grokTestConversation(t, "t", at, nil,
					grokTestResponse("r1", "r3", "human", "Q", at),
					grokTestResponse("r2", "r1", "assistant", "A", at),
					grokTestResponse("r3", "r2", "human", "Again", at),
					grokTestResponse("r4", "r3", "assistant", "Answer", at))
			},
			wantMsgs: []string{"Q", "A", "Again", "Answer"},
		},
		{
			name: "no text yields an empty result",
			data: func(t *testing.T) []byte {
				t.Helper()
				return grokTestConversation(t, "t", at, nil, grokTestResponse("r1", "", "human", " ", at))
			},
		},
		{
			name: "missing responses array",
			data: func(t *testing.T) []byte {
				t.Helper()
				return []byte(`{"id":"` + grokTestChat + `","responses":{"error":"x"}}`)
			},
			wantErr: ErrGrokWebFormat,
		},
		{
			name: "response without a sender",
			data: func(t *testing.T) []byte {
				t.Helper()
				return grokTestConversation(t, "t", at, nil, grokTestResponse("r1", "", "", "Q", at))
			},
			wantErr: ErrGrokWebFormat,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseGrokWebConversation(tc.data(t))
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			var got []string
			for _, m := range result.Messages {
				got = append(got, m.Content)
			}
			assert.Equal(t, tc.wantMsgs, got)
			if tc.wantMsgs == nil {
				assert.Empty(t, result.Session.ID)
			}
		})
	}
}

// TestGrokWebPending covers the rule Sync tests cannot isolate: a partial
// reply on a branch the web app no longer shows does not keep the chat stale.
func TestGrokWebPending(t *testing.T) {
	const at = "2026-10-21T08:00:00Z"
	old := grokTestResponse("r2", "r1", "assistant", "Old", at)
	old["partial"] = true
	data := grokTestConversation(t, "t", "", nil,
		grokTestResponse("r1", "", "human", "Q", at), old,
		grokTestResponse("r3", "r1", "assistant", "New", "2026-10-21T08:01:00Z"))
	assert.False(t, GrokWebPending(data))
}

// TestGrokWebPathLongChat guards the linear branch walk: a quadratic walk
// over 20,000 responses visits about 200 million ancestors.
func TestGrokWebPathLongChat(t *testing.T) {
	const n = 20_000
	responses := make([]grokWebResponse, n)
	for i := range responses {
		responses[i] = grokWebResponse{ResponseID: fmt.Sprint("r", i), Sender: "human", Message: "m", CreateTime: "2026-10-21T08:00:00Z"}
		if i > 0 {
			responses[i].ParentResponseID = responses[i-1].ResponseID
		}
	}
	start := time.Now()
	path := grokWebPath(responses)
	require.Len(t, path, n)
	assert.Equal(t, "r0", path[0].ResponseID)
	assert.Less(t, time.Since(start), 2*time.Second)
}

// TestGrokWebPathCycleOffStart covers a parent loop that does not return to
// the response the walk started from.
func TestGrokWebPathCycleOffStart(t *testing.T) {
	const at = "2026-10-21T08:00:00Z"
	path := grokWebPath([]grokWebResponse{
		{ResponseID: "a", ParentResponseID: "b", Sender: "human", Message: "A", CreateTime: at},
		{ResponseID: "b", ParentResponseID: "a", Sender: "assistant", Message: "B", CreateTime: at},
		{ResponseID: "c", ParentResponseID: "a", Sender: "human", Message: "C", CreateTime: at},
	})
	var ids []string
	for _, r := range path {
		ids = append(ids, r.ResponseID)
	}
	assert.Equal(t, []string{"b", "a", "c"}, ids)
}

func TestValidGrokWebID(t *testing.T) {
	assert.True(t, ValidGrokWebID(grokTestChat))
	assert.False(t, ValidGrokWebID("c_123"))
	assert.False(t, ValidGrokWebID("../"+grokTestChat))
}
