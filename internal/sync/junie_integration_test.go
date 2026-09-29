package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSyncJunieMetadataFreshnessAndSourceDeletion(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session-one")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	eventsPath := filepath.Join(sessionDir, "events.jsonl")
	require.NoError(t, os.WriteFile(eventsPath, []byte(
		`{"kind":"UserPromptEvent","requestId":"req-1","prompt":"Hello","timestampMs":1704067200500}`+"\n"+
			`{"kind":"SessionA2uxEvent","taskId":"task-1","event":{"agentEvent":{"kind":"MarkdownBlockUpdatedEvent","stepId":"response-1","text":"Draft response"}},"timestampMs":1704067200600}`+"\n"+
			`{"kind":"SessionA2uxEvent","taskId":"task-1","event":{"agentEvent":{"kind":"LlmResponseMetadataEvent","agent":"JUNIE","modelUsage":[{"model":"claude-sonnet-4-6","cost":0.00125,"inputTokens":100,"cacheInputTokens":20,"cacheCreateTokens":30,"outputTokens":40,"time":1}]}},"timestampMs":1704067200750}`+"\n",
	), 0o600))

	indexPath := filepath.Join(root, "index.jsonl")
	beforeIndex := `{"sessionId":"session-one","projectDir":"/work/old","taskName":"before","createdAt":1704067200000,"updatedAt":1704067201000}` + "\n"
	afterIndex := `{"sessionId":"session-one","projectDir":"/work/new","taskName":"after!","createdAt":1704067200000,"updatedAt":1704067201000}` + "\n"
	require.Len(t, afterIndex, len(beforeIndex))
	require.NoError(t, os.WriteFile(indexPath, []byte(beforeIndex), 0o600))

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentJunie: {root}},
		Machine:   "test",
	})
	t.Cleanup(engine.Close)

	first := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, first.Synced)
	require.Zero(t, first.Failed)

	unchanged := engine.SyncAll(t.Context(), nil)
	require.Zero(t, unchanged.Synced)
	require.Equal(t, 1, unchanged.Skipped)

	appendEvents := func(events string) {
		t.Helper()
		eventsFile, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = eventsFile.WriteString(events)
		require.NoError(t, err)
		require.NoError(t, eventsFile.Close())
	}

	beforeStat, err := os.Stat(indexPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(indexPath, []byte(afterIndex), 0o600))
	require.NoError(t, os.Chtimes(indexPath, beforeStat.ModTime(), beforeStat.ModTime()))
	afterStat, err := os.Stat(indexPath)
	require.NoError(t, err)
	require.Equal(t, beforeStat.Size(), afterStat.Size())
	require.Equal(t, beforeStat.ModTime(), afterStat.ModTime())

	metadataUpdated := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, metadataUpdated.Synced)
	require.Zero(t, metadataUpdated.Failed)
	sess, err := database.GetSessionFull(t.Context(), "junie:session-one")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "new", sess.Project)
	require.NotNil(t, sess.SessionName)
	assert.Equal(t, "after!", *sess.SessionName)
	messages, err := database.GetMessages(t.Context(), "junie:session-one", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "Hello", messages[0].Content)
	assert.Equal(t, "Draft response", messages[1].Content)

	appendEvents(
		`{"kind":"SessionA2uxEvent","taskId":"task-1","event":{"agentEvent":{"kind":"MarkdownBlockUpdatedEvent","stepId":"response-1","text":"Final response"}},"timestampMs":1704067200900}` + "\n",
	)
	contentUpdated := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, contentUpdated.Synced)
	require.Zero(t, contentUpdated.Failed)
	messages, err = database.GetMessages(t.Context(), "junie:session-one", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "Hello", messages[0].Content)
	assert.Equal(t, "Final response", messages[1].Content)
	usage, err := database.GetUsageEvents(t.Context(), "junie:session-one")
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, "claude-sonnet-4-6", usage[0].Model)
	assert.Equal(t, 100, usage[0].InputTokens)
	assert.Equal(t, 40, usage[0].OutputTokens)
	assert.Equal(t, 20, usage[0].CacheReadInputTokens)
	assert.Equal(t, 30, usage[0].CacheCreationInputTokens)
	require.NotNil(t, usage[0].Cost)
	assert.Equal(t, int64(1_250), usage[0].Cost.Microdollars)

	appendEvents(
		`{"kind":"UserMessagesDroppedFromHistory","userMessageIds":["req-1"],"timestampMs":1704067201000}` + "\n" +
			`{"kind":"SessionA2uxEvent","taskId":"task-1","event":{"agentEvent":{"kind":"MarkdownBlockUpdatedEvent","stepId":"response-1","text":""}},"timestampMs":1704067201100}` + "\n",
	)
	projectionCleared := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, projectionCleared.Synced)
	require.Zero(t, projectionCleared.Failed)
	messages, err = database.GetMessages(t.Context(), "junie:session-one", 0, 100, true)
	require.NoError(t, err)
	assert.Empty(t, messages)
	usage, err = database.GetUsageEvents(t.Context(), "junie:session-one")
	require.NoError(t, err)
	require.Len(t, usage, 1)

	unchanged = engine.SyncAll(t.Context(), nil)
	require.Zero(t, unchanged.Synced)
	require.Equal(t, 1, unchanged.Skipped)

	require.NoError(t, os.Remove(eventsPath))
	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), parser.AgentJunie, []string{root},
	))

	archived, err := database.GetSessionFull(t.Context(), "junie:session-one")
	require.NoError(t, err)
	require.NotNil(t, archived)
	assertSourceMissingState(t, archived)
	messages, err = database.GetMessages(t.Context(), "junie:session-one", 0, 100, true)
	require.NoError(t, err)
	assert.Empty(t, messages)
	usage, err = database.GetUsageEvents(t.Context(), "junie:session-one")
	require.NoError(t, err)
	require.Len(t, usage, 1)
	require.NotNil(t, usage[0].Cost)
	assert.Equal(t, int64(1_250), usage[0].Cost.Microdollars)
}

func TestSyncJuniePartialAndTitleOnlyRewrites(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session-title-only")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	eventsPath := filepath.Join(sessionDir, "events.jsonl")
	require.NoError(t, os.WriteFile(eventsPath, []byte(
		`{"kind":"UserPromptEvent","requestId":"req-1","prompt":"Remove me","timestampMs":1704067200000}`+"\n",
	), 0o600))

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentJunie: {root}},
		Machine:   "test",
	})
	t.Cleanup(engine.Close)

	first := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, first.Synced)
	require.Zero(t, first.Failed)
	messages, err := database.GetMessages(t.Context(), "junie:session-title-only", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)

	for _, rewrite := range [][]byte{
		nil,
		[]byte(`{"kind":"UnknownEvent","timestampMs":1704067200500}` + "\n"),
	} {
		require.NoError(t, os.WriteFile(eventsPath, rewrite, 0o600))
		skipped := engine.SyncAll(t.Context(), nil)
		require.Zero(t, skipped.Synced)
		require.Zero(t, skipped.Failed)
		messages, err = database.GetMessages(t.Context(), "junie:session-title-only", 0, 100, true)
		require.NoError(t, err)
		require.Len(t, messages, 1, "partial or unrecognized rewrites must preserve archived messages")
	}

	require.NoError(t, os.WriteFile(eventsPath, []byte(
		`{"kind":"UserPromptEvent","requestId":"req-1","prompt":"Remove me","timestampMs":1704067200000}`+"\n"+
			`{"kind":"UserResponseEvent","prompt":"Keep the complete append"}`+"\n"+
			`{"kind":`,
	), 0o600))
	partial := engine.SyncAll(t.Context(), nil)
	require.Zero(t, partial.Failed)
	require.Equal(t, 1, partial.Synced)
	messages, err = database.GetMessages(t.Context(), "junie:session-title-only", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "Keep the complete append", messages[1].Content)

	require.NoError(t, os.WriteFile(eventsPath, []byte(
		`{"kind":"SessionTitleSetEvent","name":"Title only","timestampMs":1704067201000}`+"\n",
	), 0o600))
	updated := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, updated.Synced)
	require.Zero(t, updated.Failed)
	messages, err = database.GetMessages(t.Context(), "junie:session-title-only", 0, 100, true)
	require.NoError(t, err)
	assert.Empty(t, messages)
	sess, err := database.GetSessionFull(t.Context(), "junie:session-title-only")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.SessionName)
	assert.Equal(t, "Title only", *sess.SessionName)
}

func TestJunieIndexChangedPathWorkIsArchiveBounded(t *testing.T) {
	for _, sessionCount := range []int{2, 128} {
		t.Run(fmt.Sprintf("sessions-%d", sessionCount), func(t *testing.T) {
			root := t.TempDir()
			indexPath := filepath.Join(root, "index.jsonl")
			writeIndex := func(targetTitle string, omitTarget bool) {
				t.Helper()
				var index strings.Builder
				for i := range sessionCount {
					if omitTarget && i == 0 {
						continue
					}
					sessionID := fmt.Sprintf("session-%03d", i)
					title := "Unchanged"
					if i == 0 {
						title = targetTitle
					}
					fmt.Fprintf(&index,
						`{"sessionId":%q,"projectDir":%q,"taskName":%q,"createdAt":1704067200000,"updatedAt":1704067201000}`+"\n",
						sessionID, "/work/"+sessionID, title,
					)
				}
				require.NoError(t, os.WriteFile(indexPath, []byte(index.String()), 0o600))
			}
			for i := range sessionCount {
				sessionID := fmt.Sprintf("session-%03d", i)
				sessionDir := filepath.Join(root, sessionID)
				require.NoError(t, os.MkdirAll(sessionDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), []byte(
					fmt.Sprintf(`{"kind":"UserPromptEvent","requestId":%q,"prompt":%q,"timestampMs":1704067200500}`+"\n", "req-"+sessionID, sessionID),
				), 0o600))
			}
			writeIndex("Before", false)

			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentJunie: {root}},
				Machine:   "test",
			})
			t.Cleanup(engine.Close)
			first := engine.SyncAll(t.Context(), nil)
			require.Equal(t, sessionCount, first.Synced)
			require.Zero(t, first.Failed)

			statusOnlyIndex := strings.ReplaceAll(
				func() string {
					var index strings.Builder
					for i := range sessionCount {
						sessionID := fmt.Sprintf("session-%03d", i)
						title := "Unchanged"
						if i == 0 {
							title = "Before"
						}
						fmt.Fprintf(&index, `{"sessionId":%q,"projectDir":%q,"taskName":%q,"createdAt":1704067200000,"updatedAt":1704067201000,"status":"RUNNING"}`+"\n", sessionID, "/work/"+sessionID, title)
					}
					return index.String()
				}(), `"status":"RUNNING"`, `"status":"DONE"`,
			)
			require.NoError(t, os.WriteFile(indexPath, []byte(statusOnlyIndex), 0o600))
			plan, err := engine.PlanChangedPathsContext(t.Context(), []string{indexPath})
			require.NoError(t, err)
			assert.Empty(t, plan.Files, "irrelevant index fields must not schedule session work")
			assert.Empty(t, plan.FallbackProviders, "classified no-op index rewrites must not trigger archive discovery")

			writeIndex("After", false)
			plan, err = engine.PlanChangedPathsContext(t.Context(), []string{indexPath})
			require.NoError(t, err)
			require.Len(t, plan.Files, 1, "one index-row change must select one session regardless of archive size")
			assert.Empty(t, plan.FallbackProviders)
			engine.writeBatchOverride = func(batch []pendingWrite, _ syncWriteMode, _ bool) (int, int, int, int) {
				return 0, 0, len(batch), 0
			}
			failed, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
			require.Error(t, err)
			require.Equal(t, 1, failed.Stats.Failed)

			plan, err = engine.PlanChangedPathsContext(t.Context(), []string{indexPath})
			require.NoError(t, err)
			assert.Empty(t, plan.Files, "unchanged index rows are not retried by the watcher")
			engine.writeBatchOverride = nil
			recovered := engine.SyncAll(t.Context(), nil)
			require.Zero(t, recovered.Failed)
			require.Equal(t, 1, recovered.Synced, "full sync must recover the failed metadata update")
			sess, err := database.GetSessionFull(t.Context(), "junie:session-000")
			require.NoError(t, err)
			require.NotNil(t, sess.SessionName)
			assert.Equal(t, "After", *sess.SessionName)

			plan, err = engine.PlanChangedPathsContext(t.Context(), []string{indexPath})
			require.NoError(t, err)
			assert.Empty(t, plan.Files, "unchanged index rows must not schedule more work")
			assert.Empty(t, plan.FallbackProviders)

			writeIndex("", true)
			plan, err = engine.PlanChangedPathsContext(t.Context(), []string{indexPath})
			require.NoError(t, err)
			require.Len(t, plan.Files, 1, "one removed index row must select one persisted session")
			result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
			require.NoError(t, err)
			require.Equal(t, 1, result.FilesProcessed)
			require.Equal(t, 1, result.Stats.Synced)
			sess, err = database.GetSessionFull(t.Context(), "junie:session-000")
			require.NoError(t, err)
			assert.Equal(t, "junie", sess.Project)
			assert.Nil(t, sess.SessionName)

			require.NoError(t, os.Remove(filepath.Join(root, "session-000", "events.jsonl")))
			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), parser.AgentJunie, []string{root},
			))
			sess, err = database.GetSessionFull(t.Context(), "junie:session-000")
			require.NoError(t, err)
			assertSourceMissingState(t, sess)
			unchanged, err := database.GetSessionFull(t.Context(), "junie:session-001")
			require.NoError(t, err)
			require.NotNil(t, unchanged)
			assert.Nil(t, unchanged.SourceMissingAt)
		})
	}
}

func TestJunieDirectSyncPreservesWatcherBaseline(t *testing.T) {
	for _, coldStart := range []bool{false, true} {
		name := "warm"
		if coldStart {
			name = "cold"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, id := range []string{"session-one", "session-two"} {
				path := filepath.Join(root, id, "events.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(
					`{"kind":"UserPromptEvent","requestId":"request","prompt":"Hello"}`+"\n",
				), 0o600))
			}
			indexPath := filepath.Join(root, "index.jsonl")
			writeIndex := func(one, two string) {
				t.Helper()
				require.NoError(t, os.WriteFile(indexPath, []byte(fmt.Sprintf(
					`{"sessionId":"session-one","taskName":%q}`+"\n"+
						`{"sessionId":"session-two","taskName":%q}`+"\n", one, two,
				)), 0o600))
			}
			writeIndex("Before", "Before")
			database := openTestDB(t)
			config := EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentJunie: {root}},
				Machine:   "test",
			}
			engine := NewEngine(t.Context(), database, config)
			t.Cleanup(func() { engine.Close() })
			require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
			if coldStart {
				engine.Close()
				engine = NewEngine(t.Context(), database, config)
			}

			writeIndex("After", "After")
			require.NoError(t, engine.SyncSingleSession("junie:session-one"))
			one, err := database.GetSessionFull(t.Context(), "junie:session-one")
			require.NoError(t, err)
			require.NotNil(t, one.SessionName)
			assert.Equal(t, "After", *one.SessionName)
			two, err := database.GetSessionFull(t.Context(), "junie:session-two")
			require.NoError(t, err)
			require.NotNil(t, two.SessionName)
			assert.Equal(t, "Before", *two.SessionName)

			plan, err := engine.PlanChangedPathsContext(t.Context(), []string{indexPath})
			require.NoError(t, err)
			require.Len(t, plan.Files, 2, "direct sync must not consume either changed index row")
			_, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
			require.NoError(t, err)
			two, err = database.GetSessionFull(t.Context(), "junie:session-two")
			require.NoError(t, err)
			require.NotNil(t, two.SessionName)
			assert.Equal(t, "After", *two.SessionName)
		})
	}
}

func TestJunieDirectSyncClearsMissingIndexMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session-one", "events.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(
		`{"kind":"UserPromptEvent","prompt":"Keep me","timestampMs":1704067201000}`+"\n",
	), 0o600))
	indexPath := filepath.Join(root, "index.jsonl")
	require.NoError(t, os.WriteFile(indexPath, []byte(
		`{"sessionId":"session-one","taskName":"Old title","projectDir":"/work/demo","createdAt":1704067200000,"updatedAt":1704067202000}`+"\n",
	), 0o600))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentJunie: {root}},
		Machine:   "test",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	require.NoError(t, os.Remove(indexPath))
	require.NoError(t, engine.SyncSingleSession("junie:session-one"))
	session, err := database.GetSessionFull(t.Context(), "junie:session-one")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Nil(t, session.SessionName)
	assert.Equal(t, "junie", session.Project)
	assert.Empty(t, session.Cwd)
	require.NotNil(t, session.StartedAt)
	require.NotNil(t, session.EndedAt)
	assert.Equal(t, "2024-01-01T00:00:01Z", *session.StartedAt)
	assert.Equal(t, "2024-01-01T00:00:01Z", *session.EndedAt)
	messages, err := database.GetMessages(t.Context(), "junie:session-one", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "Keep me", messages[0].Content)
}

func TestJunieBadIndexDoesNotBlockHealthyRoot(t *testing.T) {
	for _, tc := range []struct{ name, record string }{
		{"invalid JSON", "{"},
		{"missing session ID", `{"taskName":"Bad row"}`},
		{"oversized", `{"sessionId":"session-one","taskName":"` + strings.Repeat("x", 64*1024*1024) + `"}`},
	} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart=%t", tc.name, restart), func(t *testing.T) {
				badRoot, healthyRoot := t.TempDir(), t.TempDir()
				events := filepath.Join(badRoot, "session-one", "events.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(events), 0o755))
				require.NoError(t, os.WriteFile(events, []byte(`{"kind":"UserPromptEvent","prompt":"Keep me"}`+"\n"), 0o600))
				index := filepath.Join(badRoot, "index.jsonl")
				require.NoError(t, os.WriteFile(index, []byte(`{"sessionId":"session-one","taskName":"Original title","projectDir":"/work/demo"}`+"\n"), 0o600))
				database := openTestDB(t)
				cfg := EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentJunie: {badRoot, healthyRoot}},
					Machine:   "test",
				}
				engine := NewEngine(t.Context(), database, cfg)
				require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				if restart {
					engine.Close()
					engine = NewEngine(t.Context(), database, cfg)
				}
				t.Cleanup(engine.Close)
				require.NoError(t, os.WriteFile(index, []byte(tc.record+"\n"), 0o600))
				events = filepath.Join(healthyRoot, "session-two", "events.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(events), 0o755))
				require.NoError(t, os.WriteFile(events, []byte(`{"kind":"UserPromptEvent","prompt":"New conversation"}`+"\n"), 0o600))

				engine.SyncAll(t.Context(), nil)
				messages, err := database.GetMessages(t.Context(), "junie:session-two", 0, 100, true)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "New conversation", messages[0].Content)
				saved, err := database.GetSessionFull(t.Context(), "junie:session-one")
				require.NoError(t, err)
				require.NotNil(t, saved)
				require.NotNil(t, saved.SessionName)
				assert.Equal(t, "Original title", *saved.SessionName)
				assert.Equal(t, "demo", saved.Project)

				require.NoError(t, os.WriteFile(index, []byte(`{"sessionId":"session-one","taskName":"Repaired title"}`+"\n"), 0o600))
				engine.SyncAll(t.Context(), nil)
				saved, err = database.GetSessionFull(t.Context(), "junie:session-one")
				require.NoError(t, err)
				require.NotNil(t, saved)
				require.NotNil(t, saved.SessionName)
				assert.Equal(t, "Repaired title", *saved.SessionName)
			})
		}
	}
}
