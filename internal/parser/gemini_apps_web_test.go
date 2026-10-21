package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// geminiTestTurn builds one turn in the web app's positional layout. Fields
// the parser does not read are filled with the kinds of values the web app
// sends there so index drift shows up as a failure.
func geminiTestTurn(response, selected, prompt string, candidates [][2]string, timestamp []int64) []any {
	drafts := make([]any, 0, len(candidates))
	for _, c := range candidates {
		drafts = append(drafts, []any{c[0], []any{c[1]}, nil, nil, nil, nil, nil, nil, []any{1}})
	}
	turn := []any{
		[]any{"c_0123456789abcdef", response},
		// The previous turn's IDs, which must not select this turn's draft.
		[]any{"c_0123456789abcdef", "r_previous", "rc_a"},
		[]any{[]any{prompt, nil, nil, nil, []any{[]any{}}}, 2, nil, 1, "s", nil, nil, nil, false},
		[]any{drafts, nil, nil, selected, nil, nil, nil, nil, "s", true},
	}
	if timestamp != nil {
		turn = append(turn, timestamp)
	}
	return turn
}

func geminiTestConversation(t *testing.T, title string, updated []int64, pages ...[]any) []byte {
	t.Helper()
	conv := map[string]any{"id": "c_0123456789abcdef", "title": title, "updated_at": updated}
	conv["turns"] = pages
	data, err := json.Marshal(conv)
	require.NoError(t, err)
	return data
}

func TestParseGeminiAppsWebConversation(t *testing.T) {
	first := time.Unix(1_790_000_000, 0)
	second := time.Unix(1_790_000_600, 500_000_000)
	updated := []int64{1_790_000_900, 0}

	// Pages are newest first; the second page holds the older turn.
	data := geminiTestConversation(t, "  Trip plan ", updated,
		[]any{geminiTestTurn("r_2", "rc_b", "Second question",
			[][2]string{{"rc_a", "draft a"}, {"rc_b", "selected draft b"}}, []int64{second.Unix(), 500_000_000})},
		[]any{geminiTestTurn("r_1", "", "First question",
			[][2]string{{"rc_x", "only draft"}, {"rc_y", "other draft"}}, []int64{first.Unix(), 0})},
	)
	result, err := ParseGeminiAppsWebConversation(data)
	require.NoError(t, err)

	assert.Equal(t, "gemini-apps:c_0123456789abcdef", result.Session.ID)
	assert.Equal(t, AgentGeminiApps, result.Session.Agent)
	assert.Equal(t, "gemini.google.com", result.Session.Project)
	assert.Equal(t, "Trip plan", result.Session.SessionName)
	assert.Equal(t, "First question", result.Session.FirstMessage)
	assert.Equal(t, 4, result.Session.MessageCount)
	assert.Equal(t, 2, result.Session.UserMessageCount)
	assert.True(t, first.Equal(result.Session.StartedAt))
	assert.True(t, time.Unix(updated[0], 0).Equal(result.Session.EndedAt), "the list update time ends the session")

	require.Len(t, result.Messages, 4)
	want := []struct {
		role    RoleType
		content string
		at      time.Time
	}{
		{RoleUser, "First question", first},
		{RoleAssistant, "only draft", first},
		{RoleUser, "Second question", second},
		{RoleAssistant, "selected draft b", second},
	}
	for i, w := range want {
		m := result.Messages[i]
		assert.Equal(t, i, m.Ordinal)
		assert.Equal(t, w.role, m.Role)
		assert.Equal(t, w.content, m.Content)
		assert.Equal(t, len(w.content), m.ContentLength)
		assert.True(t, w.at.Equal(m.Timestamp), "message %d time", i)
	}
}

func TestParseGeminiAppsWebConversationEdges(t *testing.T) {
	at := []int64{1_790_000_000, 0}
	tests := []struct {
		name     string
		data     func(t *testing.T) []byte
		wantErr  error
		wantMsgs []string
		check    func(t *testing.T, result ParseResult)
	}{
		{
			name: "reply still generating keeps the prompt",
			data: func(t *testing.T) []byte {
				t.Helper()
				return geminiTestConversation(t, "", at, []any{geminiTestTurn("r_1", "", "Pending question", nil, at)})
			},
			wantMsgs: []string{"Pending question"},
			check: func(t *testing.T, result ParseResult) {
				t.Helper()
				assert.Empty(t, result.Session.SessionName, "an untitled chat keeps no name")
			},
		},
		{
			name: "turn without a timestamp keeps a zero time",
			data: func(t *testing.T) []byte {
				t.Helper()
				return geminiTestConversation(t, "t", nil, []any{geminiTestTurn("r_1", "", "Q", [][2]string{{"rc_1", "A"}}, nil)})
			},
			wantMsgs: []string{"Q", "A"},
			check: func(t *testing.T, result ParseResult) {
				t.Helper()
				assert.True(t, result.Messages[0].Timestamp.IsZero())
				assert.True(t, result.Session.EndedAt.IsZero(), "an unreadable list time is not borrowed")
			},
		},
		{
			name: "selection that names no draft falls back to the first",
			data: func(t *testing.T) []byte {
				t.Helper()
				return geminiTestConversation(t, "t", at, []any{geminiTestTurn("r_1", "rc_gone", "Q", [][2]string{{"rc_1", "first"}, {"rc_2", "second"}}, at)})
			},
			wantMsgs: []string{"Q", "first"},
		},
		{
			name: "small integer pairs are not timestamps",
			data: func(t *testing.T) []byte {
				t.Helper()
				turn := geminiTestTurn("r_1", "", "Q", [][2]string{{"rc_1", "A"}}, nil)
				turn = append(turn, []any{1, 2})
				return geminiTestConversation(t, "t", at, []any{turn})
			},
			wantMsgs: []string{"Q", "A"},
			check: func(t *testing.T, result ParseResult) {
				t.Helper()
				assert.True(t, result.Messages[0].Timestamp.IsZero())
			},
		},
		{
			name: "no text yields an empty result",
			data: func(t *testing.T) []byte {
				t.Helper()
				return geminiTestConversation(t, "t", at, []any{geminiTestTurn("r_1", "", " ", nil, at)})
			},
		},
		{
			name: "turn that is not an array",
			data: func(t *testing.T) []byte {
				t.Helper()
				return []byte(`{"id":"c_1","turns":[[{"prompt":"x"}]]}`)
			},
			wantErr: ErrGeminiAppsWebFormat,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseGeminiAppsWebConversation(tc.data(t))
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
			if tc.check != nil {
				tc.check(t, result)
			}
		})
	}
}

func TestGeminiAppsTime(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Time
		ok   bool
	}{
		{`[1790000000,250000000]`, time.Unix(1_790_000_000, 250_000_000), true},
		{`[1790000000,0,"extra"]`, time.Unix(1_790_000_000, 0), true},
		{`[1790000000.5,0]`, time.Time{}, false},
		{`[1790000000,1000000000]`, time.Time{}, false},
		{`[12,3]`, time.Time{}, false},
		{`["1790000000",0]`, time.Time{}, false},
		{`null`, time.Time{}, false},
	} {
		got, ok := GeminiAppsTime(jsontext.Value(tc.raw))
		assert.Equal(t, tc.ok, ok, tc.raw)
		assert.True(t, tc.want.Equal(got), tc.raw)
	}
}

func TestGeminiAppsWebPending(t *testing.T) {
	at := []int64{1_790_000_000, 0}
	answered := geminiTestTurn("r_1", "", "Q", [][2]string{{"rc_1", "A"}}, at)
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{"newest turn answered", geminiTestConversation(t, "t", at, []any{answered}), false},
		{"newest turn has no reply", geminiTestConversation(t, "t", at,
			[]any{geminiTestTurn("r_2", "", "Next", nil, at), answered}), true},
		{"attachment-only turn has no reply", geminiTestConversation(t, "t", at,
			[]any{geminiTestTurn("r_2", "", "", nil, at)}, []any{answered}), true},
		{"blank reply", geminiTestConversation(t, "t", at,
			[]any{geminiTestTurn("r_2", "", "Next", [][2]string{{"rc_2", " "}}, at)}), true},
		{"only an older page answers", geminiTestConversation(t, "t", at, []any{}, []any{answered}), false},
		{"no turns", geminiTestConversation(t, "t", at), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, GeminiAppsWebPending(tc.data))
		})
	}
}
