package importer

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func init() {
	// Tests use fake fetches; the real pacing would only slow them down.
	grokDetailGap = 0
}

// The grok_web fixtures are grok.com responses captured on 2026-10-21 with
// every ID replaced consistently, every text replaced, and times shifted;
// their keys and branch structure are the real ones.
func grokFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "grok_web", name))
	require.NoError(t, err)
	return data
}

// TestSyncGrokCapturedPages runs both captured list pages through Sync and
// reads one chat from the captured branched responses.
func TestSyncGrokCapturedPages(t *testing.T) {
	d := testDB(t)
	const (
		token    = "00000000-0000-4000-8000-000000000005" // list1's nextPageToken
		branched = "00000000-0000-4000-8000-000000000001" // list1's newest chat
	)
	var paths []string
	fetch := func(_ context.Context, path string) (ClaudeAIResponse, error) {
		paths = append(paths, path)
		switch path {
		case grokRequest(grokListRequest, "", ""):
			return ClaudeAIResponse{Status: 200, Body: grokFixture(t, "list1.json")}, nil
		case grokRequest(grokListPageRequest, "", token):
			return ClaudeAIResponse{Status: 200, Body: grokFixture(t, "list2.json")}, nil
		case grokRequest(grokResponsesRequest, branched, ""):
			return ClaudeAIResponse{Status: 200, Body: grokFixture(t, "responses-branched.json")}, nil
		}
		return ClaudeAIResponse{Status: 200, Body: grokFixture(t, "responses-linear.json")}, nil
	}
	stats, err := SyncGrok(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 10, stats.Imported)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, "/rest/app-chat/conversations?pageSize=100&pageToken="+token, paths[len(paths)-6],
		"the second page follows the first page's token")

	id := "grok-web:" + branched
	messages, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	// The newest branch with text: 12 responses, with the regenerated and
	// failed replies and the resent prompt's first copy left out.
	want := []string{
		"Text 11", "Text 13", "Text 130", "Text 145", "Text 266", "Text 267",
		"Text 336", "Text 482", "Text 537", "Text 538", "Text 556", "Text 557",
	}
	assert.Equal(t, want, messageContents(messages))
	for i, m := range messages {
		assert.Equal(t, map[bool]string{true: "user", false: "assistant"}[i%2 == 0], m.Role)
	}
	session, err := d.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, string(parser.AgentGrokWeb), session.Agent)
	assert.Equal(t, "Text 1", *session.DisplayName)
	require.NotNil(t, session.EndedAt)
	assert.Equal(t, "2026-03-04T07:13:23.105Z", *session.EndedAt, "the list update time ends the session")
}

func grokSyncResponse(id, parent, sender, message, at string) map[string]any {
	return map[string]any{
		"responseId": id, "parentResponseId": parent, "sender": sender,
		"message": message, "createTime": at, "model": "grok-4", "partial": false,
	}
}

const grokTestChat = "11111111-1111-4111-8111-111111111111"

type grokFake struct {
	t         *testing.T
	modified  string
	responses []map[string]any
	inflight  []any
	details   int
	status    map[string]int
	pages     map[string][]byte
}

func (f *grokFake) fetch(_ context.Context, path string) (ClaudeAIResponse, error) {
	if status, ok := f.status[path]; ok {
		return ClaudeAIResponse{Status: status, RetryAfter: "120"}, nil
	}
	if page, ok := f.pages[path]; ok {
		return ClaudeAIResponse{Status: 200, Body: page}, nil
	}
	var body any
	switch path {
	case grokRequest(grokListRequest, "", ""):
		body = map[string]any{"conversations": []any{map[string]any{
			"conversationId": grokTestChat, "title": "Trip plan", "modifyTime": f.modified, "starred": false,
		}}}
	case grokRequest(grokResponsesRequest, grokTestChat, ""):
		f.details++
		body = map[string]any{"responses": f.responses, "inflightResponses": f.inflight}
	default:
		require.Failf(f.t, "unexpected Grok request path", "%s", path)
	}
	data, err := json.Marshal(body)
	require.NoError(f.t, err)
	return ClaudeAIResponse{Status: 200, Body: data}, nil
}

func TestSyncGrokIncrementalAndHistory(t *testing.T) {
	d := testDB(t)
	id := "grok-web:" + grokTestChat
	fake := &grokFake{t: t}
	sync := func(modified string, responses ...map[string]any) ImportStats {
		t.Helper()
		fake.modified, fake.responses = modified, responses
		stats, err := SyncGrok(t.Context(), d, fake.fetch, nil)
		require.NoError(t, err)
		assert.Zero(t, stats.Errors)
		return stats
	}
	messages := func() []db.Message {
		t.Helper()
		got, err := d.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		return got
	}
	ids := func(rows []db.Message) []int64 {
		out := make([]int64, len(rows))
		for i, m := range rows {
			out[i] = m.ID
		}
		return out
	}
	q := grokSyncResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z")
	a := grokSyncResponse("r2", "r1", "assistant", "Answer", "2026-10-21T08:00:05Z")

	assert.Equal(t, 1, sync("2026-10-21T08:00:05Z", q, a).Imported)
	assert.Equal(t, 1, sync("2026-10-21T08:00:05Z", q, a).Skipped)
	assert.Equal(t, 1, fake.details, "an unchanged list time skips the detail request")
	before := messages()
	require.Len(t, before, 2)
	assert.Equal(t, []string{"r1", "r2"}, []string{before[0].SourceUUID, before[1].SourceUUID})
	assert.Equal(t, []string{"", "grok-4"}, []string{before[0].Model, before[1].Model})
	_, err := d.PinMessage(t.Context(), id, before[1].ID, new("Keep this answer"))
	require.NoError(t, err)
	// A replacement would then get new, higher IDs instead of reusing these.
	preventChatGPTMessageIDReuse(t, d)

	// A follow-up appends in place: stored rows, their IDs, and pins stay.
	f := grokSyncResponse("r3", "r2", "human", "Follow up", "2026-10-21T08:01:00Z")
	b := grokSyncResponse("r4", "r3", "assistant", "Second answer", "2026-10-21T08:01:05Z")
	assert.Equal(t, 1, sync("2026-10-21T08:01:05Z", q, a, f, b).Updated)
	after := messages()
	assert.Equal(t, []string{"Question", "Answer", "Follow up", "Second answer"}, messageContents(after))
	assert.Equal(t, ids(before), ids(after[:2]))
	pins, err := d.ListPinnedMessages(t.Context(), id, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, before[1].ID, pins[0].MessageID)
	assert.Empty(t, replacedCopies(t, d, id))

	// Regenerating the first answer switches the shown branch, which rewrites
	// history, so the previous version is kept in Trash.
	regenerated := grokSyncResponse("r5", "r1", "assistant", "Better answer", "2026-10-21T08:02:00Z")
	assert.Equal(t, 1, sync("2026-10-21T08:02:00Z", q, a, f, b, regenerated).Updated)
	assert.Equal(t, []string{"Question", "Better answer"}, messageContents(messages()))
	copies := replacedCopies(t, d, id)
	require.Len(t, copies, 1)
	saved, err := d.GetAllMessages(t.Context(), copies[0].ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Answer", "Follow up", "Second answer"}, messageContents(saved))

	// The same text under a different response ID is a branch switch too.
	resent := grokSyncResponse("r6", "r1", "assistant", "Better answer", "2026-10-21T08:03:00Z")
	assert.Equal(t, 1, sync("2026-10-21T08:03:00Z", q, a, f, b, regenerated, resent).Updated)
	assert.Len(t, replacedCopies(t, d, id), 2)
}

func TestSyncGrokRefetchesReplyInProgress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   map[string]any
		pending func(f *grokFake)
	}{
		{name: "in-flight response", pending: func(f *grokFake) { f.inflight = []any{map[string]any{"responseId": "r2"}} }},
		{name: "partial reply", reply: map[string]any{"partial": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			const at = "2026-10-21T08:00:00Z"
			fake := &grokFake{t: t, modified: at}
			q := grokSyncResponse("r1", "", "human", "Question", at)
			fake.responses = []map[string]any{q}
			if tc.reply != nil {
				partial := grokSyncResponse("r2", "r1", "assistant", "Ans", at)
				partial["partial"] = true
				fake.responses = append(fake.responses, partial)
			}
			if tc.pending != nil {
				tc.pending(fake)
			}
			for range 2 {
				_, err := SyncGrok(t.Context(), d, fake.fetch, nil)
				require.NoError(t, err)
			}
			assert.Equal(t, 2, fake.details, "a reply in progress is not cached as fresh")

			fake.inflight = nil
			fake.responses = []map[string]any{q, grokSyncResponse("r2", "r1", "assistant", "Answer", at)}
			_, err := SyncGrok(t.Context(), d, fake.fetch, nil)
			require.NoError(t, err)
			messages, err := d.GetAllMessages(t.Context(), "grok-web:"+grokTestChat)
			require.NoError(t, err)
			assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))

			stats, err := SyncGrok(t.Context(), d, fake.fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 3, fake.details)
		})
	}
}

func TestSyncGrokStops(t *testing.T) {
	list := grokRequest(grokListRequest, "", "")
	detail := grokRequest(grokResponsesRequest, grokTestChat, "")
	const token = "22222222-2222-4222-8222-222222222222"
	page := func(next string) []byte {
		return []byte(`{"conversations":[{"conversationId":"33333333-3333-4333-8333-333333333333","modifyTime":"x"}],"nextPageToken":"` + next + `"}`)
	}
	tests := []struct {
		name     string
		status   map[string]int
		pages    map[string][]byte
		wantErr  error
		wantText string
	}{
		{name: "signed out", status: map[string]int{list: 401}, wantErr: ErrGrokAuthRequired},
		{name: "forbidden", status: map[string]int{list: 403}, wantErr: ErrGrokAuthRequired},
		{name: "rate limited", status: map[string]int{detail: 429}, wantText: "Grok rate limited"},
		{name: "HTML list", pages: map[string][]byte{list: []byte("<html></html>")}, wantErr: errGrokFormat},
		{name: "list without conversations", pages: map[string][]byte{list: []byte(`{"error":"x"}`)}, wantErr: errGrokFormat},
		{name: "repeated page token", pages: map[string][]byte{
			list: page(token), grokRequest(grokListPageRequest, "", token): page(token),
		}, wantText: "repeated its page token"},
		{name: "unrecognized page token", pages: map[string][]byte{list: page("a&b")}, wantErr: errGrokFormat},
		{name: "HTML responses", pages: map[string][]byte{detail: []byte("<html></html>")}, wantErr: errGrokFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &grokFake{t: t, modified: "2026-10-21T08:00:00Z", status: tc.status, pages: tc.pages}
			fake.responses = []map[string]any{grokSyncResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z")}
			stats, err := SyncGrok(t.Context(), testDB(t), fake.fetch, nil)
			require.Error(t, err)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
			if tc.wantText != "" {
				assert.Contains(t, err.Error(), tc.wantText)
			}
			assert.Zero(t, stats.Imported)
		})
	}
}
