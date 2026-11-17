package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// The stored Claude.ai freshness hashes must not change, or every synced chat
// would be fetched again. The values were computed with the formula Claude.ai
// Sync used before the shared runner.
func TestClaudeAIFreshnessGolden(t *testing.T) {
	full := &db.Session{
		MessageCount:       2,
		TranscriptRevision: new("rev-1"),
		SessionName:        new("Chat"),
		EndedAt:            new("2026-01-02T10:05:00.123456Z"),
	}
	for _, tc := range []struct {
		name    string
		policy  config.ArchiveContent
		updated string
		leaf    string
		session *db.Session
		want    uint64
	}{
		{"full archive", config.ArchiveContentFull, "2026-01-02T10:05:00.123456Z", "reply", full, 0x7f8734e1f0d009e4},
		{"usage archive marker", config.ArchiveContentUsage, "2026-01-02T10:05:00.123456Z", "reply", full, 0xeec446da29d8743f},
		{"unset fields", config.ArchiveContentFull, "2026-01-02T10:05:00Z", "leaf", &db.Session{}, 0xdc1844fc4cd126cc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			d.SetArchiveContent(tc.policy)
			assert.Equal(t, tc.want, claudeAIFreshness(d, tc.updated, tc.leaf, tc.session))
		})
	}
}

// Sample values the path fixtures use. The extension tests check that its
// allowlist accepts every fixture line, so Go and the extension agree on
// each path Sync builds.
const (
	browserPathOrganization = "11111111-1111-4111-8111-111111111111"
	browserPathConversation = "22222222-2222-4222-8222-222222222222"
)

var browserPathBuilders = map[parser.AgentType]func() []string{
	parser.AgentClaudeAI: func() []string {
		return []string{
			claudeAIRequest(claudeAIOrganizationsRequest, "", "", 0),
			claudeAIRequest(claudeAIConversationsRequest, browserPathOrganization, "", 0),
			claudeAIRequest(claudeAIConversationsRequest, browserPathOrganization, "", 50),
			claudeAIRequest(claudeAIConversationRequest, browserPathOrganization, browserPathConversation, 0),
		}
	},
	parser.AgentChatGPT: func() []string {
		return []string{
			chatGPTRequest(chatGPTListRequest, "", 0, false),
			chatGPTRequest(chatGPTListRequest, "", 50, true),
			chatGPTRequest(chatGPTDetailRequest, browserPathConversation, 0, false),
		}
	},
}

func TestBrowserPathFixtures(t *testing.T) {
	for _, platform := range BrowserPlatforms() {
		t.Run(string(platform.Agent), func(t *testing.T) {
			build := browserPathBuilders[platform.Agent]
			require.NotNil(t, build, "every platform needs a path fixture")
			fixture, err := os.ReadFile(filepath.Join("testdata", "browser_paths", string(platform.Agent)+".txt"))
			require.NoError(t, err)
			assert.Equal(t, strings.Split(strings.TrimSpace(string(fixture)), "\n"), build())
		})
	}
}

func TestBrowserRequestFilesMatchPlatforms(t *testing.T) {
	entries, err := browserRequestFiles.ReadDir("browser_requests")
	require.NoError(t, err)
	var files []string
	for _, entry := range entries {
		files = append(files, strings.TrimSuffix(entry.Name(), ".txt"))
	}
	var names []string
	for _, platform := range BrowserPlatforms() {
		names = append(names, string(platform.Agent))
		found, ok := LookupBrowserPlatform(string(platform.Agent))
		require.True(t, ok)
		assert.Equal(t, platform.Label, found.Label)
	}
	assert.ElementsMatch(t, names, files)
	_, ok := LookupBrowserPlatform("claude")
	assert.False(t, ok, "only browser platforms resolve")
}

// A usage-only archive keeps an assistant reply's tool calls, and equal stored
// rows ignore tool calls, so a reply that gains its model while carrying fewer
// calls than stored must not be filled in place from the shorter row.
func TestSyncKeepingHistoryModelFillWithFewerToolCalls(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	const id = "chatgpt:33333333-3333-4333-8333-333333333333"
	at := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	result := func(model string, calls ...string) parser.ParseResult {
		reply := parser.ParsedMessage{
			Ordinal: 1, Role: parser.RoleAssistant, Content: "Answer", ContentLength: 6,
			Timestamp: at.Add(time.Second), Model: model, HasToolUse: len(calls) > 0,
		}
		for _, name := range calls {
			reply.ToolCalls = append(reply.ToolCalls, parser.ParsedToolCall{
				ToolUseID: name, ToolName: "subagent", Category: "Task", SubagentSessionID: "agent-" + name,
			})
		}
		return parser.ParseResult{
			Session: parser.ParsedSession{
				ID: id, Project: "chatgpt", Agent: parser.AgentChatGPT, FirstMessage: "Question",
				StartedAt: at, EndedAt: at.Add(time.Second), MessageCount: 2, UserMessageCount: 1,
			},
			Messages: []parser.ParsedMessage{
				{Ordinal: 0, Role: parser.RoleUser, Content: "Question", ContentLength: 8, Timestamp: at},
				reply,
			},
		}
	}

	status, err := syncKeepingHistory(t.Context(), d, result("", "a", "b"), nil)
	require.NoError(t, err)
	require.Equal(t, importNew, status)
	before, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, before, 1, "usage-only storage keeps only the reply")
	require.Empty(t, before[0].Model)
	require.Len(t, before[0].ToolCalls, 2)

	status, err = syncKeepingHistory(t.Context(), d, result("gpt-4o", "a"), nil)
	require.NoError(t, err)
	assert.Equal(t, importSkipped, status)
	after, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, before[0].ID, after[0].ID, "the stored reply keeps its row")
	assert.Empty(t, after[0].Model, "the shorter reply does not fill the stored row")
	assert.Equal(t, []string{"a", "b"}, toolUseIDs(after[0].ToolCalls), "stored calls are not cut to the shorter reply")
}

func toolUseIDs(calls []db.ToolCall) []string {
	ids := make([]string, len(calls))
	for i, call := range calls {
		ids[i] = call.ToolUseID
	}
	return ids
}

// A platform with no sign-in check treats every non-2xx status as an HTTP
// error rather than an expired sign-in.
func TestFetchBrowserWithoutAuthCheck(t *testing.T) {
	platform := BrowserPlatform{Label: "Example"}
	calls := 0
	_, err := fetchBrowser(t.Context(), platform, func(context.Context, string) (BrowserResponse, error) {
		calls++
		return BrowserResponse{Status: 401, Body: []byte(`{"detail":"signed out"}`)}, nil
	}, "/api/chats")
	require.EqualError(t, err, "Example returned HTTP 401")
	assert.NotErrorIs(t, err, ErrBrowserAuthRequired)
	assert.Equal(t, 1, calls, "a 4xx status is not retried")
}
