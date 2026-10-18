package importer

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const chatGPTSyncID = "22222222-2222-4222-8222-222222222222"

func init() {
	// Tests use fake fetches; the real pacing would only slow them down.
	chatGPTDetailGap = 0
}

func chatGPTSyncDetail(updated float64, extraTurn bool, firstQuestion string) string {
	chain := []chatGPTNodeSpec{{"user", "text", firstQuestion}, {"assistant", "text", "Answer"}}
	if extraTurn {
		chain = append(chain, chatGPTNodeSpec{"user", "text", "Follow up"}, chatGPTNodeSpec{"assistant", "text", "Second answer"})
	}
	body := strings.TrimSuffix(strings.TrimPrefix(chatGPTChainConvID(chatGPTSyncID, chain...), "["), "]")
	return strings.Replace(body, `"update_time":1706745710.0`, fmt.Sprintf(`"update_time":%g`, updated), 1)
}

func TestSyncChatGPTAppendFreshnessAndBranchChanges(t *testing.T) {
	d := testDB(t)
	detail := chatGPTSyncDetail(1, false, "Question")
	details := 0
	syncWith := func(updated float64, body string) ImportStats {
		t.Helper()
		stats, err := SyncChatGPT(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
			switch path {
			case chatGPTRequest(chatGPTListRequest, "", 0, true):
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
			case chatGPTRequest(chatGPTListRequest, "", 0, false):
				return ClaudeAIResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":%g}],"total":1}`, chatGPTSyncID, updated))}, nil
			case chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0, false):
				details++
				return ClaudeAIResponse{Status: 200, Body: []byte(body)}, nil
			default:
				require.Failf(t, "unexpected ChatGPT request path", "%s", path)
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
	assert.Equal(t, 1, stats.Updated)
	assert.Zero(t, stats.Errors)
	assert.Empty(t, stats.Refusals)
	messages, err = d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Rewritten question", "Answer", "Follow up", "Second answer"}, messageContents(messages))
	assert.Equal(t, 3, details)
	stats = syncWith(3, chatGPTSyncDetail(3, true, "Rewritten question"))
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 3, details)
	stats = syncWith(4, chatGPTSyncDetail(4, false, "Rewritten question"))
	assert.Equal(t, 1, stats.Skipped)
	assert.Zero(t, stats.Errors)
	stats = syncWith(4, chatGPTSyncDetail(4, false, "Rewritten question"))
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 4, details)
	messages, err = d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Rewritten question", "Answer", "Follow up", "Second answer"}, messageContents(messages))
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
			stats, err := SyncChatGPT(t.Context(), testDB(t), func(_ context.Context, path string) (ClaudeAIResponse, error) {
				if path == chatGPTRequest(chatGPTListRequest, "", 0, true) {
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
				}
				// The item must be rejected before any detail request; a detail fetch means the guard was removed.
				if path != chatGPTRequest(chatGPTListRequest, "", 0, false) {
					require.Failf(t, "unexpected ChatGPT detail request", "%s", path)
				}
				return ClaudeAIResponse{Status: 200, Body: []byte(tc.list)}, nil
			}, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Errors)
			assert.Equal(t, tc.want, stats.Imported)
		})
	}

	_, err := fetchBrowser(t.Context(), func(context.Context, string) (ClaudeAIResponse, error) {
		return ClaudeAIResponse{Status: 200, Body: []byte(strings.Repeat("x", ClaudeAIResponseLimit+1))}, nil
	}, chromehost.ChatGPT, "/backend-api/conversations?offset=0&limit=50&order=updated")
	require.ErrorIs(t, err, ErrChatGPTResponseTooLarge)
}

func TestSyncChatGPTSkipsConversationDeletedAfterList(t *testing.T) {
	stats, err := SyncChatGPT(t.Context(), testDB(t), func(_ context.Context, path string) (ClaudeAIResponse, error) {
		if path == chatGPTRequest(chatGPTListRequest, "", 0, true) {
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
		}
		if path == chatGPTRequest(chatGPTListRequest, "", 0, false) {
			return ClaudeAIResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":1}],"total":1}`, chatGPTSyncID))}, nil
		}
		require.Equal(t, chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0, false), path)
		return ClaudeAIResponse{Status: 404}, nil
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Zero(t, stats.Errors)
	assert.Empty(t, stats.Refusals)
}

func TestSyncChatGPTStopsWhenRateLimited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := testDB(t)
		details := 0
		_, err := SyncChatGPT(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
			switch path {
			case chatGPTRequest(chatGPTListRequest, "", 0, true):
				return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
			case chatGPTRequest(chatGPTListRequest, "", 0, false):
				return ClaudeAIResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":1}],"total":1}`, chatGPTSyncID))}, nil
			case chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0, false):
				details++
				return ClaudeAIResponse{Status: 429, RetryAfter: "0"}, nil
			default:
				require.Failf(t, "unexpected ChatGPT request path", "%s", path)
				return ClaudeAIResponse{}, nil
			}
		}, nil)
		require.ErrorContains(t, err, "ChatGPT rate limited")
		assert.Equal(t, 5, details, "a 429 retries the same request at most five times, then stops the sync")
		messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
		require.NoError(t, err)
		assert.Empty(t, messages)
	})
}

func chatGPTSyncFetch(t *testing.T, listUpdate string, detail string, details *int) func(context.Context, string) (ClaudeAIResponse, error) {
	t.Helper()
	return func(_ context.Context, path string) (ClaudeAIResponse, error) {
		switch path {
		case chatGPTRequest(chatGPTListRequest, "", 0, true):
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
		case chatGPTRequest(chatGPTListRequest, "", 0, false):
			return ClaudeAIResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":%s}],"total":1}`, chatGPTSyncID, listUpdate))}, nil
		case chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0, false):
			*details++
			return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
		default:
			require.Failf(t, "unexpected ChatGPT request path", "%s", path)
			return ClaudeAIResponse{}, nil
		}
	}
}

func TestSyncChatGPTEquivalentTimestampsSkipSecondDetail(t *testing.T) {
	for _, tc := range []struct {
		name, listTime           string
		detailTime               float64
		wantDetails, wantSkipped int
	}{
		{"fractional", `"2025-01-02T03:04:05.678901+00:00"`, 1735787045.678901, 1, 1},
		{"millisecond boundary", `"2025-01-02T03:04:05.679+00:00"`, 1735787045.679, 1, 1},
		{"detail changed", `"2025-01-02T03:04:05.679+00:00"`, 1735787045.681, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			details := 0
			detail := chatGPTSyncDetail(tc.detailTime, false, "Question")
			fetch := chatGPTSyncFetch(t, tc.listTime, detail, &details)
			_, err := SyncChatGPT(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			stats, err := SyncChatGPT(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSkipped, stats.Skipped)
			assert.Equal(t, tc.wantDetails, details)
		})
	}
}

func TestSyncChatGPTRenameWithSameMessagesCountsUpdate(t *testing.T) {
	d := testDB(t)
	details := 0
	_, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1", chatGPTSyncDetail(1, false, "Question"), &details), nil)
	require.NoError(t, err)

	renamed := strings.Replace(chatGPTSyncDetail(2, false, "Question"), `"title":"Tool"`, `"title":"Renamed"`, 1)
	stats, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "2", renamed, &details), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated, "a title-only change is an update")
	assert.Zero(t, stats.Imported)
	stored, err := d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.NotNil(t, stored.SessionName)
	assert.Equal(t, "Renamed", *stored.SessionName)
}

// chatGPTPopulatedList returns a full first page of distinct conversation IDs.
func chatGPTPopulatedList(t *testing.T) string {
	t.Helper()
	items := make([]string, 0, chatGPTPageSize)
	for i := range chatGPTPageSize {
		items = append(items, fmt.Sprintf(`{"id":"%08x-0000-4000-8000-000000000000","update_time":1}`, i+1))
	}
	return fmt.Sprintf(`{"items":[%s],"total":%d}`, strings.Join(items, ","), len(items))
}

func TestSyncChatGPTStopsAfterTwoDetailFailures(t *testing.T) {
	for _, transport := range []bool{false, true} {
		t.Run(fmt.Sprintf("transport=%t", transport), func(t *testing.T) {
			details := 0
			stats, err := SyncChatGPT(t.Context(), testDB(t), func(_ context.Context, path string) (ClaudeAIResponse, error) {
				if path == chatGPTRequest(chatGPTListRequest, "", 0, true) {
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
				}
				if path == chatGPTRequest(chatGPTListRequest, "", 0, false) {
					return ClaudeAIResponse{Status: 200, Body: []byte(chatGPTPopulatedList(t))}, nil
				}
				details++
				require.Equal(t, chatGPTRequest(chatGPTDetailRequest, fmt.Sprintf("%08x-0000-4000-8000-000000000000", details), 0, false), path)
				if transport {
					return ClaudeAIResponse{}, errors.New("browser fetch timed out")
				}
				return ClaudeAIResponse{Status: 403}, nil
			}, nil)
			require.Error(t, err)
			assert.Equal(t, 2, details)
			assert.Equal(t, 2, stats.Errors)
			assert.Len(t, stats.Refusals, 2)
		})
	}
}

func TestSyncChatGPTUnsupportedFormatStopsAndKeepsEarlierWrites(t *testing.T) {
	for _, tc := range []struct {
		name, mapping string
	}{
		{"missing mapping", ""},
		{"null mapping", `,"mapping":null`},
		{"array mapping", `,"mapping":[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			var details []string
			stats, err := SyncChatGPT(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case chatGPTRequest(chatGPTListRequest, "", 0, false):
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[{"id":"` + chatGPTSyncID + `","update_time":1},{"id":"00000002-0000-4000-8000-000000000000","update_time":1},{"id":"00000003-0000-4000-8000-000000000000","update_time":1}],"total":3}`)}, nil
				case chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0, false):
					details = append(details, chatGPTSyncID)
					return ClaudeAIResponse{Status: 200, Body: []byte(chatGPTSyncDetail(1, false, "Question"))}, nil
				case chatGPTRequest(chatGPTDetailRequest, "00000002-0000-4000-8000-000000000000", 0, false):
					details = append(details, "00000002-0000-4000-8000-000000000000")
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"conversation_id":"00000002-0000-4000-8000-000000000000","messages":[{"id":"message-1","author":{"role":"user"},"content":{"content_type":"text","parts":["New format"]}}],"page_info":{"start_cursor":"message-1","end_cursor":"message-1","has_previous_page":true,"has_next_page":false}` + tc.mapping + `}`)}, nil
				default:
					require.Failf(t, "unexpected ChatGPT request path", "%s", path)
					return ClaudeAIResponse{}, nil
				}
			}, nil)
			require.EqualError(t, err, "ChatGPT returned a conversation format this version can't read. Update AgentsView and try again")
			assert.Equal(t, []string{chatGPTSyncID, "00000002-0000-4000-8000-000000000000"}, details)
			assert.Equal(t, 1, stats.Imported)
			assert.Equal(t, 1, stats.Errors)
			messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
			require.NoError(t, err)
			assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))
			stored, err := d.GetSessionFull(t.Context(), "chatgpt:00000002-0000-4000-8000-000000000000")
			require.NoError(t, err)
			assert.Nil(t, stored)
		})
	}
}

func TestSyncChatGPTContinuesAfterOversizedDetails(t *testing.T) {
	d := testDB(t)
	details := 0
	stats, err := SyncChatGPT(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
		if path == chatGPTRequest(chatGPTListRequest, "", 0, true) {
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
		}
		if path == chatGPTRequest(chatGPTListRequest, "", 0, false) {
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[{"id":"00000001-0000-4000-8000-000000000000","update_time":1},{"id":"00000002-0000-4000-8000-000000000000","update_time":1},{"id":"` + chatGPTSyncID + `","update_time":1}],"total":3}`)}, nil
		}
		details++
		if details <= 2 {
			require.Equal(t, chatGPTRequest(chatGPTDetailRequest, fmt.Sprintf("%08x-0000-4000-8000-000000000000", details), 0, false), path)
			return ClaudeAIResponse{Status: 413}, nil
		}
		require.Equal(t, chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0, false), path)
		return ClaudeAIResponse{Status: 200, Body: []byte(chatGPTSyncDetail(1, false, "Question"))}, nil
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.Errors)
	assert.Equal(t, 1, stats.Imported)
	assert.Len(t, stats.Refusals, 2)
	messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))
}

func TestSyncChatGPTLongRetryAfterKeepsEarlierWrites(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := testDB(t)
				rateLimitedRequests := 0
				stats, err := SyncChatGPT(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
					switch path {
					case chatGPTRequest(chatGPTListRequest, "", 0, true):
						return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
					case chatGPTRequest(chatGPTListRequest, "", 0, false):
						return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[{"id":"` + chatGPTSyncID + `","update_time":1},{"id":"00000002-0000-4000-8000-000000000000","update_time":1}],"total":2}`)}, nil
					case chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0, false):
						return ClaudeAIResponse{Status: 200, Body: []byte(chatGPTSyncDetail(1, false, "Question"))}, nil
					case chatGPTRequest(chatGPTDetailRequest, "00000002-0000-4000-8000-000000000000", 0, false):
						rateLimitedRequests++
						return ClaudeAIResponse{Status: status, RetryAfter: "120"}, nil
					default:
						require.Failf(t, "unexpected ChatGPT request path", "%s", path)
						return ClaudeAIResponse{}, nil
					}
				}, nil)
				require.ErrorContains(t, err, "ChatGPT rate limited")
				assert.Equal(t, 1, rateLimitedRequests)
				assert.Equal(t, 1, stats.Imported)
				assert.Equal(t, 1, stats.Errors)
				messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
				require.NoError(t, err)
				assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))
			})
		})
	}
}

func TestSyncChatGPTKeepsZipImageAndAppends(t *testing.T) {
	for _, extend := range []bool{false, true} {
		t.Run(fmt.Sprintf("extend=%t", extend), func(t *testing.T) {
			d := testDB(t)
			dir := t.TempDir()
			export := testjsonl.ChatGPTImageExport()
			export["conversations-000.json"] = strings.ReplaceAll(export["conversations-000.json"], "cg-1", chatGPTSyncID)
			if extend {
				export["conversations-000.json"] = strings.Replace(export["conversations-000.json"], `"asset_pointer":"file-service://file-img1"}]`, `"asset_pointer":"file-service://file-img1"}, " incomplete"]`, 1)
			}
			for name, data := range export {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600))
			}
			stats, err := ImportChatGPT(t.Context(), d, dir, t.TempDir(), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			var conversations []map[string]any
			require.NoError(t, json.Unmarshal([]byte(export["conversations-000.json"]), &conversations))
			conversation := conversations[0]
			if extend {
				mapping := conversation["mapping"].(map[string]any)
				message := mapping["n1"].(map[string]any)["message"].(map[string]any)
				content := message["content"].(map[string]any)
				parts := content["parts"].([]any)
				parts[len(parts)-1] = " incomplete text"
			}
			id := conversation["id"].(string)
			before, err := d.GetAllMessages(t.Context(), "chatgpt:"+id)
			require.NoError(t, err)
			require.NotEmpty(t, before)
			assert.Contains(t, strings.Join(messageContents(before), "\n"), "![image](")
			// Append a user turn to the same export graph; browser parsing has no assets.
			mapping := conversation["mapping"].(map[string]any)
			mapping["follow-up"] = map[string]any{
				"id": "follow-up", "parent": conversation["current_node"],
				"message": map[string]any{
					"id": "follow-up", "author": map[string]any{"role": "user"},
					"content": map[string]any{"content_type": "text", "parts": []string{"Follow up"}},
				},
			}
			conversation["current_node"], conversation["update_time"] = "follow-up", 2
			detail, err := json.Marshal(conversation)
			require.NoError(t, err)
			fetch := func(_ context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case chatGPTRequest(chatGPTListRequest, "", 0, true):
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
				case chatGPTRequest(chatGPTListRequest, "", 0, false):
					return ClaudeAIResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":2}],"total":1}`, id))}, nil
				case chatGPTRequest(chatGPTDetailRequest, id, 0, false):
					return ClaudeAIResponse{Status: 200, Body: detail}, nil
				default:
					require.Failf(t, "unexpected ChatGPT request path", "%s", path)
					return ClaudeAIResponse{}, nil
				}
			}
			stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Updated)
			assert.Zero(t, stats.Errors)
			after, err := d.GetAllMessages(t.Context(), "chatgpt:"+id)
			require.NoError(t, err)
			require.Len(t, after, len(before)+1)
			for i := range before {
				if extend && strings.Contains(before[i].Content, "See this:") {
					assert.Equal(t, before[i].Content+" text", after[i].Content)
				} else {
					assert.Equal(t, before[i].Content, after[i].Content)
				}
				assert.Equal(t, before[i].ID, after[i].ID)
			}
			assert.Equal(t, "Follow up", after[len(before)].Content)
			stored, err := d.GetSessionFull(t.Context(), "chatgpt:"+id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			require.NotNil(t, stored.FirstMessage)
			assert.Equal(t, after[0].Content, *stored.FirstMessage)
			require.NoError(t, d.UpsertProviderStatHash(t.Context(), parser.AgentChatGPT, "chatgpt:"+id, 0))
			detail = []byte(strings.Replace(string(detail), "See this:", "Changed text:", 1))
			stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Updated)
			assert.Empty(t, stats.Refusals)
			unchanged, err := d.GetAllMessages(t.Context(), "chatgpt:"+id)
			require.NoError(t, err)
			assert.Contains(t, unchanged[0].Content, "Changed text:")
		})
	}
}

func TestSyncChatGPTEditedAndShorterHistoriesDoNotStopSync(t *testing.T) {
	d := testDB(t)
	ids := []string{chatGPTSyncID, "00000002-0000-4000-8000-000000000000", "00000003-0000-4000-8000-000000000000"}
	detail := func(id string, extra bool, question string) string {
		return strings.ReplaceAll(chatGPTSyncDetail(2, extra, question), chatGPTSyncID, id)
	}
	for _, id := range ids[:2] {
		body := detail(id, true, "Question")
		parsed, err := parser.ParseChatGPTConversation([]byte(body))
		require.NoError(t, err)
		_, err = upsertChatGPTConversation(t.Context(), d, parsed, nil)
		require.NoError(t, err)
	}
	details := 0
	stats, err := SyncChatGPT(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
		if path == chatGPTRequest(chatGPTListRequest, "", 0, true) {
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
		}
		if path == chatGPTRequest(chatGPTListRequest, "", 0, false) {
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[{"id":"` + ids[0] + `","update_time":2},{"id":"` + ids[1] + `","update_time":2},{"id":"` + ids[2] + `","update_time":2}],"total":3}`)}, nil
		}
		require.Less(t, details, len(ids))
		id := ids[details]
		require.Equal(t, chatGPTRequest(chatGPTDetailRequest, id, 0, false), path)
		question := "Question"
		if details == 0 {
			question = "Rewritten question"
		}
		details++
		return ClaudeAIResponse{Status: 200, Body: []byte(detail(id, details == 1, question))}, nil
	}, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 1, stats.Updated)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 1, stats.Imported)
	assert.Empty(t, stats.Refusals)
}

func TestSyncChatGPTPagesActiveAndArchivedLists(t *testing.T) {
	d := testDB(t)
	var lists []string
	details := 0
	fetch := func(_ context.Context, path string) (ClaudeAIResponse, error) {
		switch path {
		case "/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=false",
			"/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=true":
			lists = append(lists, path)
			// The shared summaries also exercise deduplication if a chat moves between lists.
			body := strings.Replace(chatGPTPopulatedList(t), `"total":50`, `"total":51`, 1)
			return ClaudeAIResponse{Status: 200, Body: []byte(body)}, nil
		case "/backend-api/conversations?offset=50&limit=50&order=updated&is_archived=false":
			lists = append(lists, path)
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[{"id":"11111111-1111-4111-8111-111111111111","update_time":1}],"total":51}`)}, nil
		case "/backend-api/conversations?offset=50&limit=50&order=updated&is_archived=true":
			lists = append(lists, path)
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[{"id":"22222222-2222-4222-8222-222222222222","update_time":1}],"total":51}`)}, nil
		default:
			require.True(t, strings.HasPrefix(path, "/backend-api/conversation/"), "unexpected request %s", path)
			id := strings.TrimPrefix(path, "/backend-api/conversation/")
			require.True(t, isUUID(id), "unexpected request %s", path)
			details++
			return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(chatGPTSyncDetail(1, false, "Question"), chatGPTSyncID, id))}, nil
		}
	}
	stats, err := SyncChatGPT(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 52, stats.Imported)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 52, details)
	assert.Equal(t, []string{
		"/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=false",
		"/backend-api/conversations?offset=50&limit=50&order=updated&is_archived=false",
		"/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=true",
		"/backend-api/conversations?offset=50&limit=50&order=updated&is_archived=true",
	}, lists)
	stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 52, stats.Skipped)
	assert.Equal(t, 52, details)
	assert.Len(t, lists, 8, "both lists are paged on every sync")
}

type chatGPTSyncReadStore struct {
	*db.DB
	fullReads, messageReads int
	failAfterWrite          bool
}

func (s *chatGPTSyncReadStore) GetSessionFull(ctx context.Context, id string) (*db.Session, error) {
	s.fullReads++
	if s.failAfterWrite && s.fullReads == 3 {
		return nil, errors.New("post-write read failed")
	}
	return s.DB.GetSessionFull(ctx, id)
}

func (s *chatGPTSyncReadStore) GetAllMessages(ctx context.Context, id string) ([]db.Message, error) {
	s.messageReads++
	return s.DB.GetAllMessages(ctx, id)
}

func TestSyncChatGPTReadsHistoryOnceAndCountsOnce(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("post-write failure=%t", fail), func(t *testing.T) {
			d := testDB(t)
			parsed, err := parser.ParseChatGPTConversation([]byte(chatGPTSyncDetail(1, false, "Question")))
			require.NoError(t, err)
			_, err = upsertChatGPTConversation(t.Context(), d, parsed, nil)
			require.NoError(t, err)
			store := &chatGPTSyncReadStore{DB: d, failAfterWrite: fail}
			details := 0
			stats, err := SyncChatGPT(t.Context(), store, chatGPTSyncFetch(t, "2", chatGPTSyncDetail(2, true, "Question"), &details), nil)
			if fail {
				require.ErrorContains(t, err, "post-write read failed")
				assert.Equal(t, 1, stats.Errors)
			} else {
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Updated)
			}
			assert.Equal(t, 1, stats.Imported+stats.Updated+stats.Skipped+stats.Errors)
			assert.Equal(t, 1, store.messageReads)
			messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
			require.NoError(t, err)
			assert.Equal(t, []string{"Question", "Answer", "Follow up", "Second answer"}, messageContents(messages))
		})
	}
}
