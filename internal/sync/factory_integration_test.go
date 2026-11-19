package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const factoryIntegrationID = "0f9c1c2e-6f7d-4b3a-9c2e-1a2b3c4d5e6f"

func copyFactoryFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "parser", "testdata", "factory", "-home-dev-workspace-demo-app")
	root := t.TempDir()
	dst := filepath.Join(root, "-home-dev-workspace-demo-app")
	require.NoError(t, os.MkdirAll(dst, 0o755))
	for _, name := range []string{
		factoryIntegrationID + ".jsonl",
		factoryIntegrationID + ".settings.json",
	} {
		data, err := os.ReadFile(filepath.Join(src, name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dst, name), data, 0o644))
	}
	return root
}

func TestSyncFactoryTranscript(t *testing.T) {
	root := copyFactoryFixture(t)
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFactory: {root}},
		Machine:   "devbox",
	})

	runSyncAndAssert(t, engine, SyncStats{
		TotalSessions: 1, Synced: 1, Skipped: 0,
		Anomalies: AnomalyStats{
			MalformedLinesByAgent: map[string]int{"factory": 1},
			MalformedLinesTotal:   1,
		},
	})

	id := "factory:" + factoryIntegrationID
	sess, err := database.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "factory", sess.Agent)
	assert.Equal(t, "demo_app", sess.Project)
	name, found, err := database.GetSessionName(t.Context(), id)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "add retry to fetch helper", name)
	// tool_result-only user rows fold into the preceding tool call; the
	// user_only UI notice stays as a system row.
	assert.Equal(t, 6, sess.MessageCount)
	assert.Equal(t, 2, sess.UserMessageCount)

	msgs, err := database.GetMessages(t.Context(), id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, msgs, 6)
	assert.True(t, msgs[4].IsSystem)
	assert.Equal(t, "The retry log in this screenshot looks wrong\n[image]", msgs[5].Content)
	assert.Equal(t, "Add exponential backoff to fetchWithRetry in src/http.ts", msgs[0].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].ToolName)
	assert.Contains(t, msgs[1].ToolCalls[0].ResultContent, "fetchWithRetry")

	events, err := database.GetUsageEvents(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, events, 2, "settings.json session and subagent usage must reach the DB")
	byOutput := map[int]int{}
	for _, ev := range events {
		assert.Equal(t, "claude-fable-5", ev.Model)
		assert.Equal(t, "session", ev.Source)
		byOutput[ev.OutputTokens] = ev.CacheReadInputTokens
	}
	// Cumulative totals above the per-request token bound are kept whole.
	assert.Equal(t, map[int]int{2530: 3424747, 900: 2500000}, byOutput)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 2530+900, sess.TotalOutputTokens)
	assert.False(t, sess.HasPeakContextTokens)

	// Unchanged transcript + sidecar: discovered but skipped, not re-parsed.
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 0, Skipped: 1})
}

func TestSyncFactoryLateToolResultUpdatesStoredCall(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-home-dev-demo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, factoryIntegrationID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(
		`{"type":"session_start","id":"`+factoryIntegrationID+`","title":"t","cwd":"/home/dev/demo"}
{"type":"message","id":"m1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","content":[{"type":"text","text":"read the file"}]}}
{"type":"message","id":"m2","timestamp":"2026-07-31T21:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/home/dev/demo/a.txt"}}]}}
`), 0o644))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFactory: {root}},
		Machine:   "devbox",
	})
	runSyncAndAssert(t, engine, SyncStats{TotalSessions: 1, Synced: 1})

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"message","id":"m3","timestamp":"2026-07-31T21:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"FILEBODY"}]}}
`)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	engine.SyncPaths([]string{path})

	msgs, err := database.GetMessages(t.Context(), "factory:"+factoryIntegrationID, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "FILEBODY", msgs[1].ToolCalls[0].ResultContent,
		"a result appended after a sync must reach the stored tool call")
}

func TestSyncFactorySettingsChangeResyncsUsage(t *testing.T) {
	root := copyFactoryFixture(t)
	settings := filepath.Join(root, "-home-dev-workspace-demo-app",
		factoryIntegrationID+".settings.json")
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFactory: {root}},
		Machine:   "devbox",
	})
	engine.SyncAll(t.Context(), nil)
	id := "factory:" + factoryIntegrationID

	require.NoError(t, os.WriteFile(settings, []byte(
		`{"model":"claude-fable-6","providerLock":"anthropic","tokenUsage":{"inputTokens":1,"outputTokens":4242}}`,
	), 0o644))
	engine.SyncPaths([]string{settings})
	events, err := database.GetUsageEvents(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, events, 1, "the old model's event and the removed child are replaced")
	assert.Equal(t, "claude-fable-6", events[0].Model)
	assert.Equal(t, 4242, events[0].OutputTokens)
	sess, err := database.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 4242, sess.TotalOutputTokens)

	require.NoError(t, os.Remove(settings))
	engine.SyncPaths([]string{settings})
	events, err = database.GetUsageEvents(t.Context(), id)
	require.NoError(t, err)
	assert.Empty(t, events, "deleting the sidecar clears its usage")
}

func TestSyncFactoryUnreadableSettingsKeepsTranscript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits do not block reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	root := copyFactoryFixture(t)
	settings := filepath.Join(root, "-home-dev-workspace-demo-app",
		factoryIntegrationID+".settings.json")
	require.NoError(t, os.Chmod(settings, 0))
	t.Cleanup(func() { _ = os.Chmod(settings, 0o644) })
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFactory: {root}},
		Machine:   "devbox",
	})
	stats := engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 1, stats.Synced, "an unreadable sidecar must not block the transcript")

	id := "factory:" + factoryIntegrationID
	sess, err := database.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, sess)
	events, err := database.GetUsageEvents(t.Context(), id)
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestSyncFactorySameSizeSettingsRewriteResyncs(t *testing.T) {
	root := copyFactoryFixture(t)
	dir := filepath.Join(root, "-home-dev-workspace-demo-app")
	settings := filepath.Join(dir, factoryIntegrationID+".settings.json")
	write := func(output string) {
		t.Helper()
		require.NoError(t, os.WriteFile(settings, []byte(
			`{"model":"claude-fable-5","tokenUsage":{"inputTokens":1,"outputTokens":`+output+`}}`,
		), 0o644))
	}
	write("1111")
	info, err := os.Stat(settings)
	require.NoError(t, err)
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFactory: {root}},
		Machine:   "devbox",
	})
	runSyncAndAssert(t, engine, SyncStats{
		TotalSessions: 1, Synced: 1,
		Anomalies: AnomalyStats{
			MalformedLinesByAgent: map[string]int{"factory": 1},
			MalformedLinesTotal:   1,
		},
	})

	// Same byte length and the same mtime: only the content changed.
	write("2222")
	require.NoError(t, os.Chtimes(settings, info.ModTime(), info.ModTime()))
	runSyncAndAssert(t, engine, SyncStats{
		TotalSessions: 1, Synced: 1,
		Anomalies: AnomalyStats{
			MalformedLinesByAgent: map[string]int{"factory": 1},
			MalformedLinesTotal:   1,
		},
	})
	events, err := database.GetUsageEvents(t.Context(), "factory:"+factoryIntegrationID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, 2222, events[0].OutputTokens)
}

func TestSyncFactoryMessagelessSessionClearsUsageWithSidecar(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-home-dev-demo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	transcript := filepath.Join(dir, factoryIntegrationID+".jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(
		`{"type":"session_start","id":"`+factoryIntegrationID+`","title":"t","cwd":"/home/dev/demo"}
{"type":"message","id":"m1","timestamp":"2026-07-31T21:00:00Z","message":{"role":"user","visibility":"llm_only","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"}]}}
`), 0o644))
	settings := factoryIntegrationSettingsPath(transcript)
	validSettings := `{"model":"claude-fable-5","tokenUsage":{"inputTokens":4,"outputTokens":120}}`
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFactory: {root}},
		Machine:   "devbox",
	})
	id := "factory:" + factoryIntegrationID
	usage := func() []db.UsageEvent {
		t.Helper()
		events, err := database.GetUsageEvents(t.Context(), id)
		require.NoError(t, err)
		return events
	}

	for _, tc := range []struct {
		name  string
		spoil func()
	}{
		{"sidecar deleted", func() { require.NoError(t, os.Remove(settings)) }},
		{"sidecar invalid", func() { require.NoError(t, os.WriteFile(settings, []byte("{oops"), 0o644)) }},
		{"counters zeroed", func() {
			require.NoError(t, os.WriteFile(settings, []byte(
				`{"model":"claude-fable-5","tokenUsage":{"inputTokens":0,"outputTokens":0}}`,
			), 0o644))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(settings, []byte(validSettings), 0o644))
			engine.SyncPaths([]string{settings})
			events := usage()
			require.Len(t, events, 1, "a session with no visible messages still records usage")
			assert.Equal(t, 120, events[0].OutputTokens)

			tc.spoil()
			engine.SyncPaths([]string{settings})
			assert.Empty(t, usage(), "stored usage must clear when the sidecar stops providing it")
			sess, err := database.GetSession(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, sess)
			assert.Zero(t, sess.TotalOutputTokens)
		})
	}
}

func factoryIntegrationSettingsPath(transcript string) string {
	return strings.TrimSuffix(transcript, ".jsonl") + ".settings.json"
}

func TestSyncFactoryEmptiedTranscriptClearsStoredMessages(t *testing.T) {
	root := copyFactoryFixture(t)
	transcript := filepath.Join(root, "-home-dev-workspace-demo-app", factoryIntegrationID+".jsonl")
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFactory: {root}},
		Machine:   "devbox",
	})
	engine.SyncAll(t.Context(), nil)
	id := "factory:" + factoryIntegrationID
	msgs, err := database.GetMessages(t.Context(), id, 0, 100, true)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)

	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	engine.SyncPaths([]string{transcript})
	msgs, err = database.GetMessages(t.Context(), id, 0, 100, true)
	require.NoError(t, err)
	assert.Empty(t, msgs, "an emptied transcript must not leave its old messages stored")
}
