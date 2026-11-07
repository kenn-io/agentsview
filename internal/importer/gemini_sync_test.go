package importer

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func init() {
	// Tests use fake fetches; the real pacing would only slow them down.
	geminiDetailGap = 0
}

// The gemini_web fixtures are Gemini web app responses captured on
// 2026-10-21 with every string replaced and timestamps shifted; their array
// layout is the real one.
func geminiFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "gemini_web", name))
	require.NoError(t, err)
	return data
}

// geminiRPCBody frames payload the way batchexecute does.
func geminiRPCBody(t *testing.T, rpc string, payload any) []byte {
	t.Helper()
	inner, err := json.Marshal(payload)
	require.NoError(t, err)
	chunk, err := json.Marshal([]any{
		[]any{"wrb.fr", rpc, string(inner), nil, nil, nil, "generic"},
		[]any{"di", 120},
		[]any{"af.httprm", 120, "0", 31},
	})
	require.NoError(t, err)
	return []byte(")]}'\n\n" + "999\n" + string(chunk) + "\n25\n" + `[["e",4,null,null,1330]]` + "\n")
}

const geminiTestChat = "c_00000000000000b1"

// geminiSyncTurn builds a turn in the web app's layout: index 1 names the
// previous turn, and the reply's index 3 selects the shown draft.
func geminiSyncTurn(response, prompt, reply string, seconds int64) []any {
	var drafts []any
	if reply != "" {
		drafts = []any{[]any{"rc_" + response, []any{reply}, nil, nil, nil, nil, nil, nil, []any{2}, "en"}}
	}
	return []any{
		[]any{geminiTestChat, "r_" + response},
		nil,
		[]any{[]any{prompt, nil, nil, nil, []any{[]any{}}}, 2, nil, 1, "s", nil, nil, nil, false},
		[]any{drafts, nil, nil, "rc_" + response, nil, nil, nil, nil, "s", true},
		[]any{seconds, 0},
	}
}

type geminiFake struct {
	t       *testing.T
	listAt  int64
	turns   []any // newest first
	details int
	status  map[string]int
	pages   map[string][]byte
}

func (f *geminiFake) fetch(_ context.Context, path string) (ClaudeAIResponse, error) {
	if status, ok := f.status[path]; ok {
		return ClaudeAIResponse{Status: status, RetryAfter: "120"}, nil
	}
	if page, ok := f.pages[path]; ok {
		return ClaudeAIResponse{Status: 200, Body: page}, nil
	}
	switch path {
	case geminiRequest(geminiListRequest, "", ""):
		entry := []any{geminiTestChat, "Trip plan", nil, nil, nil, []any{f.listAt, 0}}
		return ClaudeAIResponse{Status: 200, Body: geminiRPCBody(f.t, geminiListRPC, []any{nil, nil, []any{entry}})}, nil
	case geminiRequest(geminiDetailRequest, geminiTestChat, ""):
		f.details++
		return ClaudeAIResponse{Status: 200, Body: geminiRPCBody(f.t, geminiDetailRPC, []any{f.turns, nil, nil, []any{}})}, nil
	}
	require.Failf(f.t, "unexpected Gemini request path", "%s", path)
	return ClaudeAIResponse{}, nil
}

func TestGeminiRPCPayload(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		rpc     string
		want    string
		wantErr string
	}{
		{name: "captured list", body: geminiFixture(t, "list1.txt"), rpc: geminiListRPC, want: `[null,"cursor+1/",`},
		{name: "captured empty list", body: geminiFixture(t, "list-pinned.txt"), rpc: geminiListRPC, want: `[]`},
		{name: "captured detail", body: geminiFixture(t, "detail-full.txt"), rpc: geminiDetailRPC, want: `[[[["c_`},
		{name: "other RPC", body: geminiFixture(t, "list1.txt"), rpc: geminiDetailRPC, wantErr: errGeminiFormat.Error()},
		{name: "refused call", body: []byte(")]}'\n\n40\n" + `[["wrb.fr","MaZiqc",null,null,null,[3],"generic"]]`), rpc: geminiListRPC, wantErr: "the MaZiqc request was rejected by Gemini"},
		{name: "HTML page", body: []byte("<html>Sign in</html>"), rpc: geminiListRPC, wantErr: errGeminiFormat.Error()},
		{name: "invalid payload", body: []byte(")]}'\n" + `[["wrb.fr","MaZiqc","[1,",null]]`), rpc: geminiListRPC, wantErr: errGeminiFormat.Error()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := geminiRPCPayload(tc.body, tc.rpc)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(string(payload), tc.want), "payload starts %.40s", payload)
		})
	}
}

// TestSyncGeminiCapturedPages runs one conversation through Sync using the
// captured list and the captured detail pages, including a real page cursor.
func TestSyncGeminiCapturedPages(t *testing.T) {
	d := testDB(t)
	list, err := geminiRPCPayload(geminiFixture(t, "list1.txt"), geminiListRPC)
	require.NoError(t, err)
	var page []jsontext.Value
	require.NoError(t, json.Unmarshal(list, &page))
	var items []jsontext.Value
	require.NoError(t, json.Unmarshal(page[2], &items))
	const chat = "c_000000000000a002" // the captured detail pages belong to this chat
	var keep []jsontext.Value
	for _, item := range items {
		if strings.Contains(string(item), `"`+chat+`"`) {
			keep = append(keep, item)
		}
	}
	require.Len(t, keep, 1)

	var paths []string
	fetch := func(_ context.Context, path string) (ClaudeAIResponse, error) {
		paths = append(paths, path)
		switch path {
		case geminiRequest(geminiListRequest, "", ""):
			return ClaudeAIResponse{Status: 200, Body: geminiRPCBody(t, geminiListRPC, []any{nil, "cursor+1/", keep})}, nil
		case geminiRequest(geminiListRequest, "", "cursor+1/"):
			return ClaudeAIResponse{Status: 200, Body: geminiFixture(t, "list-pinned.txt")}, nil
		case geminiRequest(geminiDetailRequest, chat, ""):
			return ClaudeAIResponse{Status: 200, Body: geminiFixture(t, "detail-p1.txt")}, nil
		case geminiRequest(geminiDetailRequest, chat, "cursor+43/"):
			return ClaudeAIResponse{Status: 200, Body: geminiFixture(t, "detail-p2.txt")}, nil
		}
		require.Failf(t, "unexpected Gemini request path", "%s", path)
		return ClaudeAIResponse{}, nil
	}
	stats, err := SyncGemini(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Zero(t, stats.Errors)
	assert.Contains(t, paths, "/_/BardChatUi/data/batchexecute?rpcids=hNvQHb&chat=c_000000000000a002&cursor=cursor%2B43%2F",
		"cursors keep + and / through query escaping")

	id := "gemini-apps:" + chat
	messages, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	// The paged transcript matches the one-page capture of the same chat.
	full, err := geminiRPCPayload(geminiFixture(t, "detail-full.txt"), geminiDetailRPC)
	require.NoError(t, err)
	var fullPage []jsontext.Value
	require.NoError(t, json.Unmarshal(full, &fullPage))
	assembled, err := json.Marshal(parser.GeminiAppsWebConversation{ID: chat, Turns: []jsontext.Value{fullPage[0]}})
	require.NoError(t, err)
	want, err := parser.ParseGeminiAppsWebConversation(assembled)
	require.NoError(t, err)
	require.Len(t, messages, 4)
	require.Len(t, want.Messages, 4)
	for i, m := range messages {
		assert.Equal(t, want.Messages[i].Content, m.Content)
		assert.Equal(t, string(want.Messages[i].Role), m.Role)
	}
	assert.Equal(t, []string{"user", "assistant", "user", "assistant"}, []string{messages[0].Role, messages[1].Role, messages[2].Role, messages[3].Role})
	session, err := d.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, string(parser.AgentGeminiApps), session.Agent)
	require.NotNil(t, session.EndedAt)
	assert.Equal(t, "2023-08-18T07:03:44.26581Z", *session.EndedAt, "the list update time ends the session")
}

func TestSyncGeminiIncrementalAndHistory(t *testing.T) {
	d := testDB(t)
	id := "gemini-apps:" + geminiTestChat
	fake := &geminiFake{t: t}
	sync := func(listAt int64, turns ...[]any) ImportStats {
		t.Helper()
		fake.listAt = listAt
		fake.turns = nil
		for _, turn := range slices.Backward(turns) { // the web app sends newest first
			fake.turns = append(fake.turns, turn)
		}
		stats, err := SyncGemini(t.Context(), d, fake.fetch, nil)
		require.NoError(t, err)
		assert.Zero(t, stats.Errors)
		return stats
	}
	contents := func() []string {
		t.Helper()
		messages, err := d.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		return messageContents(messages)
	}
	const t0 = 1_790_000_000
	first := geminiSyncTurn("1", "Question", "Answer", t0)

	assert.Equal(t, 1, sync(t0, first).Imported)
	assert.Equal(t, []string{"Question", "Answer"}, contents())

	assert.Equal(t, 1, sync(t0, first).Skipped)
	assert.Equal(t, 1, fake.details, "an unchanged list time skips the detail request")

	// A reply cut off by an earlier Sync is completed in place.
	partial := geminiSyncTurn("2", "Follow up", "Second ans", t0+60)
	assert.Equal(t, 1, sync(t0+60, first, partial).Updated)
	assert.Equal(t, 1, sync(t0+90, first, geminiSyncTurn("2", "Follow up", "Second answer", t0+60)).Updated)
	assert.Equal(t, []string{"Question", "Answer", "Follow up", "Second answer"}, contents())
	assert.Empty(t, replacedCopies(t, d, id), "appends and completed text make no copy")

	// A regenerated first reply rewrites history, so the old version is kept.
	regenerated := geminiSyncTurn("1", "Question", "Better answer", t0+120)
	second := geminiSyncTurn("2", "Follow up", "Second answer", t0+60)
	assert.Equal(t, 1, sync(t0+120, regenerated, second).Updated)
	assert.Equal(t, []string{"Question", "Better answer", "Follow up", "Second answer"}, contents())
	copies := replacedCopies(t, d, id)
	require.Len(t, copies, 1)
	saved, err := d.GetAllMessages(t.Context(), copies[0].ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Answer", "Follow up", "Second answer"}, messageContents(saved))

	// A shorter transcript replaces the archive and keeps the longer one.
	assert.Equal(t, 1, sync(t0+180, regenerated).Updated)
	assert.Equal(t, []string{"Question", "Better answer"}, contents())
	assert.Len(t, replacedCopies(t, d, id), 2)
	assert.Equal(t, 1, sync(t0+180, regenerated).Skipped)
}

func TestSyncGeminiRefetchesReplyInProgress(t *testing.T) {
	d := testDB(t)
	fake := &geminiFake{t: t, listAt: 1_790_000_000}
	fake.turns = []any{geminiSyncTurn("1", "Question", "", 1_790_000_000)}
	stats, err := SyncGemini(t.Context(), d, fake.fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)

	// Same list time: a missing reply is not trusted as fresh.
	fake.turns = []any{geminiSyncTurn("1", "Question", "Answer", 1_790_000_000)}
	stats, err = SyncGemini(t.Context(), d, fake.fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	assert.Equal(t, 2, fake.details)
	messages, err := d.GetAllMessages(t.Context(), "gemini-apps:"+geminiTestChat)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))

	stats, err = SyncGemini(t.Context(), d, fake.fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 2, fake.details)
}

func TestSyncGeminiStops(t *testing.T) {
	list := geminiRequest(geminiListRequest, "", "")
	detail := geminiRequest(geminiDetailRequest, geminiTestChat, "")
	tests := []struct {
		name     string
		status   map[string]int
		pages    map[string]func(t *testing.T) []byte
		wantErr  error
		wantText string
	}{
		{name: "signed out", status: map[string]int{list: 401}, wantErr: ErrGeminiAuthRequired},
		{name: "rate limited", status: map[string]int{detail: 429}, wantText: "Gemini rate limited"},
		{name: "unreadable list", pages: map[string]func(*testing.T) []byte{
			list: func(t *testing.T) []byte {
				t.Helper()
				return []byte("<html></html>")
			},
		}, wantErr: errGeminiFormat},
		{name: "repeated list cursor", pages: map[string]func(*testing.T) []byte{
			list: func(t *testing.T) []byte {
				t.Helper()
				return geminiRPCBody(t, geminiListRPC, []any{nil, "same", []any{}})
			},
			geminiRequest(geminiListRequest, "", "same"): func(t *testing.T) []byte {
				t.Helper()
				return geminiRPCBody(t, geminiListRPC, []any{nil, "same", []any{}})
			},
		}, wantText: "repeated its page cursor"},
		{name: "unrecognized turns", pages: map[string]func(*testing.T) []byte{
			detail: func(t *testing.T) []byte {
				t.Helper()
				return geminiRPCBody(t, geminiDetailRPC, []any{[]any{map[string]string{"prompt": "x"}}})
			},
		}, wantErr: errGeminiFormat},
		{name: "unreadable detail", pages: map[string]func(*testing.T) []byte{
			detail: func(t *testing.T) []byte {
				t.Helper()
				return []byte("<html></html>")
			},
		}, wantErr: errGeminiFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &geminiFake{t: t, listAt: 1_790_000_000, status: tc.status, pages: map[string][]byte{}}
			fake.turns = []any{geminiSyncTurn("1", "Question", "Answer", 1_790_000_000)}
			for path, body := range tc.pages {
				fake.pages[path] = body(t)
			}
			stats, err := SyncGemini(t.Context(), testDB(t), fake.fetch, nil)
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

// TestSyncGeminiKeepsPinsInPlace checks that appends and completed replies
// write in place: stored message IDs and pins survive, even on repeated text.
func TestSyncGeminiKeepsPinsInPlace(t *testing.T) {
	d := testDB(t)
	id := "gemini-apps:" + geminiTestChat
	fake := &geminiFake{t: t}
	const t0 = 1_790_000_000
	sync := func(listAt int64, turns ...any) {
		t.Helper()
		fake.listAt, fake.turns = listAt, turns
		stats, err := SyncGemini(t.Context(), d, fake.fetch, nil)
		require.NoError(t, err)
		assert.Zero(t, stats.Errors)
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
	first := geminiSyncTurn("1", "Question", "Answer", t0)
	sync(t0, first)
	before := messages()
	require.Len(t, before, 2)
	_, err := d.PinMessage(t.Context(), id, before[1].ID, new("First answer"))
	require.NoError(t, err)
	// A replacement would then get new, higher IDs instead of reusing these.
	preventChatGPTMessageIDReuse(t, d)

	// The second turn repeats the first one's text while its reply is cut off.
	sync(t0+60, geminiSyncTurn("2", "Question", "Ans", t0+60), first)
	appended := messages()
	require.Len(t, appended, 4)
	assert.Equal(t, ids(before), ids(appended[:2]), "an append keeps stored rows")
	_, err = d.PinMessage(t.Context(), id, appended[3].ID, nil)
	require.NoError(t, err)

	sync(t0+90, geminiSyncTurn("2", "Question", "Answer", t0+60), first)
	after := messages()
	assert.Equal(t, []string{"Question", "Answer", "Question", "Answer"}, messageContents(after))
	assert.Equal(t, ids(appended), ids(after), "a completed reply keeps its row")
	pins, err := d.ListPinnedMessages(t.Context(), id, "")
	require.NoError(t, err)
	require.Len(t, pins, 2)
	byMessage := map[int64]*string{}
	for _, pin := range pins {
		byMessage[pin.MessageID] = pin.Note
	}
	assert.Equal(t, map[int64]*string{after[1].ID: new("First answer"), after[3].ID: nil}, byMessage)
	assert.Empty(t, replacedCopies(t, d, id))
}

// TestSyncGeminiRefetchesAttachmentOnlyTurn covers a newest turn with no
// prompt text, which the parser drops, while its reply is still being written.
func TestSyncGeminiRefetchesAttachmentOnlyTurn(t *testing.T) {
	const t0 = 1_790_000_000
	answered := geminiSyncTurn("1", "Question", "Answer", t0)
	tests := []struct {
		name  string
		older []any
		want  []string
	}{
		{name: "only turn", want: []string{"Answer"}},
		{name: "after an answered turn", older: []any{answered}, want: []string{"Question", "Answer", "Image answer"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			fake := &geminiFake{t: t, listAt: t0 + 60}
			reply := "Answer"
			if tc.older != nil {
				reply = "Image answer"
			}
			fake.turns = append([]any{geminiSyncTurn("2", "", "", t0+60)}, tc.older...)
			for range 2 {
				_, err := SyncGemini(t.Context(), d, fake.fetch, nil)
				require.NoError(t, err)
			}
			assert.Equal(t, 2, fake.details, "a pending turn is not cached as fresh")

			fake.turns[0] = geminiSyncTurn("2", "", reply, t0+60)
			_, err := SyncGemini(t.Context(), d, fake.fetch, nil)
			require.NoError(t, err)
			messages, err := d.GetAllMessages(t.Context(), "gemini-apps:"+geminiTestChat)
			require.NoError(t, err)
			assert.Equal(t, tc.want, messageContents(messages))

			stats, err := SyncGemini(t.Context(), d, fake.fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 3, fake.details)
		})
	}
}

// TestSyncGeminiDraftSwitchKeepsHistory covers choosing another draft whose
// text extends the stored one: the draft IDs differ, so it is a rewrite and
// the previous answer stays in Trash instead of being extended in place.
func TestSyncGeminiDraftSwitchKeepsHistory(t *testing.T) {
	d := testDB(t)
	id := "gemini-apps:" + geminiTestChat
	const t0 = 1_790_000_000
	turn := func(selected string) []any {
		t.Helper()
		turn := geminiSyncTurn("1", "Question", "Answer", t0)
		turn[3] = []any{[]any{
			[]any{"rc_a", []any{"Answer"}},
			[]any{"rc_b", []any{"Answer, with more detail"}},
		}, nil, nil, selected}
		return turn
	}
	fake := &geminiFake{t: t, listAt: t0, turns: []any{turn("rc_a")}}
	_, err := SyncGemini(t.Context(), d, fake.fetch, nil)
	require.NoError(t, err)
	stored, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, stored, 2)
	assert.Equal(t, []string{"r_1", "rc_a"}, []string{stored[0].SourceUUID, stored[1].SourceUUID})

	fake.listAt, fake.turns = t0+60, []any{turn("rc_b")}
	_, err = SyncGemini(t.Context(), d, fake.fetch, nil)
	require.NoError(t, err)
	after, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Answer, with more detail"}, messageContents(after))
	copies := replacedCopies(t, d, id)
	require.Len(t, copies, 1)
	saved, err := d.GetAllMessages(t.Context(), copies[0].ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Answer"}, messageContents(saved))
}

// TestSyncGeminiSameTextDraftSwitchKeepsPins covers draft switches that
// leave the stored rows equal: identical text, and any text under
// usage-only storage. The draft IDs differ, so the pinned version is kept in
// Trash instead of being overwritten.
func TestSyncGeminiSameTextDraftSwitchKeepsPins(t *testing.T) {
	for _, tc := range []struct {
		name       string
		usageOnly  bool
		secondText string
	}{
		{name: "identical text", secondText: "Answer"},
		{name: "usage-only storage", usageOnly: true, secondText: "Another answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			if tc.usageOnly {
				d.SetArchiveContent(config.ArchiveContentUsage)
			}
			id := "gemini-apps:" + geminiTestChat
			const t0 = 1_790_000_000
			turn := func(selected string) []any {
				t.Helper()
				turn := geminiSyncTurn("1", "Question", "Answer", t0)
				turn[3] = []any{[]any{
					[]any{"rc_a", []any{"Answer"}},
					[]any{"rc_b", []any{tc.secondText}},
				}, nil, nil, selected}
				return turn
			}
			fake := &geminiFake{t: t, listAt: t0, turns: []any{turn("rc_a")}}
			_, err := SyncGemini(t.Context(), d, fake.fetch, nil)
			require.NoError(t, err)
			stored, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.NotEmpty(t, stored) // usage-only storage keeps only the reply row
			_, err = d.PinMessage(t.Context(), id, stored[len(stored)-1].ID, new("First draft"))
			require.NoError(t, err)

			fake.listAt, fake.turns = t0+60, []any{turn("rc_b")}
			_, err = SyncGemini(t.Context(), d, fake.fetch, nil)
			require.NoError(t, err)
			after, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, after, len(stored))
			assert.Equal(t, "rc_b", after[len(after)-1].SourceUUID)
			copies := replacedCopies(t, d, id)
			require.Len(t, copies, 1, "the previous draft is kept in Trash")
			pins, err := d.ListPinnedMessages(t.Context(), copies[0].ID, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			if !tc.usageOnly { // usage-only storage keeps no pin notes
				assert.Equal(t, new("First draft"), pins[0].Note)
			}
		})
	}
}
