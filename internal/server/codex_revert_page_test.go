package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func codexRevertUserJSON(timestamp, text string) string {
	return `{"type":"response_item","timestamp":"` + timestamp +
		`","payload":{"type":"message","role":"user","content":` +
		`[{"type":"input_text","text":"` + text + `"}]}}`
}

// A Codex thread/revert writes a second rollout named <thread>_<rollout>
// whose header carries the thread id. Each file must stay its own session so
// neither overwrites the other, with the page linked to the thread.
func TestCodexRevertPageSessionsThroughAPI(t *testing.T) {
	const (
		threadID  = "11111111-1111-4111-8111-111111111111"
		rolloutID = "22222222-2222-4222-8222-222222222222"
		headID    = "codex:" + threadID
		pageID    = "codex:" + threadID + "_" + rolloutID
	)
	te := setup(t)
	dir := filepath.Join(te.dataDir, "codex", "2026", "09", "01")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "rollout-2026-09-01T10-00-00-"+threadID+".jsonl"),
		[]byte(testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(
				threadID, "/work/project", "codex_cli_rs", "2026-09-01T10:00:00Z"),
			codexRevertUserJSON("2026-09-01T10:00:01Z", "first file one"),
			codexRevertUserJSON("2026-09-01T10:00:02Z", "first file two"),
		)), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "rollout-2026-09-01T11-00-00-"+threadID+"_"+rolloutID+".jsonl"),
		[]byte(testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaWithFieldsJSON(
				threadID, "/work/project", "codex_cli_rs", "2026-09-01T11:00:00Z",
				map[string]any{
					"history_mode": "paginated",
					"history_base": map[string]any{
						"thread_id":             threadID,
						"end_ordinal_exclusive": 3,
					},
				}),
			codexRevertUserJSON("2026-09-01T11:00:01Z", "second file one"),
			codexRevertUserJSON("2026-09-01T11:30:00Z", "second file two"),
		)), 0o600))

	te.engine.SyncAll(t.Context(), nil)

	messageTexts := func(id string) []string {
		t.Helper()
		w := te.get(t, "/api/v1/sessions/"+id+"/messages")
		require.Equal(t, http.StatusOK, w.Code, "messages of %s: %s", id, w.Body.String())
		var texts []string
		for _, m := range decode[messageListResponse](t, w).Messages {
			texts = append(texts, m.Content)
		}
		return texts
	}
	assert.Equal(t, []string{"first file one", "first file two"}, messageTexts(headID))
	assert.Equal(t, []string{"second file one", "second file two"}, messageTexts(pageID))

	w := te.get(t, "/api/v1/sessions/"+pageID)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	page := decode[db.Session](t, w)
	require.NotNil(t, page.ParentSessionID)
	assert.Equal(t, headID, *page.ParentSessionID)
	assert.Equal(t, "continuation", page.RelationshipType)
	require.NotNil(t, page.EndedAt)
	assert.Equal(t, "2026-09-01T11:30:00Z", *page.EndedAt)
}
