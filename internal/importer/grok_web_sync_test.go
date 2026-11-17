package importer

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func init() {
	// Tests use fake fetches; the real pacing would only slow them down.
	grokDetailGap = 0
}

// The grok_web fixtures are grok.com responses captured on 2026-10-21 with
// every ID replaced consistently, every text replaced, and times shifted;
// their keys are the real ones. responses-branched.json keeps the captured
// branch points but not the whole chat: two linear prompt-and-reply pairs
// were cut (the next prompt now follows the reply before them), and each
// array keeps only its first entry.
func grokFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "grok_web", name))
	require.NoError(t, err)
	return data
}

// grokTestOptions fixes Sync's clock an hour after the list times the tests
// use, so a reply still being written falls inside the pending window.
var grokTestOptions = BrowserSyncOptions{Now: func() time.Time { return time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC) }}

// TestSyncGrokCapturedPages runs both captured list pages through Sync and
// reads one chat from the captured branched responses.
func TestSyncGrokCapturedPages(t *testing.T) {
	d := testDB(t)
	const (
		token    = "00000000-0000-4000-8000-000000000005" // list1's nextPageToken
		branched = "00000000-0000-4000-8000-000000000001" // list1's newest chat
	)
	// An hour after the branched chat's list time, so its replies are still
	// inside the pending window and the second Sync tests the pending rule
	// rather than the window.
	opts := BrowserSyncOptions{Now: func() time.Time { return time.Date(2026, 3, 4, 8, 13, 23, 0, time.UTC) }}
	var lists []string
	details := 0
	fetch := func(_ context.Context, path string) (BrowserResponse, error) {
		switch path {
		case "/rest/app-chat/conversations?pageSize=100":
			lists = append(lists, path)
			return BrowserResponse{Status: 200, Body: grokFixture(t, "list1.json")}, nil
		case "/rest/app-chat/conversations?pageSize=100&pageToken=" + token:
			lists = append(lists, path)
			return BrowserResponse{Status: 200, Body: grokFixture(t, "list2.json")}, nil
		case "/rest/app-chat/conversations/" + branched + "/responses":
			details++
			return BrowserResponse{Status: 200, Body: grokFixture(t, "responses-branched.json")}, nil
		}
		details++
		return BrowserResponse{Status: 200, Body: grokFixture(t, "responses-linear.json")}, nil
	}
	stats, err := syncGrokWeb(t.Context(), d, fetch, opts, nil)
	require.NoError(t, err)
	assert.Equal(t, 10, stats.Imported)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, []string{
		"/rest/app-chat/conversations?pageSize=100",
		"/rest/app-chat/conversations?pageSize=100&pageToken=" + token,
	}, lists, "the second page follows the first page's token, and the last page has none")

	id := "grok-web:" + branched
	messages, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	// The newest branch with text. Left out: the first copy of the resent
	// prompt (Text 130) with its cut-off and failed replies, and the reply
	// the user did not prefer (Text 337).
	assert.Equal(t, []string{
		"Text 11", "Text 13", "Text 130", "Text 145", "Text 336", "Text 482", "Text 537", "Text 538",
	}, messageContents(messages))
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

	// The cut-off reply sits on a branch the web app no longer shows, so the
	// chat is not pending and the next Sync fetches nothing.
	stats, err = syncGrokWeb(t.Context(), d, fetch, opts, nil)
	require.NoError(t, err)
	assert.Equal(t, 10, stats.Skipped)
	assert.Equal(t, 10, details)
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

func (f *grokFake) fetch(_ context.Context, path string) (BrowserResponse, error) {
	if status, ok := f.status[path]; ok {
		return BrowserResponse{Status: status, RetryAfter: "120"}, nil
	}
	if page, ok := f.pages[path]; ok {
		return BrowserResponse{Status: 200, Body: page}, nil
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
	return BrowserResponse{Status: 200, Body: data}, nil
}

func TestSyncGrokIncrementalAndHistory(t *testing.T) {
	d := testDB(t)
	id := "grok-web:" + grokTestChat
	fake := &grokFake{t: t}
	sync := func(modified string, responses ...map[string]any) ImportStats {
		t.Helper()
		fake.modified, fake.responses = modified, responses
		stats, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
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
		// Grok has started the reply but written no text yet, and the list
		// update time never moves.
		{name: "empty partial reply", reply: map[string]any{"partial": true, "message": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			const at = "2026-10-21T08:00:00Z"
			fake := &grokFake{t: t, modified: at}
			q := grokSyncResponse("r1", "", "human", "Question", at)
			fake.responses = []map[string]any{q}
			if tc.reply != nil {
				partial := grokSyncResponse("r2", "r1", "assistant", "Ans", at)
				maps.Copy(partial, tc.reply)
				fake.responses = append(fake.responses, partial)
			}
			if tc.pending != nil {
				tc.pending(fake)
			}
			for range 2 {
				_, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
				require.NoError(t, err)
			}
			assert.Equal(t, 2, fake.details, "a reply in progress is not cached as fresh")

			fake.inflight = nil
			fake.responses = []map[string]any{q, grokSyncResponse("r2", "r1", "assistant", "Answer", at)}
			_, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
			require.NoError(t, err)
			messages, err := d.GetAllMessages(t.Context(), "grok-web:"+grokTestChat)
			require.NoError(t, err)
			assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))

			stats, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 3, fake.details)
		})
	}
}

// TestSyncGrokRefetchesRegeneration covers regenerating the newest reply:
// the new reply has no text yet and sits beside the shown one, and the list
// update time does not move when it completes.
func TestSyncGrokRefetchesRegeneration(t *testing.T) {
	d := testDB(t)
	id := "grok-web:" + grokTestChat
	q := grokSyncResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z")
	first := grokSyncResponse("r2", "r1", "assistant", "First answer", "2026-10-21T08:00:05Z")
	regenerating := grokSyncResponse("r3", "r1", "assistant", "", "2026-10-21T08:01:00Z")
	regenerating["partial"] = true
	fake := &grokFake{t: t, modified: "2026-10-21T08:01:00Z", responses: []map[string]any{q, first, regenerating}}
	for range 2 {
		_, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
		require.NoError(t, err)
	}
	assert.Equal(t, 2, fake.details, "a regeneration in progress is not cached as fresh")

	fake.responses = []map[string]any{q, first, grokSyncResponse("r3", "r1", "assistant", "Better answer", "2026-10-21T08:01:00Z")}
	stats, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	messages, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Better answer"}, messageContents(messages))
	copies := replacedCopies(t, d, id)
	require.Len(t, copies, 1)
	saved, err := d.GetAllMessages(t.Context(), copies[0].ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "First answer"}, messageContents(saved))

	stats, err = syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 3, fake.details)
}

// TestSyncGrokPendingWindow checks that a reply in progress keeps a chat
// stale only while the list time is within a day of now, so a reply that
// never completes stops being fetched on every Sync.
func TestSyncGrokPendingWindow(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		modified string
		details  int
	}{
		{name: "updated an hour ago", modified: "2026-10-21T08:00:00Z", details: 2},
		{name: "updated in the future", modified: "2026-10-21T10:00:00Z", details: 2},
		{name: "updated 25 hours ago", modified: "2026-10-20T08:00:00Z", details: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			partial := grokSyncResponse("r2", "r1", "assistant", "", "2026-10-21T08:00:05Z")
			partial["partial"] = true
			fake := &grokFake{t: t, modified: tc.modified, responses: []map[string]any{
				grokSyncResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z"), partial,
			}}
			opts := BrowserSyncOptions{Now: func() time.Time { return now }}
			for range 2 {
				stats, err := syncGrokWeb(t.Context(), d, fake.fetch, opts, nil)
				require.NoError(t, err)
				assert.Zero(t, stats.Errors)
			}
			assert.Equal(t, tc.details, fake.details)
		})
	}
}

func TestSyncGrokStops(t *testing.T) {
	list := grokRequest(grokListRequest, "", "")
	detail := grokRequest(grokResponsesRequest, grokTestChat, "")
	const tokenA, tokenB = "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	page := func(next string) []byte {
		return []byte(`{"conversations":[{"conversationId":"not-a-chat-id","modifyTime":"2026-10-21T08:00:00Z"}],"nextPageToken":"` + next + `"}`)
	}
	tests := []struct {
		name     string
		status   map[string]int
		pages    map[string][]byte
		wantErr  error
		wantText string
	}{
		{name: "signed out", status: map[string]int{list: 401}, wantErr: ErrBrowserAuthRequired, wantText: "Grok sign-in required"},
		{name: "forbidden", status: map[string]int{list: 403}, wantErr: ErrBrowserAuthRequired, wantText: "Grok sign-in required"},
		{name: "forbidden chat", status: map[string]int{detail: 403}, wantErr: ErrBrowserAuthRequired, wantText: "Grok sign-in required"},
		{name: "rate limited", status: map[string]int{detail: 429}, wantText: "Grok rate limited"},
		{name: "HTML list", pages: map[string][]byte{list: []byte("<html></html>")}, wantErr: ErrBrowserFormat},
		{name: "list without conversations", pages: map[string][]byte{list: []byte(`{"error":"x"}`)}, wantErr: ErrBrowserFormat},
		{name: "repeated page token", pages: map[string][]byte{
			list: page(tokenA), grokRequest(grokListPageRequest, "", tokenA): page(tokenA),
		}, wantText: "the Grok chat list repeated a page"},
		{name: "page token cycle", pages: map[string][]byte{
			list: page(tokenA),
			grokRequest(grokListPageRequest, "", tokenA): page(tokenB),
			grokRequest(grokListPageRequest, "", tokenB): page(tokenA),
		}, wantText: "the Grok chat list repeated a page"},
		{name: "unrecognized page token", pages: map[string][]byte{list: page("a&b")}, wantErr: ErrBrowserFormat},
		{name: "HTML responses", pages: map[string][]byte{detail: []byte("<html></html>")}, wantErr: ErrBrowserFormat},
		{name: "responses without a sender", pages: map[string][]byte{
			detail: []byte(`{"responses":[{"responseId":"r1","message":"Question"}]}`),
		}, wantErr: ErrBrowserFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &grokFake{t: t, modified: "2026-10-21T08:00:00Z", status: tc.status, pages: tc.pages}
			fake.responses = []map[string]any{grokSyncResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z")}
			stats, err := syncGrokWeb(t.Context(), testDB(t), fake.fetch, grokTestOptions, nil)
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

// TestSyncGrokSkipsUnreadableListEntries checks that a list entry without a
// valid chat ID, or one that is not a JSON object, counts as an error and
// Sync imports the rest.
func TestSyncGrokSkipsUnreadableListEntries(t *testing.T) {
	fake := &grokFake{t: t, pages: map[string][]byte{
		grokRequest(grokListRequest, "", ""): []byte(`{"conversations":[
			{"conversationId":"../settings","modifyTime":"2026-10-21T08:00:00Z"},
			"44444444-4444-4444-8444-444444444444",
			{"conversationId":"` + grokTestChat + `","title":"Trip plan","modifyTime":"2026-10-21T08:00:00Z"}
		]}`),
	}}
	fake.responses = []map[string]any{
		grokSyncResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z"),
		grokSyncResponse("r2", "r1", "assistant", "Answer", "2026-10-21T08:00:05Z"),
	}
	stats, err := syncGrokWeb(t.Context(), testDB(t), fake.fetch, grokTestOptions, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Equal(t, 2, stats.Errors)
	assert.Equal(t, 1, fake.details)
}

// TestSyncGrokUnreadableListTime checks that a chat whose list update time
// is missing or cannot be read is still synced. With no time, the pending
// rule treats the chat as recently updated, so an unfinished reply is
// fetched again on the next Sync, while a finished chat is imported once and
// then counts as fresh.
func TestSyncGrokUnreadableListTime(t *testing.T) {
	question := grokSyncResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z")
	unfinished := grokSyncResponse("r2", "r1", "assistant", "", "2026-10-21T08:00:05Z")
	unfinished["partial"] = true
	finished := grokSyncResponse("r2", "r1", "assistant", "Answer", "2026-10-21T08:00:05Z")
	for _, modified := range []struct{ name, field string }{
		{name: "missing", field: ``},
		{name: "null", field: `,"modifyTime":null`},
		{name: "text", field: `,"modifyTime":"yesterday"`},
		{name: "number", field: `,"modifyTime":1790000000`},
	} {
		for _, tc := range []struct {
			name         string
			reply        map[string]any
			wantDetails  int
			wantMessages []string
		}{
			{name: "unfinished reply", reply: unfinished, wantDetails: 2, wantMessages: []string{"Question"}},
			{name: "finished reply", reply: finished, wantDetails: 1, wantMessages: []string{"Question", "Answer"}},
		} {
			t.Run(modified.name+"/"+tc.name, func(t *testing.T) {
				d := testDB(t)
				fake := &grokFake{t: t, pages: map[string][]byte{
					grokRequest(grokListRequest, "", ""): []byte(`{"conversations":[` +
						`{"conversationId":"` + grokTestChat + `","title":"Trip plan"` + modified.field + `}]}`),
				}}
				fake.responses = []map[string]any{question, tc.reply}
				stats, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Imported)
				assert.Zero(t, stats.Errors)

				stats, err = syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
				require.NoError(t, err)
				assert.Zero(t, stats.Errors)
				assert.Equal(t, tc.wantDetails, fake.details)
				messages, err := d.GetAllMessages(t.Context(), "grok-web:"+grokTestChat)
				require.NoError(t, err)
				assert.Equal(t, tc.wantMessages, messageContents(messages))
			})
		}
	}
}

// TestSyncGrokLongChatList checks that a list longer than any fixed page
// budget still finishes and imports the chat on its last page.
func TestSyncGrokLongChatList(t *testing.T) {
	const pages = 250
	token := func(page int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", page) }
	fake := &grokFake{t: t, modified: "2026-10-21T08:00:00Z", pages: map[string][]byte{}}
	fake.responses = []map[string]any{grokSyncResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z")}
	other := `{"conversationId":"not-a-chat-id","modifyTime":"2026-10-21T08:00:00Z"}`
	for page := range pages - 1 {
		path := grokRequest(grokListRequest, "", "")
		if page > 0 {
			path = grokRequest(grokListPageRequest, "", token(page))
		}
		fake.pages[path] = []byte(`{"conversations":[` + other + `],"nextPageToken":"` + token(page+1) + `"}`)
	}
	fake.pages[grokRequest(grokListPageRequest, "", token(pages-1))] = []byte(`{"conversations":[` +
		`{"conversationId":"` + grokTestChat + `","modifyTime":"2026-10-21T08:00:00Z"}]}`)
	stats, err := syncGrokWeb(t.Context(), testDB(t), fake.fetch, grokTestOptions, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Equal(t, pages-1, stats.Errors, "each earlier page lists one unreadable entry")
}

// TestSyncGrokUsageOnlyAppendBackfillsModel covers usage-only storage, which
// keeps only reply rows: an append that also fills an earlier reply's model
// must update that reply, not the dropped prompt row at the same index.
func TestSyncGrokUsageOnlyAppendBackfillsModel(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	id := "grok-web:" + grokTestChat
	fake := &grokFake{t: t}
	q1 := grokSyncResponse("r1", "", "human", "Question", "2026-10-21T08:00:00Z")
	a1 := grokSyncResponse("r2", "r1", "assistant", "Answer", "2026-10-21T08:00:05Z")
	a1["model"] = ""
	q2 := grokSyncResponse("r3", "r2", "human", "Follow up", "2026-10-21T08:01:00Z")
	a2 := grokSyncResponse("r4", "r3", "assistant", "Second answer", "2026-10-21T08:01:05Z")
	fake.modified, fake.responses = "2026-10-21T08:01:05Z", []map[string]any{q1, a1, q2, a2}
	_, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
	require.NoError(t, err)
	before, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 2, "usage-only storage keeps the two replies")
	assert.Equal(t, []string{"", "grok-4"}, []string{before[0].Model, before[1].Model})
	preventChatGPTMessageIDReuse(t, d)

	modeled := grokSyncResponse("r2", "r1", "assistant", "Answer", "2026-10-21T08:00:05Z")
	q3 := grokSyncResponse("r5", "r4", "human", "Third question", "2026-10-21T08:02:00Z")
	a3 := grokSyncResponse("r6", "r5", "assistant", "Third answer", "2026-10-21T08:02:05Z")
	fake.modified, fake.responses = "2026-10-21T08:02:05Z", []map[string]any{q1, modeled, q2, a2, q3, a3}
	_, err = syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
	require.NoError(t, err)
	after, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, after, 3)
	assert.Equal(t, []int64{before[0].ID, before[1].ID}, []int64{after[0].ID, after[1].ID}, "the append keeps the stored replies")
	assert.Equal(t, []string{"grok-4", "grok-4", "grok-4"}, []string{after[0].Model, after[1].Model, after[2].Model})
	assert.Equal(t, []string{"r2", "r4", "r6"}, []string{after[0].SourceUUID, after[1].SourceUUID, after[2].SourceUUID})
}

// TestSyncGrokCompletionFillsModel covers a reply that was saved while
// still being written, with no model, and later completes with the same
// response ID, text, and times. The rows compare equal under every storage
// policy, so the model is filled in place.
func TestSyncGrokCompletionFillsModel(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			id := "grok-web:" + grokTestChat
			const at = "2026-10-21T08:00:00Z"
			q := grokSyncResponse("r1", "", "human", "Question", at)
			partial := grokSyncResponse("r2", "r1", "assistant", "Answer", at)
			partial["model"], partial["partial"] = "", true
			fake := &grokFake{t: t, modified: at, responses: []map[string]any{q, partial}}
			_, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
			require.NoError(t, err)
			before, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.NotEmpty(t, before)
			reply := before[len(before)-1]
			assert.Empty(t, reply.Model)
			// A replacement would then get new, higher IDs instead of reusing these.
			preventChatGPTMessageIDReuse(t, d)

			fake.responses = []map[string]any{q, grokSyncResponse("r2", "r1", "assistant", "Answer", at)}
			_, err = syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
			require.NoError(t, err)
			after, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, after, len(before))
			assert.Equal(t, reply.ID, after[len(after)-1].ID, "the reply is updated in place")
			assert.Equal(t, "grok-4", after[len(after)-1].Model)

			stats, err := syncGrokWeb(t.Context(), d, fake.fetch, grokTestOptions, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 2, fake.details)
		})
	}
}
