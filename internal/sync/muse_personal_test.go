package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const (
	musePersonalSyncID     = "11111111-2222-4333-8444-555555555555"
	musePersonalSyncHeader = `{"type":"session_header","version":1,"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z"}` + "\n"
)

func musePersonalSyncItem(seq int, item string) string {
	return fmt.Sprintf(`{"type":"item","seq":%d,"source":"scheduler.cron","created_at":"2026-10-05T15:04:06Z","item":%s}`+"\n", seq, item)
}

func musePersonalSyncFile(t *testing.T, body string) (string, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "agent-66666666-7777-4888-8999-000000000000", "sessions", musePersonalSyncID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(musePersonalSyncHeader+body), 0o600))
	return root, path
}

func TestMusePersonalSQLiteOccurrencePairing(t *testing.T) {
	call := `{"type":"function_call","call_id":"reused","name":"read","arguments":"{\"path\":\"/example/file\"}"}`
	output := func(text string) string {
		return fmt.Sprintf(`{"type":"function_call_output","call_id":"reused","output":%q,"success":true}`, text)
	}
	root, _ := musePersonalSyncFile(t, musePersonalSyncItem(0, output("orphan-before"))+musePersonalSyncItem(1, call)+musePersonalSyncItem(2, output("first"))+musePersonalSyncItem(3, call)+musePersonalSyncItem(4, output("second"))+musePersonalSyncItem(5, call)+musePersonalSyncItem(6, call)+musePersonalSyncItem(7, output("ambiguous")))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)
	messages, err := database.GetMessages(t.Context(), "muse-personal:"+musePersonalSyncID, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 6)
	assert.Equal(t, "orphan-before", messages[0].Content)
	for i, want := range []string{"first", "second"} {
		require.Len(t, messages[i+1].ToolCalls, 1)
		c := messages[i+1].ToolCalls[0]
		assert.Equal(t, "reused", c.ToolUseID)
		require.Len(t, c.ResultEvents, 1)
		assert.Equal(t, want, c.ResultContent)
		assert.Equal(t, want, c.ResultEvents[0].Content)
	}
	assert.Empty(t, messages[3].ToolCalls[0].ResultEvents)
	assert.Empty(t, messages[4].ToolCalls[0].ResultEvents)
	assert.Equal(t, "ambiguous", messages[5].Content)
	assert.Equal(t, "tool_result", messages[5].SourceSubtype)
}

func TestMusePersonalBlockedResultsAndArchivePolicy(t *testing.T) {
	for _, mode := range []string{"blocked", "transcripts", "usage"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := musePersonalSyncFile(t, musePersonalSyncItem(0, `{"type":"message","role":"user","text":"visible dialogue"}`)+musePersonalSyncItem(1, `{"type":"function_call","call_id":"a","name":"read","arguments":"{\"path\":\"/example/file\"}"}`)+musePersonalSyncItem(2, `{"type":"function_call_output","call_id":"a","output":"hidden-result","success":true}`)+musePersonalSyncItem(3, `{"type":"function_call_output","call_id":"orphan","output":"orphan-result","success":false}`)+musePersonalSyncItem(4, `{"type":"future","text":"hidden-unknown-body"}`))
			database := openTestDB(t)
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"}
			switch mode {
			case "blocked":
				cfg.BlockedResultCategories = []string{"Read"}
			case "transcripts":
				cfg.ArchiveContent = config.ArchiveContentTranscripts
				database.SetArchiveContent(cfg.ArchiveContent)
			case "usage":
				cfg.ArchiveContent = config.ArchiveContentUsage
				database.SetArchiveContent(cfg.ArchiveContent)
			}
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			messages, err := database.GetMessages(t.Context(), "muse-personal:"+musePersonalSyncID, 0, 100, true)
			require.NoError(t, err)
			for _, m := range messages {
				assert.NotContains(t, m.Content, "hidden-unknown-body")
				if mode == "usage" {
					assert.Empty(t, m.Content)
					assert.Empty(t, m.ThinkingText)
				}
				if mode == "transcripts" && m.Role == "tool" {
					assert.Empty(t, m.Content)
				}
				for _, c := range m.ToolCalls {
					assert.Equal(t, "Read", c.Category)
					assert.Empty(t, c.ResultContent)
					for _, event := range c.ResultEvents {
						assert.Empty(t, event.Content)
					}
					if mode != "blocked" {
						assert.Empty(t, c.InputJSON)
					}
				}
			}
			if mode != "usage" {
				require.Len(t, messages, 4)
				assert.Equal(t, "visible dialogue", messages[0].Content)
			}
		})
	}
}

func TestMusePersonalArchiveReplacementAndIndexPolling(t *testing.T) {
	user := musePersonalSyncItem(0, `{"type":"message","role":"user","text":"original"}`)
	call := musePersonalSyncItem(1, `{"type":"function_call","call_id":"late","name":"exec","arguments":"{}"}`)
	root, path := musePersonalSyncFile(t, user+call)
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentMusePersonal: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	ctx := t.Context()
	id := "muse-personal:" + musePersonalSyncID
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	assert.Zero(t, engine.SyncAll(ctx, nil).Synced)
	output := musePersonalSyncItem(2, `{"type":"function_call_output","call_id":"late","output":"late output","success":true}`)
	require.NoError(t, os.WriteFile(path, []byte(musePersonalSyncHeader+user+call+output), 0o600))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	messages, err := database.GetMessages(ctx, id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Len(t, messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "late output", messages[1].ToolCalls[0].ResultEvents[0].Content)
	before := engine.SourceMtime(ctx, id)
	index := filepath.Join(filepath.Dir(path), "sessions.json")
	indexText := `{"version":1,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T16:00:00Z"}]}`
	require.NoError(t, os.WriteFile(index, []byte(indexText), 0o600))
	assert.NotEqual(t, before, engine.SourceMtime(ctx, id))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	session, err := database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, session.EndedAt)
	assert.Equal(t, "2026-10-05T16:00:00Z", *session.EndedAt)
	// Same-size, same-mtime index replacement must still refresh metadata.
	info, err := os.Stat(index)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(index, []byte(strings.ReplaceAll(indexText, "16:00:00", "17:00:00")), 0o600))
	require.NoError(t, os.Chtimes(index, info.ModTime(), info.ModTime()))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	session, err = database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-05T17:00:00Z", *session.EndedAt)
	require.NoError(t, os.Remove(index))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	session, err = database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-05T15:04:06Z", *session.EndedAt)
	// A shorter unfinished rewrite must preserve all previously archived results.
	replacement := musePersonalSyncItem(0, `{"type":"message","role":"user","text":"replacement"}`)
	require.NoError(t, os.WriteFile(path, []byte(musePersonalSyncHeader+replacement+`{"type":"item"`), 0o600))
	assert.Positive(t, engine.SyncAll(ctx, nil).Failed)
	messages, err = database.GetMessages(ctx, id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "late output", messages[1].ToolCalls[0].ResultContent)
	require.NoError(t, os.WriteFile(path, []byte(musePersonalSyncHeader+replacement), 0o600))
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	messages, err = database.GetMessages(ctx, id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "replacement", messages[0].Content)
	found, err := database.Search(ctx, db.SearchFilter{Query: "original"})
	require.NoError(t, err)
	assert.Empty(t, found.Results)
}
