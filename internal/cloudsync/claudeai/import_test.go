package claudeai

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestPrepareBrowserImportUsesOnlySuppliedBatchAndForceOverwritesCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	summary := json.RawMessage(`{"uuid":"conversation-1","name":"one","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z"}`)
	first, err := PrepareBrowserImport(root, []BrowserConversation{{Summary: summary, Conversation: json.RawMessage(`{"uuid":"conversation-1","chat_messages":[{"uuid":"m1","sender":"human","text":"old","created_at":"2025-01-01T00:00:00Z"}]}`)}})
	require.NoError(t, err)
	require.NoError(t, first.Commit())
	second, err := PrepareBrowserImportWithForce(root, []BrowserConversation{{Summary: summary, Conversation: json.RawMessage(`{"uuid":"conversation-1","chat_messages":[{"uuid":"m1","sender":"human","text":"new","created_at":"2025-01-01T00:00:00Z"}]}`)}}, true)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"uuid":"conversation-1","name":"one","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z","chat_messages":[{"uuid":"m1","sender":"human","text":"new","created_at":"2025-01-01T00:00:00Z"}]}]`, string(second.ExportJSON))
}

func TestNormalizeConversationUsesActiveBranch(t *testing.T) {
	t.Parallel()
	got, err := normalizeConversation([]byte(`{"uuid":"conversation-1","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z","current_leaf_message_uuid":"selected","chat_messages":[{"uuid":"root"},{"uuid":"discarded","parent_message_uuid":"root"},{"uuid":"selected","parent_message_uuid":"root"}]}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"uuid":"conversation-1","name":"","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-02T00:00:00Z","chat_messages":[{"uuid":"root"},{"uuid":"selected","parent_message_uuid":"root"}]}`, string(got))
}
