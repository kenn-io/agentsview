package claudeai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareImportCachesChangedConversationsAndBuildsExport(t *testing.T) {
	t.Parallel()
	var detailCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/organizations/org-123/chat_conversations_v2":
			_, _ = w.Write([]byte(`{"conversations":[{"uuid":"conversation-1","name":"Hello","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z"}],"has_more":false}`))
		case "/api/organizations/org-123/chat_conversations/conversation-1":
			detailCalls.Add(1)
			_, _ = w.Write([]byte(`{"uuid":"conversation-1","name":"Hello","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z","current_leaf_message_uuid":"m2","chat_messages":[{"uuid":"m1","sender":"human","text":"hi","created_at":"2025-01-01T00:00:00Z"},{"uuid":"m2","parent_message_uuid":"m1","sender":"assistant","content":[{"type":"text","text":"hello"}],"created_at":"2025-01-01T00:00:01Z"}]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(server.Client(), server.URL, Credentials{Cookie: "sessionKey=secret; lastActiveOrg=org-123"})
	require.NoError(t, err)
	root := t.TempDir()

	first, err := PrepareImport(context.Background(), client, root, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, first.Downloaded)
	assert.Equal(t, 0, first.Unchanged)
	assert.JSONEq(t, `[{"uuid":"conversation-1","name":"Hello","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z","chat_messages":[{"uuid":"m1","sender":"human","text":"hi","created_at":"2025-01-01T00:00:00Z"},{"uuid":"m2","parent_message_uuid":"m1","sender":"assistant","content":[{"type":"text","text":"hello"}],"created_at":"2025-01-01T00:00:01Z"}]}]`, string(first.ExportJSON))
	require.NoError(t, first.Commit())

	second, err := PrepareImport(context.Background(), client, root, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, second.Downloaded)
	assert.Equal(t, 1, second.Unchanged)
	assert.Equal(t, int32(1), detailCalls.Load())
}

func TestPlanBrowserImportSkipsCachedSummaryAndPersistsState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	summary := []byte(`{"uuid":"conversation-1","updated_at":"2025-01-02T00:00:00Z"}`)
	first, err := PlanBrowserImport(root, []json.RawMessage{summary})
	require.NoError(t, err)
	assert.Equal(t, []string{"conversation-1"}, first.ChangedIDs)
	assert.Equal(t, 1, first.State.Scanned)
	require.NoError(t, atomicWriteJSON(conversationCachePath(root, "conversation-1"), json.RawMessage(`{"uuid":"conversation-1"}`)))
	require.NoError(t, atomicWriteJSON(filepath.Join(root, cacheManifestName), map[string]string{"conversation-1": "2025-01-02T00:00:00Z"}))
	second, err := PlanBrowserImport(root, []json.RawMessage{summary})
	require.NoError(t, err)
	assert.Empty(t, second.ChangedIDs)
	assert.Equal(t, 1, second.Unchanged)
	repair, err := PlanBrowserImportWithForce(root, []json.RawMessage{summary}, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"conversation-1"}, repair.ChangedIDs)
	assert.Zero(t, repair.Unchanged)
	state, err := CompleteBrowserImport(root)
	require.NoError(t, err)
	assert.Equal(t, "completed", state.Status)
	state, err = FailBrowserImport(root)
	require.NoError(t, err)
	assert.Equal(t, "failed", state.Status)
	assert.Equal(t, 1, state.Failed)
}

func TestNormalizeConversationUsesActiveBranch(t *testing.T) {
	t.Parallel()
	got, err := normalizeConversation([]byte(`{"uuid":"conversation-1","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z","current_leaf_message_uuid":"selected","chat_messages":[{"uuid":"root"},{"uuid":"discarded","parent_message_uuid":"root"},{"uuid":"selected","parent_message_uuid":"root"}]}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"uuid":"conversation-1","name":"","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z","chat_messages":[{"uuid":"root"},{"uuid":"selected","parent_message_uuid":"root"}]}`, string(got))
}
