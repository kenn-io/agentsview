package server_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeHookOutputSurvivesIncrementalMessagesAPI(t *testing.T) {
	te := setup(t)
	path := te.writeProjectFile(t, "hooks", "hook-api.jsonl", `{"type":"user","uuid":"u1","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","message":{"id":"answer-1","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}}
`)
	first := te.engine.SyncAll(t.Context(), nil)
	require.Zero(t, first.Failed)
	require.Equal(t, 1, first.Synced)
	w := te.get(t, "/api/v1/sessions/hook-api/messages")
	assertStatus(t, w, http.StatusOK)
	require.Len(t, decode[messageListResponse](t, w).Messages, 2)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"attachment","uuid":"h1","parentUuid":"a1","timestamp":"2024-01-01T00:00:02Z","attachment":{"type":"hook_success","content":"  中文 <script>literal</script>\n  preserved  \n"}}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	appended := te.engine.SyncAll(t.Context(), nil)
	require.Zero(t, appended.Failed)
	require.Equal(t, 1, appended.Synced)
	w = te.get(t, "/api/v1/sessions/hook-api/messages")
	assertStatus(t, w, http.StatusOK)
	response := decode[messageListResponse](t, w)
	require.Len(t, response.Messages, 3)
	hook := response.Messages[2]
	assert.Equal(t, "user", hook.Role)
	assert.True(t, hook.IsSystem)
	assert.Equal(t, "attachment", hook.SourceType)
	assert.Equal(t, "hook_success", hook.SourceSubtype)
	assert.Equal(t, "h1", hook.SourceUUID)
	assert.Equal(t, "a1", hook.SourceParentUUID)
	assert.Equal(t, "Content:\n  中文 <script>literal</script>\n  preserved  \n", hook.Content)
	session, err := te.db.GetSessionFull(t.Context(), "hook-api")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 3, session.MessageCount)
	assert.Equal(t, 1, session.UserMessageCount)
	unchanged := te.engine.SyncAll(t.Context(), nil)
	assert.Zero(t, unchanged.Failed)
	assert.Zero(t, unchanged.Synced)
	w = te.get(t, "/api/v1/sessions/hook-api/messages?direction=desc&limit=2")
	assertStatus(t, w, http.StatusOK)
	page := decode[messageListResponse](t, w)
	require.Len(t, page.Messages, 2)
	assert.Equal(t, "hook_success", page.Messages[0].SourceSubtype)
}
