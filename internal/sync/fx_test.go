package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func TestFxSyncProviderModelsAndSkills(t *testing.T) {
	for _, test := range []struct{ name, provider, replayProvider, wantProvider, wantReplayProvider string }{
		{"builtin", `"gateway"`, `"codex"`, "gateway", "codex"},
		{"configured", `{"name":"custom","binding":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`, `{"name":"custom-replay","binding":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`, "custom", "custom-replay"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "provider-session")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "session.json"), []byte(fmt.Sprintf(`{"schema_version":4,"model":"anthropic/claude-sonnet-4.5","provider":%s}`, test.provider)), 0o600))
			events := fmt.Sprintf(`{"schema_version":3,"seq":1,"timestamp_ms":1791839193807,"event":{"user":{"text":"Review the code"}}}
{"schema_version":3,"seq":2,"timestamp_ms":1791839194000,"event":{"tool_call":{"call_id":"skill-1","tool_name":"skill","arguments_json":"{\"location\":\"file:///tmp/skills/code%%20review\"}"}}}
{"schema_version":3,"seq":3,"timestamp_ms":1791839195000,"event":{"tool_result":{"call_id":"skill-1","status":"success","preview":"Loaded"}}}
{"schema_version":3,"seq":4,"timestamp_ms":1791839196000,"event":{"interrupted":{"partial_text":"Partial review"}}}
{"schema_version":3,"seq":5,"timestamp_ms":1791839197000,"event":{"assistant":{"text":"Reviewed","provider_replay":{"source":{"model":"anthropic/claude-sonnet-4.5","provider":%s}}}}}
{"schema_version":3,"seq":6,"timestamp_ms":1791839198000,"event":{"tool_call":{"call_id":"read-1","tool_name":"read_file","arguments_json":"{}"}}}
{"schema_version":3,"seq":7,"timestamp_ms":1791839199000,"event":{"tool_result":{"call_id":"read-1","status":"success","preview":"Contents"}}}
{"schema_version":3,"seq":8,"timestamp_ms":1791839200000,"event":{"tool_call":{"call_id":"read-2","tool_name":"read_file","arguments_json":"{}"}}}
`, test.replayProvider)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(events), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "usage-v2.json"), []byte(`{"schema_version":1,"session_id":"provider-session","snapshot":{"schema_version":3,"models":[{"model":"anthropic/claude-sonnet-4.5","input_tokens":100,"output_tokens":20,"cache_read_tokens":10}]}}`), 0o600))
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}},
				Machine:   "local", DisableSignalRecomputation: true,
			})
			t.Cleanup(engine.Close)
			engine.SyncAllForceParse(t.Context(), nil)
			messages, err := database.GetAllMessages(t.Context(), "fx:provider-session")
			require.NoError(t, err)
			require.Len(t, messages, 5)
			require.Len(t, messages[1].ToolCalls, 1)
			assert.Equal(t, "code review", messages[1].ToolCalls[0].SkillName)
			assert.Equal(t, "Partial review", messages[2].Content)
			assert.Equal(t, "Reviewed", messages[3].Content)
			assert.Equal(t, "anthropic/claude-sonnet-4.5", messages[3].Model)
			assert.Equal(t, test.wantReplayProvider, messages[3].ProviderID)
			for _, row := range []int{1, 2, 4} {
				assert.Empty(t, messages[row].Model)
				assert.Empty(t, messages[row].ProviderID)
			}
			usage, err := database.GetUsageEvents(t.Context(), "fx:provider-session")
			require.NoError(t, err)
			require.Len(t, usage, 1)
			assert.Equal(t, "anthropic/claude-sonnet-4.5", usage[0].Model)
			assert.Equal(t, "2026-10-12T21:06:40Z", usage[0].OccurredAt)
			embedded := pricingpkg.EmbeddedGenAIDocument()
			resolver := export.NewPricingResolver([]export.EffectivePricingRow{{GenAI: embedded.Prices, GenAISource: export.PricingRowSourceEmbedded}})
			at, err := time.Parse(time.RFC3339Nano, usage[0].OccurredAt)
			require.NoError(t, err)
			priced, lookup := resolver.ResolveAt(usage[0].Model, "", at)
			require.True(t, lookup.OK)
			assert.Equal(t, "anthropic/claude-sonnet-4.5", priced)
			assert.Equal(t, money.MustParseDollars("3"), lookup.Rates.InputPerMTok)
			assert.Equal(t, money.MustParseDollars("15"), lookup.Rates.OutputPerMTok)
		})
	}
}

func TestFxSyncCompanionChangesAndCompaction(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sync-session")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "events.jsonl")
	metadataPath := filepath.Join(dir, "session.json")
	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"schema_version":4,"title":"Before","created_at_ms":1735689601000,"updated_at_ms":1735689602000}`), 0o600))
	require.NoError(t, os.WriteFile(path, []byte(`{"seq":1,"timestamp_ms":1735689601000,"event":{"user":{"text":"Question"}}}
{"seq":2,"timestamp_ms":1735689602000,"event":{"assistant":{"text":"Answer"}}}
{"seq":3,"timestamp_ms":1735689602000,"event":{"tool_call":{"call_id":"read-1","tool_name":"read_file","arguments_json":"{}"}}}
{"seq":4,"timestamp_ms":1735689602000,"event":{"tool_result":{"call_id":"read-1","status":"success","preview":"File contents"}}}
{"seq":5,"timestamp_ms":1735689603000,"event":{"turn_completed":{}}}
`), 0o600))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAllForceParse(t.Context(), nil)
	assert.GreaterOrEqual(t, stats.Synced, 1)
	session, err := database.GetSessionFull(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.SessionName)
	assert.Equal(t, "Before", *session.SessionName)
	assert.Equal(t, 1, session.UserMessageCount)
	messages, err := database.GetAllMessages(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, 2, session.MessageCount)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "read-1", messages[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "File contents", messages[1].ToolCalls[0].ResultContent)
	require.Len(t, messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "completed", messages[1].ToolCalls[0].ResultEvents[0].Status)
	assert.Equal(t, "tool_result", messages[1].ToolCalls[0].ResultEvents[0].Source)
	assert.Equal(t, "File contents", messages[1].ToolCalls[0].ResultEvents[0].Content)

	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"schema_version":4,"title":"After rename","created_at_ms":1735689601000,"updated_at_ms":1735689602000}`), 0o600))
	engine.SyncAll(t.Context(), nil)
	session, err = database.GetSessionFull(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.SessionName)
	assert.Equal(t, "After rename", *session.SessionName)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "usage-v2.json"), []byte(`{"snapshot":{"models":[{"model":"gpt-6.1-sol","input_tokens":20,"cache_read_tokens":5,"cache_write_tokens":null,"output_tokens":7,"reasoning_tokens":null,"request_count":null}]}}`), 0o600))
	engine.SyncAll(t.Context(), nil)
	session, err = database.GetSessionFull(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 7, session.TotalOutputTokens)
	assert.Zero(t, session.PeakContextTokens)
	assert.False(t, session.HasPeakContextTokens)
	usage, err := database.GetUsageEvents(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, "2025-01-01T00:00:03Z", usage[0].OccurredAt)

	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"schema_version":4,"title":"After preferences","created_at_ms":1735689601000,"updated_at_ms":1735776000000}`), 0o600))
	engine.SyncAll(t.Context(), nil)
	usage, err = database.GetUsageEvents(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, "2025-01-01T00:00:03Z", usage[0].OccurredAt)
	assert.Equal(t, 15, usage[0].InputTokens)
	assert.Equal(t, 5, usage[0].CacheReadInputTokens)
	assert.Equal(t, 7, usage[0].OutputTokens)
	assert.Zero(t, usage[0].ReasoningTokens)
	assert.Zero(t, usage[0].CacheCreationInputTokens)

	transcript, err := os.ReadFile(path)
	require.NoError(t, err)
	usagePath := filepath.Join(dir, "usage-v2.json")
	usageData, err := os.ReadFile(usagePath)
	require.NoError(t, err)
	for _, test := range []struct{ name, file, content string }{
		{"broken frame", path, string(transcript) + "{broken\n"},
		{"empty event", path, "{\"event\":{}}\n"},
		{"empty event type", path, "{\"event\":{\"\":{}}}\n"},
		{"null recognized event", path, "{\"event\":{\"assistant\":null}}\n"},
		{"null usage", usagePath, `null`},
		{"missing snapshot", usagePath, `{}`},
		{"null snapshot", usagePath, `{"snapshot":null}`},
		{"null model", usagePath, `{"snapshot":{"models":[null]}}`},
		{"negative count", usagePath, `{"snapshot":{"models":[{"model":"gpt-6.1-sol","input_tokens":-1}]}}`},
		{"wrong session", usagePath, `{"session_id":"other-session","snapshot":{"models":[]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(test.file, []byte(test.content), 0o600))
			engine.SyncAll(t.Context(), nil)
			stored, err := database.GetAllMessages(t.Context(), "fx:sync-session")
			require.NoError(t, err)
			require.Len(t, stored, 2)
			assert.Equal(t, "Answer", stored[1].Content)
			storedUsage, err := database.GetUsageEvents(t.Context(), "fx:sync-session")
			require.NoError(t, err)
			require.Len(t, storedUsage, 1)
			assert.Equal(t, 7, storedUsage[0].OutputTokens)
			require.NoError(t, os.WriteFile(path, transcript, 0o600))
			require.NoError(t, os.WriteFile(usagePath, usageData, 0o600))
		})
	}

	logFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = logFile.WriteString(`{"seq":6,"timestamp_ms":1735689604000,"event":{"context_checkpoint":{"summary":"Compacted work"}}}` + "\n")
	require.NoError(t, logFile.Close())
	require.NoError(t, err)
	engine.SyncAll(t.Context(), nil)
	messages, err = database.GetAllMessages(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 3)
	assert.Equal(t, "Question", messages[0].Content)
	assert.Equal(t, "Answer", messages[1].Content)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "File contents", messages[1].ToolCalls[0].ResultContent)
	assert.Equal(t, "Compacted work", messages[2].Content)
	assert.True(t, messages[2].IsCompactBoundary)
}

func TestFxSyncChildBeforeParent(t *testing.T) {
	root := t.TempDir()
	childDir := filepath.Join(root, "a.child")
	require.NoError(t, os.MkdirAll(filepath.Join(childDir, "subagent"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "session.json"), []byte(`{"schema_version":4,"created_at_ms":1000,"updated_at_ms":2000}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "events.jsonl"), []byte(`{"schema_version":3,"timestamp_ms":2000,"event":{"user":{"text":"Child task"}}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "subagent", "owner.json"), []byte(`{"schema_version":1,"parent_id":"z.parent"}`), 0o600))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)
	engine.SyncAllForceParse(t.Context(), nil)
	child, err := database.GetSessionFull(t.Context(), "fx:a.child")
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "fx:z.parent", *child.ParentSessionID)
	assert.Equal(t, string(parser.RelSubagent), child.RelationshipType)

	parentDir := filepath.Join(root, "z.parent")
	require.NoError(t, os.MkdirAll(parentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(parentDir, "session.json"), []byte(`{"schema_version":4,"created_at_ms":1000,"updated_at_ms":2000}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(parentDir, "events.jsonl"), []byte(`{"schema_version":3,"timestamp_ms":1000,"event":{"user":{"text":"Parent task"}}}`), 0o600))
	engine.SyncAllForceParse(t.Context(), nil)
	parent, err := database.GetSessionFull(t.Context(), "fx:z.parent")
	require.NoError(t, err)
	require.NotNil(t, parent)
	child, err = database.GetSessionFull(t.Context(), "fx:a.child")
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, parent.ID, *child.ParentSessionID)
	assert.Equal(t, string(parser.RelSubagent), child.RelationshipType)
	require.NotNil(t, child.FirstMessage)
	assert.Equal(t, "Child task", *child.FirstMessage)
}

func TestFxSyncFixtureToolResults(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "parent-session")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, name := range []string{"session.json", "events.jsonl", "usage-v2.json"} {
		data, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "fx", "parent-session", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)
	engine.SyncAllForceParse(t.Context(), nil)
	messages, err := database.GetAllMessages(t.Context(), "fx:parent-session")
	require.NoError(t, err)
	require.Len(t, messages, 11)
	var statuses []string
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			require.Len(t, call.ResultEvents, 1)
			statuses = append(statuses, call.ResultEvents[0].Status)
			assert.Equal(t, "tool_result", call.ResultEvents[0].Source)
		}
	}
	assert.Equal(t, []string{"completed", "completed", "errored", "completed", "completed", "completed", "completed", "completed", "completed", "errored"}, statuses)
	usage, err := database.GetUsageEvents(t.Context(), "fx:parent-session")
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, "codex/gpt-6.1-sol", usage[0].Model)
	assert.Equal(t, "codex", usage[0].ProviderID)
	assert.Equal(t, "gpt-6.1-sol", messages[1].Model)
	assert.Equal(t, usage[0].ProviderID, messages[1].ProviderID)
}

func TestFxSyncEqualLengthCompanionRewrites(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "session-a")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	events := filepath.Join(dir, "events.jsonl")
	metadata := filepath.Join(dir, "session.json")
	usageFile := filepath.Join(dir, "usage-v2.json")
	require.NoError(t, os.WriteFile(events, []byte(`{"timestamp_ms":2000,"event":{"assistant":{"text":"Reply","provider_replay":{"source":{"model":"historical","provider":"codex"}}}}}`), 0o600))
	require.NoError(t, os.WriteFile(metadata, []byte(`{"schema_version":4,"title":"Before","model":"old"}`), 0o600))
	require.NoError(t, os.WriteFile(usageFile, []byte(`{"snapshot":{"models":[{"model":"test","output_tokens":5}]}}`), 0o600))
	newer, older := time.Unix(2000, 0), time.Unix(1000, 0)
	require.NoError(t, os.Chtimes(events, newer, newer))
	for _, file := range []string{metadata, usageFile} {
		require.NoError(t, os.Chtimes(file, older, older))
	}
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}}, Machine: "local", DisableSignalRecomputation: true})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	for _, test := range []struct {
		name, file, content, title string
		output                     int
	}{
		{"metadata", metadata, `{"schema_version":4,"title":"After!","model":"new"}`, "After!", 5},
		{"usage", usageFile, `{"snapshot":{"models":[{"model":"test","output_tokens":9}]}}`, "After!", 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(test.file, []byte(test.content), 0o600))
			require.NoError(t, os.Chtimes(test.file, older, older))
			engine.SyncAll(t.Context(), nil)
			session, err := database.GetSessionFull(t.Context(), "fx:session-a")
			require.NoError(t, err)
			require.NotNil(t, session)
			require.NotNil(t, session.SessionName)
			assert.Equal(t, test.title, *session.SessionName)
			usage, err := database.GetUsageEvents(t.Context(), "fx:session-a")
			require.NoError(t, err)
			require.Len(t, usage, 1)
			assert.Equal(t, test.output, usage[0].OutputTokens)
			messages, err := database.GetAllMessages(t.Context(), "fx:session-a")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "historical", messages[0].Model)
			assert.Equal(t, "codex", messages[0].ProviderID)
		})
	}
	stats := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Synced)
	assert.GreaterOrEqual(t, stats.Skipped, 1)
}
