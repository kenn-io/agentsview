package importer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const chatGPTSyncID = "22222222-2222-4222-8222-222222222222"

func init() {
	// Tests use fake fetches; the real pacing would only slow them down.
	chatGPTDetailGap = 0
}

func chatGPTSyncDetail(updated float64, extraTurn bool, firstQuestion string) string {
	turns := `"u1":{"id":"u1","message":{"id":"u1","author":{"role":"user"},"create_time":1700000000,"content":{"content_type":"text","parts":["` + firstQuestion + `"]}}},` +
		`"a1":{"id":"a1","parent":"u1","message":{"id":"a1","author":{"role":"assistant"},"create_time":1700000001,"content":{"content_type":"text","parts":["Answer"]}}}`
	current := "a1"
	if extraTurn {
		turns += `,"u2":{"id":"u2","parent":"a1","message":{"id":"u2","author":{"role":"user"},"create_time":1700000002,"content":{"content_type":"text","parts":["Follow up"]}}},` +
			`"a2":{"id":"a2","parent":"u2","message":{"id":"a2","author":{"role":"assistant"},"create_time":1700000003,"content":{"content_type":"text","parts":["Second answer"]}}}`
		current = "a2"
	}
	return fmt.Sprintf(`{"id":%q,"title":"Example","create_time":1700000000,"update_time":%g,"current_node":%q,"mapping":{%s}}`, chatGPTSyncID, updated, current, turns)
}

func TestSyncChatGPTAppendFreshnessAndRefusals(t *testing.T) {
	d := testDB(t)
	detail := chatGPTSyncDetail(1, false, "Question")
	details := 0
	syncWith := func(updated float64, body string) ImportStats {
		t.Helper()
		stats, err := SyncChatGPT(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
			switch path {
			case chatGPTRequest(chatGPTListRequest, "", 0):
				return ClaudeAIResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":%g}],"total":1}`, chatGPTSyncID, updated))}, nil
			case chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0):
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(body)}, nil
			default:
				require.FailNow(t, "unexpected ChatGPT request path: %s", path)
				return ClaudeAIResponse{}, nil
			}
		}, nil)
		require.NoError(t, err)
		return stats
	}

	stats := syncWith(1, detail)
	assert.Equal(t, 1, stats.Imported)
	assert.Zero(t, stats.Errors)
	messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))

	stats = syncWith(1, detail)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 1, details, "freshness should skip the detail request")

	detail = chatGPTSyncDetail(2, true, "Question")
	stats = syncWith(2, detail)
	assert.Equal(t, 1, stats.Updated)
	assert.Zero(t, stats.Errors)
	messages, err = d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Answer", "Follow up", "Second answer"}, messageContents(messages))

	stats = syncWith(3, chatGPTSyncDetail(3, true, "Rewritten question"))
	assert.Equal(t, 1, stats.Errors)
	require.Equal(t, []ImportRefusal{{SessionID: "chatgpt:" + chatGPTSyncID, Reason: RefusalDiverged}}, stats.Refusals)
	messages, err = d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Answer", "Follow up", "Second answer"}, messageContents(messages))
	assert.Equal(t, 3, details)
}

func TestSyncChatGPTRejectsInvalidSummaryAndOversizedResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		list string
		want int
	}{
		{name: "invalid UUID", list: `{"items":[{"id":"../../settings","update_time":1}],"total":1}`, want: 0},
		{name: "missing update time", list: fmt.Sprintf(`{"items":[{"id":%q}],"total":1}`, chatGPTSyncID), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := SyncChatGPT(t.Context(), testDB(t), func(context.Context, string) (ClaudeAIResponse, error) {
				return ClaudeAIResponse{Status: 200, Body: []byte(tc.list)}, nil
			}, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Errors)
			assert.Equal(t, tc.want, stats.Imported)
		})
	}

	_, err := fetchChatGPT(t.Context(), func(context.Context, string) (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(strings.Repeat("x", ChatGPTResponseLimit+1))}, nil
	}, "/backend-api/conversations?offset=0&limit=50&order=updated")
	require.ErrorIs(t, err, ErrChatGPTResponseTooLarge)
}

func TestSyncChatGPTSkipsConversationDeletedAfterList(t *testing.T) {
	stats, err := SyncChatGPT(t.Context(), testDB(t), func(_ context.Context, path string) (ClaudeAIResponse, error) {
		if path == chatGPTRequest(chatGPTListRequest, "", 0) {
			return ClaudeAIResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":1}],"total":1}`, chatGPTSyncID))}, nil
		}
		require.Equal(t, chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0), path)
		return ClaudeAIResponse{Status: 404}, nil
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Zero(t, stats.Errors)
	assert.Empty(t, stats.Refusals)
}

func TestSyncChatGPTStopsWhenRateLimited(t *testing.T) {
	d := testDB(t)
	details := 0
	_, err := SyncChatGPT(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
		switch path {
		case chatGPTRequest(chatGPTListRequest, "", 0):
			return ClaudeAIResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":1}],"total":1}`, chatGPTSyncID))}, nil
		case chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0):
			details++
			return ClaudeAIResponse{Status: 429, RetryAfter: "0"}, nil
		default:
			require.FailNow(t, "unexpected ChatGPT request path: %s", path)
			return ClaudeAIResponse{}, nil
		}
	}, nil)
	require.ErrorContains(t, err, "ChatGPT rate limited")
	assert.Equal(t, 5, details, "a 429 retries the same request at most five times, then stops the sync")
	messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	assert.Empty(t, messages)
}
