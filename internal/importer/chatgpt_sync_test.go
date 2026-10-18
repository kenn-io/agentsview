package importer

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/config"
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
	assert.Equal(t, "m-n1", messages[0].SourceUUID, "a first import stores source identities")

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
	assert.Equal(t, 1, stats.Updated)
	assert.Zero(t, stats.Errors)
	stats = syncWith(4, chatGPTSyncDetail(4, false, "Rewritten question"))
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 4, details)
	messages, err = d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Rewritten question", "Answer"}, messageContents(messages))
	assert.Empty(t, replacedCopies(t, d, "chatgpt:"+chatGPTSyncID), "replacements without pin or image loss make no copy")
}

func TestSyncChatGPTMissingCreateTime(t *testing.T) {
	for _, tc := range []struct {
		name          string
		zipImport     bool
		undatedSystem bool
		wantStart     string
	}{
		{name: "preserve ZIP start", zipImport: true, wantStart: "2024-01-31T00:00:00Z"},
		{name: "use first message", wantStart: "2024-02-01T00:00:00Z"},
		{name: "skip undated system message", undatedSystem: true, wantStart: "2024-02-01T00:00:10Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			if tc.zipImport {
				dir := t.TempDir()
				body := strings.Replace(chatGPTSyncDetail(1706745710, false, "Question"), `"create_time":1706745600.0`, `"create_time":1706659200.0`, 1)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "conversations.json"), []byte("["+body+"]"), 0o600))
				stats, err := ImportChatGPT(t.Context(), d, dir, t.TempDir(), nil)
				require.NoError(t, err)
				require.Equal(t, 1, stats.Imported)
				stored, err := d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
				require.NoError(t, err)
				require.NotNil(t, stored)
				require.Equal(t, new(tc.wantStart), stored.StartedAt)
			}
			detail := strings.Replace(chatGPTSyncDetail(1706745711, false, "Question"), `"create_time":1706745600.0`, `"create_time":null`, 1)
			if tc.undatedSystem {
				detail = strings.TrimSuffix(strings.TrimPrefix(chatGPTChainConvID(chatGPTSyncID,
					chatGPTNodeSpec{"system", "text", "Instructions"},
					chatGPTNodeSpec{"user", "text", "Question"},
					chatGPTNodeSpec{"assistant", "text", "Answer"},
				), "["), "]")
				detail = strings.Replace(detail, `"create_time":1706745600.0`, `"create_time":null`, 1)
				detail = strings.Replace(detail, `"create_time":1.7067456e+09,`, "", 1)
			}
			details := 0
			stats, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745711", detail, &details), nil)
			require.NoError(t, err)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, 1, details)
			stored, err := d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
			require.NoError(t, err)
			require.NotNil(t, stored)
			require.NotNil(t, stored.StartedAt)
			assert.Equal(t, tc.wantStart, *stored.StartedAt)
		})
	}
}

func TestSyncChatGPTUsageArchiveShorterBranchRefreshesMetadata(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	details := 0
	detail := strings.TrimSuffix(strings.TrimPrefix(chatGPTChainConvID(chatGPTSyncID,
		chatGPTNodeSpec{"user", "text", "Question"},
		chatGPTNodeSpec{"assistant", "text", "Answer"},
		chatGPTNodeSpec{"user", "text", "Follow up"},
	), "["), "]")
	stats, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745710", detail, &details), nil)
	require.NoError(t, err)
	require.Equal(t, ImportStats{Imported: 1}, stats)
	session, err := d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Equal(t, 3, session.MessageCount)
	require.Equal(t, 2, session.UserMessageCount)
	before, err := d.GetAllMessages(t.Context(), session.ID)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Empty(t, before[0].Model)

	detail = strings.ReplaceAll(chatGPTSyncDetail(1706745711, false, "Question"), `"metadata":{}`, `"metadata":{"model_slug":"gpt-4o"}`)
	fetch := chatGPTSyncFetch(t, "1706745711", detail, &details)
	stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, ImportStats{Updated: 1}, stats)
	session, err = d.GetSessionFull(t.Context(), session.ID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 2, session.MessageCount)
	assert.Equal(t, 1, session.UserMessageCount)
	require.NotNil(t, session.EndedAt)
	assert.Equal(t, "2024-02-01T00:01:51Z", *session.EndedAt)
	after, err := d.GetAllMessages(t.Context(), session.ID)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, "gpt-4o", after[0].Model)
	before[0].Model = "gpt-4o"
	assert.Equal(t, before, after)
	stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, ImportStats{Skipped: 1}, stats)
	assert.Equal(t, 2, details)
}

func TestSyncChatGPTArchiveUpdatesAppendedTurns(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			details := 0
			detail := strings.ReplaceAll(chatGPTSyncDetail(1706745711, false, "Question"), `"metadata":{}`, `"metadata":{"model_slug":"gpt-4o"}`)
			stats, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745711", detail, &details), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			session, err := d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
			require.NoError(t, err)
			require.NotNil(t, session)
			require.Equal(t, 2, session.MessageCount)
			messages, err := d.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			require.NotEmpty(t, messages)
			assert.Equal(t, "gpt-4o", messages[len(messages)-1].Model)

			detail = strings.Replace(chatGPTSyncDetail(1706745712, true, "Question"), `"parts":["Second answer"]},"status":"finished_successfully","metadata":{}`, `"parts":["Second answer"]},"status":"finished_successfully","metadata":{"model_slug":"gpt-5"}`, 1)
			stats, err = SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745712", detail, &details), nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Updated)
			assert.Zero(t, stats.Errors)
			session, err = d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, 4, session.MessageCount)
			assert.Equal(t, 2, session.UserMessageCount)
			require.NotNil(t, session.EndedAt)
			assert.Equal(t, "2024-02-01T00:01:52Z", *session.EndedAt)
			messages, err = d.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			if policy.UsageOnly() {
				assert.Equal(t, []string{"", ""}, messageContents(messages), "usage-only archives retain assistant activity without transcript text")
			} else {
				assert.Equal(t, []string{"Question", "Answer", "Follow up", "Second answer"}, messageContents(messages))
			}
			var models []string
			for _, message := range messages {
				if message.Role == "assistant" {
					models = append(models, message.Model)
				}
			}
			assert.Equal(t, []string{"gpt-4o", "gpt-5"}, models)

			// Regenerating the later reply preserves the earlier reply's stored model.
			detail = strings.Replace(detail, `"parts":["Second answer"]`, `"parts":["Regenerated answer"]`, 1)
			detail = strings.Replace(detail, `"update_time":1.706745712e+09`, `"update_time":1706745712.5`, 1)
			stats, err = SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745712.5", detail, &details), nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Updated)
			assert.Zero(t, stats.Errors)
			messages, err = d.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			models = nil
			for _, message := range messages {
				if message.Role == "assistant" {
					models = append(models, message.Model)
				}
			}
			assert.Equal(t, []string{"gpt-4o", "gpt-5"}, models)
			if !policy.UsageOnly() {
				assert.Equal(t, []string{"Question", "Answer", "Follow up", "Regenerated answer"}, messageContents(messages))
			}

			// A metadata-only refresh with omitted models keeps both stored models.
			detail = strings.ReplaceAll(detail, `"metadata":{"model_slug":"gpt-5"}`, `"metadata":{}`)
			detail = strings.Replace(detail, `"update_time":1706745712.5`, `"update_time":1706745713`, 1)
			detail = strings.Replace(detail, `"title":"Tool"`, `"title":"Renamed"`, 1)
			_, err = SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745713", detail, &details), nil)
			require.NoError(t, err)
			messages, err = d.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			models = nil
			for _, message := range messages {
				if message.Role == "assistant" {
					models = append(models, message.Model)
				}
			}
			assert.Equal(t, []string{"gpt-4o", "gpt-5"}, models)

			stats, err = SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745713", detail, &details), nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 4, details)
		})
	}
}

func TestSyncChatGPTUpdatesRetainedModelAndCachesFreshness(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts} {
		for _, initialModel := range []string{"", "gpt-4o-mini"} {
			t.Run(string(policy)+"/"+initialModel, func(t *testing.T) {
				d := testDB(t)
				d.SetArchiveContent(policy)
				details := 0
				detail := strings.ReplaceAll(chatGPTSyncDetail(1, false, "Question"), `"metadata":{}`, fmt.Sprintf(`"metadata":{"model_slug":%q}`, initialModel))
				_, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1", detail, &details), nil)
				require.NoError(t, err)
				id := "chatgpt:" + chatGPTSyncID
				before, err := d.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, before, 2)
				require.Equal(t, initialModel, before[1].Model)
				_, err = d.PinMessage(t.Context(), id, before[1].ID, new("Keep this answer"))
				require.NoError(t, err)

				detail = strings.ReplaceAll(chatGPTSyncDetail(2, false, "Question"), `"metadata":{}`, `"metadata":{"model_slug":"gpt-4o"}`)
				fetch := chatGPTSyncFetch(t, "2", detail, &details)
				stats, err := SyncChatGPT(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Zero(t, stats.Errors)
				after, err := d.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, after, 2)
				wantModel := initialModel
				if wantModel == "" {
					wantModel = "gpt-4o"
				}
				assert.Equal(t, wantModel, after[1].Model)
				assert.Equal(t, before[1].ID, after[1].ID)
				assert.Equal(t, messageContents(before), messageContents(after))
				pins, err := d.ListPinnedMessages(t.Context(), id, "")
				require.NoError(t, err)
				require.Len(t, pins, 1)
				assert.Equal(t, before[1].ID, pins[0].MessageID)
				assert.Equal(t, new("Keep this answer"), pins[0].Note)

				stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Zero(t, stats.Errors)
				assert.Equal(t, 1, stats.Skipped)
				assert.Equal(t, 2, details, "the third sync makes no detail request")
			})
		}
	}
}

func TestSyncChatGPTUsageArchiveFillsModelWithUnchangedTimestamp(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	detail := chatGPTSyncDetail(1706745710, false, "Question")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "conversations.json"), []byte("["+detail+"]"), 0o600))
	stats, err := ImportChatGPT(t.Context(), d, dir, t.TempDir(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Imported)
	id := "chatgpt:" + chatGPTSyncID
	before, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Empty(t, before[0].Model)

	details := 0
	detail = strings.ReplaceAll(detail, `"metadata":{}`, `"metadata":{"model_slug":"gpt-4o"}`)
	fetch := chatGPTSyncFetch(t, "1706745710", detail, &details)
	stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 1, stats.Updated)
	after, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, "gpt-4o", after[0].Model)
	assert.Equal(t, before[0].ID, after[0].ID)
	assert.Empty(t, after[0].Content)

	stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 1, details, "the next sync fetches no detail")
}

func TestSyncChatGPTRejectsInvalidSummary(t *testing.T) {
	for _, tc := range []struct {
		name string
		list string
	}{
		{name: "invalid UUID", list: `{"items":[{"id":"../../settings","update_time":1}],"total":1}`},
		{name: "missing update time", list: fmt.Sprintf(`{"items":[{"id":%q}],"total":1}`, chatGPTSyncID)},
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
		})
	}
}

func TestFetchBrowserChatGPTOversizedResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response ClaudeAIResponse
	}{
		{name: "body over limit", response: ClaudeAIResponse{Status: 200, Body: []byte(strings.Repeat("x", ClaudeAIResponseLimit+1))}},
		{name: "413 status", response: ClaudeAIResponse{Status: 413}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetchBrowser(t.Context(), func(context.Context, string) (ClaudeAIResponse, error) {
				return tc.response, nil
			}, chromehost.ChatGPT, chatGPTRequest(chatGPTListRequest, "", 0, false))
			require.ErrorIs(t, err, ErrChatGPTResponseTooLarge)
		})
	}
}

func chatGPTSyncFetch(t *testing.T, listUpdate string, detail string, details *int, conversationID ...string) func(context.Context, string) (ClaudeAIResponse, error) {
	t.Helper()
	id := chatGPTSyncID
	if len(conversationID) > 0 {
		id = conversationID[0]
	}
	return func(_ context.Context, path string) (ClaudeAIResponse, error) {
		switch path {
		case chatGPTRequest(chatGPTListRequest, "", 0, true):
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
		case chatGPTRequest(chatGPTListRequest, "", 0, false):
			return ClaudeAIResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":%s}],"total":1,"limit":50,"offset":0}`, id, listUpdate))}, nil
		case chatGPTRequest(chatGPTDetailRequest, id, 0, false):
			*details++
			return ClaudeAIResponse{Status: 200, Body: []byte(detail)}, nil
		default:
			require.Failf(t, "unexpected ChatGPT request path", "%s", path)
			return ClaudeAIResponse{}, nil
		}
	}
}

func chatGPTSyncCapturedDetail(t *testing.T, id string, updated float64) string {
	t.Helper()
	var detail struct {
		Mapping     jsontext.Value `json:"mapping"`
		CurrentNode string         `json:"current_node"`
	}
	require.NoError(t, json.Unmarshal([]byte(chatGPTSyncDetail(updated, false, "Question")), &detail))
	return fmt.Sprintf(`{
		"title":null,
		"create_time":1787016340.016941,
		"update_time":%g,
		"moderation_results":[],
		"plugin_ids":[],
		"conversation_id":%q,
		"conversation_template_id":null,
		"gizmo_id":null,
		"gizmo_type":null,
		"is_archived":false,
		"is_starred":false,
		"safe_urls":[],
		"blocked_urls":[],
		"default_model_slug":null,
		"atlas_mode_enabled":false,
		"conversation_origin":null,
		"is_read_only":false,
		"voice":null,
		"async_status":null,
		"disabled_tool_ids":[],
		"is_temporary_chat":false,
		"is_do_not_remember":false,
		"memory_scope":null,
		"context_scopes":[],
		"sugar_item_id":null,
		"sugar_item_visible":false,
		"pinned_time":null,
		"is_study_mode":false,
		"owner":null,
		"sectioned_conversation":null,
		"mapping":%s,
		"current_node":%q,
		"context_truncation_continuation":null
	}`, updated, id, detail.Mapping, detail.CurrentNode)
}

func TestSyncChatGPTEquivalentTimestampsSkipSecondDetail(t *testing.T) {
	for _, tc := range []struct {
		name, listTime           string
		detailTime               float64
		wantDetails, wantSkipped int
		id, detail               string
	}{
		{"fractional", `"2025-01-02T03:04:05.678901+00:00"`, 1735787045.678901, 1, 1, chatGPTSyncID, ""},
		{"millisecond boundary", `"2025-01-02T03:04:05.679+00:00"`, 1735787045.679, 1, 1, chatGPTSyncID, ""},
		{"detail changed", `"2025-01-02T03:04:05.679+00:00"`, 1735787105.679, 2, 1, chatGPTSyncID, ""},
		{
			name: "captured endpoint skew", listTime: `"2026-08-18T01:25:43.336713Z"`,
			detailTime: 1787016344.016941, wantDetails: 1, wantSkipped: 1,
			id:     "6a83b493-b890-83ea-915b-90410c25ef98",
			detail: chatGPTSyncCapturedDetail(t, "6a83b493-b890-83ea-915b-90410c25ef98", 1787016344.016941),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			details := 0
			detail := tc.detail
			if detail == "" {
				detail = chatGPTSyncDetail(tc.detailTime, false, "Question")
			}
			fetch := chatGPTSyncFetch(t, tc.listTime, detail, &details, tc.id)
			_, err := SyncChatGPT(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			stats, err := SyncChatGPT(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSkipped, stats.Skipped)
			assert.Equal(t, tc.wantDetails, details)
		})
	}
}

func TestSyncChatGPTEmptyConversationFreshness(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
	}{
		{"empty mapping", `"mapping":{}`},
		{"selected null root", `"mapping":{"root":{"id":"root","message":null}},"current_node":"root"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			details := 0
			const laterID = "00000003-0000-4000-8000-000000000000"
			fetch := func(_ context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case chatGPTRequest(chatGPTListRequest, "", 0, true):
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
				case chatGPTRequest(chatGPTListRequest, "", 0, false):
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[{"id":"` + chatGPTSyncID + `","update_time":1},{"id":"` + laterID + `","update_time":1}],"total":2}`)}, nil
				case chatGPTRequest(chatGPTDetailRequest, chatGPTSyncID, 0, false):
					details++
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"id":"` + chatGPTSyncID + `","update_time":1,` + tc.fields + `}`)}, nil
				case chatGPTRequest(chatGPTDetailRequest, laterID, 0, false):
					details++
					return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(chatGPTSyncDetail(1, false, "Question"), chatGPTSyncID, laterID))}, nil
				default:
					require.Failf(t, "unexpected ChatGPT request path", "%s", path)
					return ClaudeAIResponse{}, nil
				}
			}
			stats, err := SyncChatGPT(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 1, stats.Imported)
			assert.Zero(t, stats.Errors)
			assert.Empty(t, stats.Refusals)
			messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+laterID)
			require.NoError(t, err)
			assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))
			stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 2, stats.Skipped)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, 2, details, "the second sync makes no detail requests")
			session, err := d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
			require.NoError(t, err)
			assert.Nil(t, session, "empty chats stay out of the archive")
		})
	}
}

func TestSyncChatGPTRenameWithSameMessagesCountsUpdate(t *testing.T) {
	d := testDB(t)
	details := 0
	_, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745711", chatGPTSyncDetail(1706745711, false, "Question"), &details), nil)
	require.NoError(t, err)

	id := "chatgpt:" + chatGPTSyncID
	require.NoError(t, d.UpdateSessionSignals(t.Context(), id, db.SessionSignalUpdate{
		HealthScore: new(42), QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
	}))
	require.NoError(t, d.ReplaceSessionSecretFindings(t.Context(), id, []db.SecretFinding{{RuleName: "test-secret", Confidence: "high", LocationKind: "message", MessageOrdinal: 0, MatchStart: 0, MatchEnd: 5, RedactedMatch: "[redacted]"}}, 1, "test-rules"))
	findings, err := d.SessionSecretFindings(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, findings, 1)

	renamed := strings.Replace(chatGPTSyncDetail(1706745712, false, "Question"), `"title":"Tool"`, `"title":"Renamed"`, 1)
	stats, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745712", renamed, &details), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated, "a title-only change is an update")
	assert.Zero(t, stats.Imported)
	stored, err := d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.NotNil(t, stored.SessionName)
	assert.Equal(t, "Renamed", *stored.SessionName)
	require.NotNil(t, stored.EndedAt)
	assert.Equal(t, "2024-02-01T00:01:52Z", *stored.EndedAt)
	assert.Equal(t, new(42), stored.HealthScore)
	assert.Equal(t, db.CurrentQualitySignalVersion, stored.QualitySignalVersion)
	afterFindings, err := d.SessionSecretFindings(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, findings, afterFindings)
	stats, err = SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745712", renamed, &details), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Skipped)
	assert.Equal(t, 2, details)
}

func TestSyncChatGPTTimestampOnlyCountsUpdate(t *testing.T) {
	d := testDB(t)
	details := 0
	stats, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745711", chatGPTSyncDetail(1706745711, false, "Question"), &details), nil)
	require.NoError(t, err)
	require.Equal(t, ImportStats{Imported: 1}, stats)
	fetch := chatGPTSyncFetch(t, "1706745712", chatGPTSyncDetail(1706745712, false, "Question"), &details)
	stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, ImportStats{Updated: 1}, stats)
	stored, err := d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, new("2024-02-01T00:01:52Z"), stored.EndedAt)
	stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, ImportStats{Skipped: 1}, stats)
	assert.Equal(t, 2, details, "the third sync makes no detail request")
}

// chatGPTPopulatedList returns a full first page of distinct conversation IDs.
func chatGPTPopulatedList(t *testing.T) string {
	t.Helper()
	items := make([]string, 0, 50)
	for i := range 50 {
		items = append(items, fmt.Sprintf(`{"id":"%08x-0000-4000-8000-000000000000","update_time":1}`, i+1))
	}
	return fmt.Sprintf(`{"items":[%s],"total":%d}`, strings.Join(items, ","), len(items))
}

func TestSyncChatGPTUnsupportedFormatCountsTowardDetailStop(t *testing.T) {
	for _, tc := range []struct {
		name, mapping string
	}{
		{"missing mapping", ""},
		{"null mapping", `,"mapping":null`},
		{"array mapping", `,"mapping":[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			details := 0
			stats, err := SyncChatGPT(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
				if path == chatGPTRequest(chatGPTListRequest, "", 0, true) {
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
				}
				if path == chatGPTRequest(chatGPTListRequest, "", 0, false) {
					return ClaudeAIResponse{Status: 200, Body: []byte(chatGPTPopulatedList(t))}, nil
				}
				details++
				id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", details)
				require.Equal(t, chatGPTRequest(chatGPTDetailRequest, id, 0, false), path)
				if details > 1 && details <= 3 {
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"conversation_id":"` + id + `"` + tc.mapping + `}`)}, nil
				}
				return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(chatGPTSyncDetail(1, false, "Question"), chatGPTSyncID, id))}, nil
			}, nil)
			require.EqualError(t, err, "ChatGPT returned a conversation format this version can't read. Update AgentsView and try again")
			assert.Equal(t, 3, details)
			assert.Equal(t, 1, stats.Imported)
			assert.Equal(t, 2, stats.Errors)
			require.Len(t, stats.Refusals, 2)
			messages, err := d.GetAllMessages(t.Context(), "chatgpt:00000001-0000-4000-8000-000000000000")
			require.NoError(t, err)
			assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))
		})
	}
}

func TestSyncChatGPTSingleUnsupportedDetail(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
	}{
		{"missing mapping", `{"id":"00000002-0000-4000-8000-000000000000"}`},
		{"missing identity", `{"mapping":{},"update_time":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			details := 0
			fetch := func(_ context.Context, path string) (ClaudeAIResponse, error) {
				switch path {
				case chatGPTRequest(chatGPTListRequest, "", 0, true):
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
				case chatGPTRequest(chatGPTListRequest, "", 0, false):
					return ClaudeAIResponse{Status: 200, Body: []byte(`{"items":[{"id":"00000001-0000-4000-8000-000000000000","update_time":1},{"id":"00000002-0000-4000-8000-000000000000","update_time":1},{"id":"00000003-0000-4000-8000-000000000000","update_time":1}],"total":3}`)}, nil
				case chatGPTRequest(chatGPTDetailRequest, "00000002-0000-4000-8000-000000000000", 0, false):
					details++
					return ClaudeAIResponse{Status: 200, Body: []byte(tc.detail)}, nil
				default:
					for _, id := range []string{"00000001-0000-4000-8000-000000000000", "00000003-0000-4000-8000-000000000000"} {
						if path == chatGPTRequest(chatGPTDetailRequest, id, 0, false) {
							details++
							return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(chatGPTSyncDetail(1, false, "Question"), chatGPTSyncID, id))}, nil
						}
					}
					require.Failf(t, "unexpected ChatGPT request path", "%s", path)
					return ClaudeAIResponse{}, nil
				}
			}
			for run := range 2 {
				stats, err := SyncChatGPT(t.Context(), d, fetch, nil)
				require.ErrorIs(t, err, errChatGPTFormat)
				assert.Equal(t, 1, stats.Errors)
				if run == 0 {
					assert.Equal(t, 2, stats.Imported)
				} else {
					assert.Equal(t, 2, stats.Skipped)
				}
				assert.Equal(t, 3+run, details, "unsupported detail must be fetched again")
			}
			for _, id := range []string{"00000001-0000-4000-8000-000000000000", "00000003-0000-4000-8000-000000000000"} {
				messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+id)
				require.NoError(t, err)
				assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))
			}
		})
	}
}

func TestSyncChatGPTLongRetryAfterKeepsEarlierWrites(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		retryAfter   string
		wantRequests int
	}{
		{"429 long retry after", 429, "120", 1},
		{"503 long retry after", 503, "120", 1},
		{"429 retries exhausted", 429, "0", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
						return ClaudeAIResponse{Status: tc.status, RetryAfter: tc.retryAfter}, nil
					default:
						require.Failf(t, "unexpected ChatGPT request path", "%s", path)
						return ClaudeAIResponse{}, nil
					}
				}, nil)
				require.ErrorContains(t, err, "ChatGPT rate limited")
				assert.Equal(t, tc.wantRequests, rateLimitedRequests)
				assert.Equal(t, 1, stats.Imported)
				assert.Equal(t, 1, stats.Errors)
				messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
				require.NoError(t, err)
				assert.Equal(t, []string{"Question", "Answer"}, messageContents(messages))
			})
		})
	}
}

func TestSyncChatGPTZipImageReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name, role, text         string
		regenerate, changedTime  bool
		changedParent, missingID bool
		extend, legacy           bool
	}{
		{name: "regenerated assistant image", role: "assistant", text: "See this:", regenerate: true},
		{name: "different user branch with identical timestamp", role: "user", text: "See this:", regenerate: true},
		{name: "different user turn", role: "user", text: "See this:", changedTime: true},
		{name: "different parent", role: "user", text: "See this:", changedParent: true},
		{name: "missing identity", role: "user", text: "See this:", missingID: true},
		{name: "literal markdown and attachment", role: "user", text: "![image](https://example.com/image.png)"},
		{name: "control character", role: "user", text: `See\u0001 this:`},
		{name: "extended text", role: "user", text: "See this:", extend: true},
		{name: "legacy identity", role: "user", text: "See this:", legacy: true},
		{name: "extended text and legacy identity", role: "user", text: "See this:", extend: true, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			dir := t.TempDir()
			export := testjsonl.ChatGPTImageExport()
			body := strings.ReplaceAll(export["conversations-000.json"], "cg-1", chatGPTSyncID)
			body = strings.Replace(body, `"role":"user"`, `"role":"`+tc.role+`"`, 1)
			body = strings.Replace(body, "See this:", tc.text, 1)
			if tc.missingID {
				body = strings.Replace(body, `"id":"m1",`, "", 1)
			}
			if tc.extend {
				body = strings.Replace(body, `"asset_pointer":"file-service://file-img1"}]`, `"asset_pointer":"file-service://file-img1"}, " incomplete"]`, 1)
			}
			export["conversations-000.json"] = body
			for name, data := range export {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600))
			}
			stats, err := ImportChatGPT(t.Context(), d, dir, t.TempDir(), nil)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Imported)
			if tc.extend || tc.legacy {
				extend := tc.extend
				if tc.legacy {
					// ZIP imports on main predate source message identities.
					session, err := d.GetSessionFull(t.Context(), "chatgpt:"+chatGPTSyncID)
					require.NoError(t, err)
					require.NotNil(t, session)
					messages, err := d.GetAllMessages(t.Context(), session.ID)
					require.NoError(t, err)
					for i := range messages {
						messages[i].SourceUUID, messages[i].SourceParentUUID = "", ""
					}
					_, err = d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: *session, Messages: messages, ReplaceMessages: true, SkipSignalUpdates: true}})
					require.NoError(t, err)
				}
				var conversations []map[string]any
				require.NoError(t, json.Unmarshal([]byte(export["conversations-000.json"]), &conversations))
				conversation := conversations[0]
				unchangedDetail, err := json.Marshal(conversation)
				require.NoError(t, err)
				details := 0
				stats, err = SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1", string(unchangedDetail), &details), nil)
				require.NoError(t, err)
				if tc.legacy {
					assert.Equal(t, 1, stats.Updated)
				} else {
					assert.Equal(t, 1, stats.Skipped)
				}
				assert.Empty(t, replacedCopies(t, d, "chatgpt:"+chatGPTSyncID))
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
					want := before[i].Content
					if extend && i == 0 {
						want += " text"
					}
					assert.Equal(t, want, after[i].Content)
					assert.Equal(t, before[i].ID, after[i].ID)
				}
				assert.Contains(t, after[0].Content, "![image](")
				if extend {
					assert.Contains(t, after[0].Content, " incomplete text")
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
				return
			}
			before, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
			require.NoError(t, err)
			require.Len(t, before, 1)
			require.Contains(t, before[0].Content, "asset://")
			if tc.regenerate {
				body = strings.Replace(body, `"id":"m1"`, `"id":"m2"`, 1)
			}
			if tc.regenerate || tc.changedTime {
				body = strings.Replace(body, "file-service://file-img1", "file-service://file-img2", 1)
				body = strings.Replace(body, `"update_time":1706745660.0`, `"update_time":1706745720.0`, 1)
			}
			if tc.changedTime {
				body = strings.Replace(body, `"id":"m1","create_time":1706745600.0,`, `"id":"m1","create_time":1706745720.0,`, 1)
			}
			if tc.changedParent {
				body = strings.Replace(body, `"parent":"r"`, `"parent":null`, 1)
			}
			detail := strings.TrimSuffix(strings.TrimPrefix(body, "["), "]")
			details := 0
			_, err = SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745720", detail, &details), nil)
			require.NoError(t, err)
			after, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
			require.NoError(t, err)
			require.Len(t, after, 1)
			if tc.regenerate || tc.changedTime || tc.changedParent {
				assert.Equal(t, "See this:\n[image unavailable]", after[0].Content)
				copies := replacedCopies(t, d, "chatgpt:"+chatGPTSyncID)
				require.Len(t, copies, 1)
				saved, err := d.GetAllMessages(t.Context(), copies[0].ID)
				require.NoError(t, err)
				assert.Equal(t, messageContents(before), messageContents(saved))
				// Force another fetch to check that the same loss creates only one copy.
				require.NoError(t, d.UpsertProviderStatHash(t.Context(), parser.AgentChatGPT, "chatgpt:"+chatGPTSyncID, 0))
				_, err = SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745720", detail, &details), nil)
				require.NoError(t, err)
				assert.Len(t, replacedCopies(t, d, "chatgpt:"+chatGPTSyncID), 1)
			} else {
				assert.Equal(t, before[0].Content, after[0].Content)
				assert.Equal(t, before[0].ContentLength, after[0].ContentLength)
				assert.Equal(t, before[0].ID, after[0].ID)
				assert.Empty(t, replacedCopies(t, d, "chatgpt:"+chatGPTSyncID))
			}
		})
	}
}

func TestSyncChatGPTUpdatesArchivedToolCalls(t *testing.T) {
	for _, tc := range []struct {
		name, initial, later string
	}{
		{"additional call", chatGPTChainConv(chatGPTNodeSpec{"user", "text", "Run print(42)"}, chatGPTNodeSpec{"assistant", "text", "Running it."}), chatGPTToolRunConv("42", false)},
		{"changed result", chatGPTToolRunConv("41", false), chatGPTToolRunConv("42", false)},
		{"filled result", chatGPTToolRunConv("", false), chatGPTToolRunConv("42", false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			details := 0
			var before []db.Message
			for i, body := range []string{tc.initial, tc.later} {
				body = strings.ReplaceAll(body, "cg-tool", chatGPTSyncID)
				body = strings.Replace(body, `"update_time":1706745710.0`, fmt.Sprintf(`"update_time":%d`, i+1), 1)
				body = strings.TrimSuffix(strings.TrimPrefix(body, "["), "]")
				_, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, strconv.Itoa(i+1), body, &details), nil)
				require.NoError(t, err)
				if i == 0 {
					messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
					require.NoError(t, err)
					require.Len(t, messages, 2)
					before = messages
					// Keep another message after these rows so a replacement cannot reuse their IDs.
					_, err = d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
						Session:  db.Session{ID: "another-chat", Agent: "chatgpt", Project: "chatgpt"},
						Messages: []db.Message{{SessionID: "another-chat", Ordinal: 0, Role: "user", Content: "Another chat"}},
					}})
					require.NoError(t, err)
					_, err = d.PinMessage(t.Context(), "chatgpt:"+chatGPTSyncID, messages[1].ID, nil)
					require.NoError(t, err)
				}
			}
			messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			require.Len(t, messages[1].ToolCalls, 1)
			assert.Equal(t, "code_interpreter", messages[1].ToolCalls[0].ToolName)
			assert.Contains(t, messages[1].ToolCalls[0].ResultContent, "42")
			if tc.name == "filled result" {
				assert.Equal(t, before[0].ID, messages[0].ID)
				assert.Equal(t, before[1].ID, messages[1].ID)
			}
			pins, err := d.ListPinnedMessages(t.Context(), "chatgpt:"+chatGPTSyncID, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, 1, pins[0].Ordinal)
		})
	}
}

func TestSyncChatGPTUpdatesToolResultLengthsUnderTranscriptsPolicy(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentTranscripts)
	details := 0
	for i, tc := range []struct {
		output string
		length int
	}{{"41", 10}, {"42000", 13}} {
		body := strings.ReplaceAll(chatGPTToolRunConv(tc.output, false), "cg-tool", chatGPTSyncID)
		body = strings.Replace(body, `"update_time":1706745710.0`, fmt.Sprintf(`"update_time":%d`, i+1), 1)
		body = strings.TrimSuffix(strings.TrimPrefix(body, "["), "]")
		stats, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, strconv.Itoa(i+1), body, &details), nil)
		require.NoError(t, err)
		assert.Zero(t, stats.Errors)
		if i == 1 {
			assert.Equal(t, 1, stats.Updated)
		}
		messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
		require.NoError(t, err)
		require.Len(t, messages, 2)
		require.Len(t, messages[1].ToolCalls, 1)
		assert.Empty(t, messages[1].ToolCalls[0].ResultContent)
		assert.Equal(t, tc.length, messages[1].ToolCalls[0].ResultContentLength)
		if i == 0 {
			_, err = d.PinMessage(t.Context(), "chatgpt:"+chatGPTSyncID, messages[1].ID, new("Keep this result"))
			require.NoError(t, err)
		}
	}
	pins, err := d.ListPinnedMessages(t.Context(), "chatgpt:"+chatGPTSyncID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 1, pins[0].Ordinal)
	assert.Equal(t, new("Keep this result"), pins[0].Note)
}

func TestSyncChatGPTPagesActiveAndArchivedLists(t *testing.T) {
	for _, tc := range []struct {
		name         string
		withTotal    bool
		wantImported int
		wantPaths    []string
	}{
		{
			name: "with total", withTotal: true, wantImported: 52,
			wantPaths: []string{
				"/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=false",
				"/backend-api/conversations?offset=50&limit=50&order=updated&is_archived=false",
				"/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=true",
				"/backend-api/conversations?offset=50&limit=50&order=updated&is_archived=true",
			},
		},
		{
			name: "short pages without total", wantImported: 1,
			wantPaths: []string{
				"/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=false",
				"/backend-api/conversation/" + chatGPTSyncID,
				"/backend-api/conversations?offset=1&limit=50&order=updated&is_archived=false",
				"/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=true",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			var paths []string
			details := 0
			fetch := func(_ context.Context, path string) (ClaudeAIResponse, error) {
				isDetail := strings.HasPrefix(path, "/backend-api/conversation/")
				if !tc.withTotal || !isDetail {
					paths = append(paths, path)
				}
				if isDetail {
					id := strings.TrimPrefix(path, "/backend-api/conversation/")
					_, err := uuid.Parse(id)
					require.NoError(t, err, "unexpected request %s", path)
					details++
					return ClaudeAIResponse{Status: 200, Body: []byte(strings.ReplaceAll(chatGPTSyncDetail(1, false, "Question"), chatGPTSyncID, id))}, nil
				}
				body := ""
				switch path {
				case "/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=false":
					body = `{"items":[{"id":"` + chatGPTSyncID + `","update_time":1}]}`
					if tc.withTotal {
						body = strings.Replace(chatGPTPopulatedList(t), `"total":50`, `"total":51`, 1)
					}
				case "/backend-api/conversations?offset=0&limit=50&order=updated&is_archived=true":
					body = `{"items":[]}`
					if tc.withTotal {
						// Shared summaries exercise deduplication when a chat moves between lists.
						body = strings.Replace(chatGPTPopulatedList(t), `"total":50`, `"total":51`, 1)
					}
				case "/backend-api/conversations?offset=1&limit=50&order=updated&is_archived=false":
					require.False(t, tc.withTotal, "unexpected request %s", path)
					body = `{"items":[]}`
				case "/backend-api/conversations?offset=50&limit=50&order=updated&is_archived=false":
					require.True(t, tc.withTotal, "unexpected request %s", path)
					body = `{"items":[{"id":"11111111-1111-4111-8111-111111111111","update_time":1}],"total":51}`
				case "/backend-api/conversations?offset=50&limit=50&order=updated&is_archived=true":
					require.True(t, tc.withTotal, "unexpected request %s", path)
					body = `{"items":[{"id":"22222222-2222-4222-8222-222222222222","update_time":1}],"total":51}`
				default:
					require.Failf(t, "unexpected request", "%s", path)
				}
				return ClaudeAIResponse{Status: 200, Body: []byte(body)}, nil
			}
			stats, err := SyncChatGPT(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.wantImported, stats.Imported)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, tc.wantImported, details)
			assert.Equal(t, tc.wantPaths, paths)
			if tc.withTotal {
				stats, err = SyncChatGPT(t.Context(), d, fetch, nil)
				require.NoError(t, err)
				assert.Equal(t, 52, stats.Skipped)
				assert.Equal(t, 52, details)
				assert.Len(t, paths, 8, "both lists are paged on every sync")
			}
		})
	}
}

func TestSyncChatGPTBackfillsLegacyIdentityBeforeReplacingEditedTurn(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts} {
		t.Run(string(policy), func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(policy)
			details := 0
			detail := strings.Trim(chatGPTChainConvID(chatGPTSyncID,
				chatGPTNodeSpec{"user", "text", "Question"},
				chatGPTNodeSpec{"assistant", "text", "Answer"},
				chatGPTNodeSpec{"user", "text", "Follow up"},
			), "[]")
			result, err := parser.ParseChatGPTConversation([]byte(detail))
			require.NoError(t, err)
			legacy := chatGPTMessages(result.Session.ID, result.Messages)
			for i := range legacy {
				legacy[i].SourceUUID, legacy[i].SourceParentUUID = "", ""
			}
			_, err = d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
				Session: chatGPTSession(result.Session), Messages: legacy,
				ReplaceMessages: true, SkipSignalUpdates: true,
			}})
			require.NoError(t, err)
			id := "chatgpt:" + chatGPTSyncID
			seeded, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, seeded, 3)
			_, err = d.PinMessage(t.Context(), id, seeded[2].ID, new("Keep the original"))
			require.NoError(t, err)
			stats, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745710", detail, &details), nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Updated)
			before, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, before, 3)
			assert.Equal(t, "m-n3", before[2].SourceUUID)
			assert.Equal(t, "n2", before[2].SourceParentUUID)
			assert.Equal(t, seeded[2].ID, before[2].ID)

			detail = strings.NewReplacer(`"id":"m-n3"`, `"id":"new-user-message"`,
				`"Follow up"`, `"Follow up with more detail"`,
				`"update_time":1706745710.0`, `"update_time":1706745711`).Replace(detail)
			stats, err = SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1706745711", detail, &details), nil)
			require.NoError(t, err)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, 1, stats.Updated)
			after, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, after, 3)
			assert.Equal(t, "Follow up with more detail", after[2].Content)
			assert.Equal(t, "new-user-message", after[2].SourceUUID)
			pins, err := d.ListPinnedMessages(t.Context(), id, "")
			require.NoError(t, err)
			assert.Empty(t, pins, "the changed source turn no longer owns the original pin")
			copies := replacedCopies(t, d, id)
			require.Len(t, copies, 1)
			assert.True(t, d.IsSessionTrashed(t.Context(), copies[0].ID))
			original, err := d.GetAllMessages(t.Context(), copies[0].ID)
			require.NoError(t, err)
			require.Len(t, original, 3)
			assert.Equal(t, "Follow up", original[2].Content)
			assert.Equal(t, "m-n3", original[2].SourceUUID)
			pins, err = d.ListPinnedMessages(t.Context(), copies[0].ID, "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, original[2].ID, pins[0].MessageID)
			assert.Equal(t, new("Keep the original"), pins[0].Note)
		})
	}
}

func TestSyncChatGPTReplacesShorterBranchAndPreservesLostPins(t *testing.T) {
	d := testDB(t)
	details := 0
	_, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1", chatGPTSyncDetail(1, true, "Question"), &details), nil)
	require.NoError(t, err)
	id := "chatgpt:" + chatGPTSyncID
	before, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	_, err = d.PinMessage(t.Context(), id, before[3].ID, nil)
	require.NoError(t, err)
	stats, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "2", chatGPTSyncDetail(2, false, "Edited question"), &details), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	after, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, []string{"Edited question", "Answer"}, messageContents(after))
	copies := replacedCopies(t, d, id)
	require.Len(t, copies, 1)
	pins, err := d.ListPinnedMessages(t.Context(), copies[0].ID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 3, pins[0].Ordinal)
}

type chatGPTPostWriteReadStore struct{ *db.DB }

func (s chatGPTPostWriteReadStore) GetSessionFull(ctx context.Context, id string) (*db.Session, error) {
	session, err := s.DB.GetSessionFull(ctx, id)
	if err == nil && session != nil && session.MessageCount == 4 {
		return nil, errors.New("post-write read failed")
	}
	return session, err
}

func TestSyncChatGPTCommittedUpdateCountsOnReadFailure(t *testing.T) {
	d := testDB(t)
	details := 0
	_, err := SyncChatGPT(t.Context(), d, chatGPTSyncFetch(t, "1", chatGPTSyncDetail(1, false, "Question"), &details), nil)
	require.NoError(t, err)
	stats, err := SyncChatGPT(t.Context(), chatGPTPostWriteReadStore{d}, chatGPTSyncFetch(t, "2", chatGPTSyncDetail(2, true, "Question"), &details), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Updated)
	assert.Equal(t, 1, stats.Errors)
	messages, err := d.GetAllMessages(t.Context(), "chatgpt:"+chatGPTSyncID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Question", "Answer", "Follow up", "Second answer"}, messageContents(messages))
}

func TestSyncChatGPTCommittedContentCountsOnProgressCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	details := 0
	stats, err := SyncChatGPT(ctx, testDB(t), chatGPTSyncFetch(t, "1", chatGPTSyncDetail(1, false, "Question"), &details), &ImportCallbacks{
		OnProgress: func(ImportStats) { cancel() },
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, stats.Imported)
	assert.Equal(t, 1, details)
}
