package sync_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestUsageOnlyStoragePreservesUsageWithoutTranscriptContent(t *testing.T) {
	codexRoot := t.TempDir()
	sessionID := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	path := filepath.Join(
		codexRoot,
		"2026", "08", "31",
		"rollout-2026-08-31T10-00-00-"+sessionID+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			sessionID, "/workspace/private-project", "codex_cli_rs",
			"2026-08-31T10:00:00Z",
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.5", "2026-08-31T10:00:01Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "private prompt that must stay in the source transcript",
			"2026-08-31T10:00:02Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call-private",
			map[string]any{"cmd": "read /workspace/private-project/secret.txt"},
			"2026-08-31T10:00:03Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call-private", "private tool output",
			"2026-08-31T10:00:04Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2026-08-31T10:00:05Z", 100_000, 250, 64_000,
		),
	)), 0o600))

	claudeRoot := t.TempDir()
	claudeSessionID := "claude-private-session"
	claudePath := filepath.Join(
		claudeRoot, "private-project", claudeSessionID+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(claudePath), 0o755))
	claudeBuilder := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			"2026-08-31T11:00:00Z",
			"You are a code reviewer. Review the code changes shown below. private Claude prompt",
			claudeSessionID,
			"/workspace/private-project",
		).
		AddClaudeAssistant(
			"2026-08-31T11:00:00.500Z",
			"private unbilled Claude response",
		).
		AddClaudeAssistantUsage(
			"2026-08-31T11:00:01Z",
			"private Claude response",
			testjsonl.ClaudeAssistantUsage{
				MessageID:    "msg-private-1",
				RequestID:    "req-private-1",
				Model:        "claude-sonnet-4-6",
				InputTokens:  1_000,
				OutputTokens: 200,
			},
		)
	require.NoError(t, os.WriteFile(
		claudePath, []byte(claudeBuilder.String()), 0o600,
	))

	agentDirs := map[parser.AgentType][]string{
		parser.AgentClaude: {claudeRoot},
		parser.AgentCodex:  {codexRoot},
	}
	fullDB := dbtest.OpenTestDB(t)
	fullEngine := sync.NewEngine(fullDB, sync.EngineConfig{
		AgentDirs: agentDirs,
		Machine:   "local",
	})
	t.Cleanup(fullEngine.Close)
	usageDB := dbtest.OpenTestDB(t)
	usageEngine := sync.NewEngine(usageDB, sync.EngineConfig{
		AgentDirs:      agentDirs,
		Machine:        "local",
		ArchiveContent: config.ArchiveContentUsage,
	})
	t.Cleanup(usageEngine.Close)

	require.Equal(t, 2, fullEngine.SyncAll(t.Context(), nil).Synced)
	require.Equal(t, 2, usageEngine.SyncAll(t.Context(), nil).Synced)

	appendBuilder := testjsonl.NewSessionBuilder().AddClaudeAssistantUsage(
		"2026-08-31T11:00:02Z",
		"private incrementally appended Claude response",
		testjsonl.ClaudeAssistantUsage{
			MessageID:    "msg-private-2",
			RequestID:    "req-private-2",
			Model:        "claude-sonnet-4-6",
			InputTokens:  1_250,
			OutputTokens: 250,
		},
	)
	appendFile, err := os.OpenFile(claudePath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = appendFile.WriteString(appendBuilder.String())
	require.NoError(t, err)
	require.NoError(t, appendFile.Close())
	fullEngine.SyncPathsContext(t.Context(), []string{claudePath})
	usageEngine.SyncPathsContext(t.Context(), []string{claudePath})
	resyncStats := usageEngine.ResyncAll(t.Context(), nil)
	require.False(t, resyncStats.Aborted)
	require.Zero(t, resyncStats.Failed)

	for _, agent := range []string{"claude", "codex"} {
		filter := db.UsageFilter{
			From: "2026-08-31", To: "2026-08-31",
			Agent: agent, Timezone: "UTC", Breakdowns: true,
		}
		fullUsage, err := fullDB.GetDailyUsage(context.Background(), filter)
		require.NoError(t, err)
		usageOnlyUsage, err := usageDB.GetDailyUsage(
			context.Background(), filter,
		)
		require.NoError(t, err)
		assert.Equal(t, fullUsage.Totals, usageOnlyUsage.Totals)
		assert.Equal(t, fullUsage.Daily, usageOnlyUsage.Daily)
		assert.Equal(t, fullUsage.SessionCounts, usageOnlyUsage.SessionCounts)

		fullMatching, err := fullDB.GetUsageMatchingSessionCount(
			context.Background(), filter,
		)
		require.NoError(t, err)
		usageOnlyMatching, err := usageDB.GetUsageMatchingSessionCount(
			context.Background(), filter,
		)
		require.NoError(t, err)
		assert.Equal(t, fullMatching, usageOnlyMatching)

		automatedFilter := filter
		automatedFilter.AutomatedScope = "automated"
		fullAutomated, err := fullDB.GetDailyUsage(
			context.Background(), automatedFilter,
		)
		require.NoError(t, err)
		usageOnlyAutomated, err := usageDB.GetDailyUsage(
			context.Background(), automatedFilter,
		)
		require.NoError(t, err)
		assert.Equal(t, fullAutomated, usageOnlyAutomated)
	}

	messages, err := usageDB.GetAllMessages(context.Background(), "codex:"+sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	for _, message := range messages {
		assert.Empty(t, message.Content)
		assert.Empty(t, message.ThinkingText)
		assert.Empty(t, message.ToolCalls)
		assert.Empty(t, message.ToolResults)
	}

	session, err := usageDB.GetSessionFull(
		context.Background(), "codex:"+sessionID,
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Nil(t, session.FirstMessage)
	assert.Nil(t, session.DisplayName)
	assert.Nil(t, session.SessionName)
	assert.Equal(t, 0, session.SecretLeakCount)

	fullClaudeMessages, err := fullDB.GetAllMessages(
		context.Background(), claudeSessionID,
	)
	require.NoError(t, err)
	require.NotEmpty(t, fullClaudeMessages)
	var fullClaudeText strings.Builder
	for _, message := range fullClaudeMessages {
		fullClaudeText.WriteString(message.Content)
	}
	assert.Contains(t, fullClaudeText.String(), "private Claude prompt")
	assert.Contains(t, fullClaudeText.String(), "incrementally appended")

	usageClaudeMessages, err := usageDB.GetAllMessages(
		context.Background(), claudeSessionID,
	)
	require.NoError(t, err)
	require.Less(t, len(usageClaudeMessages), len(fullClaudeMessages),
		"usage-only storage must omit rows unrelated to accounting")
	for _, message := range usageClaudeMessages {
		assert.True(t,
			(message.TokenUsage != nil && message.Model != "" &&
				message.Model != "<synthetic>") ||
				(message.Role == "assistant" && message.Model != "<synthetic>"),
			"stored message %d is unrelated to usage accounting", message.Ordinal,
		)
		assert.Empty(t, message.Content)
		assert.Empty(t, message.ThinkingText)
		assert.Empty(t, message.ToolCalls)
		assert.Empty(t, message.ToolResults)
	}
}

func TestUsageOnlyStorageClaudeUserAppendStaysIncremental(t *testing.T) {
	claudeRoot := t.TempDir()
	sessionID := "usage-only-incremental-claude"
	path := filepath.Join(claudeRoot, "project", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	initial := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			"2026-08-31T10:00:00Z", "initial private prompt",
			sessionID, "/workspace/project",
		).
		AddClaudeAssistantUsage(
			"2026-08-31T10:00:01Z", "initial private response",
			testjsonl.ClaudeAssistantUsage{
				MessageID: "msg-initial", RequestID: "req-initial",
				Model: "claude-sonnet-4-6", InputTokens: 100, OutputTokens: 10,
			},
		)
	require.NoError(t, os.WriteFile(path, []byte(initial.String()), 0o600))

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeRoot},
		},
		Machine: "local", ArchiveContent: config.ArchiveContentUsage,
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	appendFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = appendFile.WriteString(
		testjsonl.ClaudeUserJSON(
			"second private prompt", "2026-08-31T10:00:02Z",
		) + "\n",
	)
	require.NoError(t, err)
	require.NoError(t, appendFile.Close())
	engine.SyncPathsContext(t.Context(), []string{path})

	stored, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.LastWriteIncremental,
		"discarded previews cannot force a full parse on every user append")
	assert.Equal(t, 2, stored.UserMessageCount)
	assert.Nil(t, stored.FirstMessage)
}

func TestUsageOnlyStorageSettlesLegacySignalBackfillOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-usage.db")
	seedDatabase, err := db.Open(path)
	require.NoError(t, err)
	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, seedDatabase.UpsertSession(db.Session{
		ID: "legacy-signals", Project: "project", Agent: "claude",
		Machine: "local", StartedAt: &startedAt, MessageCount: 1,
	}))
	require.NoError(t, seedDatabase.InsertMessages([]db.Message{{
		SessionID: "legacy-signals", Ordinal: 0, Role: "assistant",
		Model: "model-a", TokenUsage: []byte(`{"input_tokens":10,"output_tokens":2}`),
	}}))
	require.NoError(t, seedDatabase.UpdateSessionSignals(
		"legacy-signals", db.SessionSignalUpdate{
			ToolFailureSignalCount: 3,
			Outcome:                "failure",
			QualitySignals: db.QualitySignals{
				Version:          0,
				ShortPromptCount: 2,
			},
		},
	))
	require.NoError(t, seedDatabase.ReplaceSessionSecretFindings(
		"legacy-signals", nil, 2, "legacy-rules",
	))
	require.NoError(t, seedDatabase.Close())

	database, err := db.OpenWithArchiveContent(path, config.ArchiveContentUsage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	engine := sync.NewEngine(database, sync.EngineConfig{ArchiveContent: config.ArchiveContentUsage})
	t.Cleanup(engine.Close)
	compute := engine.BackfillSignalComputer()
	calls := 0
	runBackfill := func() {
		require.NoError(t, database.BackfillSignals(
			t.Context(), func(ctx context.Context, sessionID string) error {
				calls++
				return compute(ctx, sessionID)
			},
		))
	}

	runBackfill()
	require.Equal(t, 1, calls)
	stored, err := database.GetSessionFull(t.Context(), "legacy-signals")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, db.CurrentQualitySignalVersion, stored.QualitySignalVersion)
	assert.Zero(t, stored.ToolFailureSignalCount)
	assert.Empty(t, stored.Outcome)
	assert.Zero(t, stored.ShortPromptCount)
	assert.Zero(t, stored.SecretLeakCount)
	assert.Empty(t, stored.SecretsRulesVersion)

	calls = 0
	runBackfill()
	assert.Zero(t, calls,
		"the current marker must keep later startups from revisiting the row")
}

func TestUsageOnlyStoragePreservesNestedToolLinkedSubagentUsage(
	t *testing.T,
) {
	fullDB := dbtest.OpenTestDB(t)
	usageDB := dbtest.OpenTestDB(t)
	usageDB.SetArchiveContent(config.ArchiveContentUsage)

	for _, database := range []*db.DB{fullDB, usageDB} {
		seedUsageOnlySubagentUsage(t, database)
		require.NoError(t, database.LinkSubagentSessions())
	}

	fullUsage, err := service.SessionUsageWithSubagents(
		t.Context(), fullDB, "root", true,
	)
	require.NoError(t, err)
	require.NotNil(t, fullUsage)
	usageOnly, err := service.SessionUsageWithSubagents(
		t.Context(), usageDB, "root", true,
	)
	require.NoError(t, err)
	require.NotNil(t, usageOnly)

	require.Equal(t, 2, fullUsage.SubagentCount)
	assert.Equal(t, fullUsage, usageOnly,
		"content compaction must preserve nested delegated token and cost totals")

	for _, tc := range []struct {
		sessionID string
		childID   string
	}{
		{sessionID: "root", childID: "child"},
		{sessionID: "child", childID: "grandchild"},
	} {
		messages, err := usageDB.GetAllMessages(t.Context(), tc.sessionID)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		require.Len(t, messages[0].ToolCalls, 1)
		call := messages[0].ToolCalls[0]
		assert.Equal(t, tc.childID, call.SubagentSessionID)
		assert.Equal(t, "subagent", call.ToolName)
		assert.Equal(t, "Task", call.Category)
		assert.Empty(t, call.InputJSON)
		assert.Empty(t, call.ResultContent)
		assert.Empty(t, call.ResultEvents)
	}
}

func seedUsageOnlySubagentUsage(t *testing.T, database *db.DB) {
	t.Helper()
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "test-model",
		InputPerMTok:  money.MustParseDollars("2"),
		OutputPerMTok: money.MustParseDollars("10"),
	}}), "usage-only nested fixture")

	startedAt := "2026-08-31T10:00:00Z"
	for index, fixture := range []struct {
		id     string
		input  int
		output int
		child  string
	}{
		{id: "root", input: 1_000, output: 100, child: "child"},
		{id: "child", input: 2_000, output: 200, child: "grandchild"},
		{id: "grandchild", input: 3_000, output: 300},
	} {
		require.NoError(t, database.UpsertSession(db.Session{
			ID: fixture.id, Project: "project", Agent: "claude", Machine: "local",
			StartedAt: &startedAt, EndedAt: &startedAt, MessageCount: 1,
			TotalOutputTokens: fixture.output, HasTotalOutputTokens: true,
			PeakContextTokens: fixture.input, HasPeakContextTokens: true,
		}))

		message := db.Message{
			SessionID: fixture.id, Ordinal: 0, Role: "assistant",
			Timestamp: fmt.Sprintf("2026-08-31T10:00:0%dZ", index),
			Model:     "test-model", Content: "private delegated response",
			ClaudeMessageID: "message-" + fixture.id,
			ClaudeRequestID: "request-" + fixture.id,
			TokenUsage: []byte(fmt.Sprintf(
				`{"input_tokens":%d,"output_tokens":%d}`,
				fixture.input, fixture.output,
			)),
		}
		if fixture.child != "" {
			message.HasToolUse = true
			message.ToolCalls = []db.ToolCall{{
				ToolName: "Agent", Category: "Task",
				ToolUseID:           "tool-use-" + fixture.id,
				InputJSON:           `{"prompt":"private delegated prompt"}`,
				ResultContent:       "private delegated result",
				ResultContentLength: 24,
				SubagentSessionID:   fixture.child,
				ResultEvents: []db.ToolResultEvent{{
					SubagentSessionID: fixture.child,
					Content:           "private event content",
				}},
			}}
		}
		require.NoError(t, database.InsertMessages([]db.Message{message}))
	}
}

func writeTranscriptsFixture(t *testing.T, root, sessionID string) string {
	t.Helper()
	path := filepath.Join(root, "project", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			"2026-08-31T10:00:00Z", "please run the tests",
			sessionID, "/workspace/project",
		).
		AddRaw(testjsonl.ClaudeAssistantJSON(
			[]map[string]any{{
				"type":  "tool_use",
				"id":    "toolu_1",
				"name":  "Bash",
				"input": map[string]string{"command": "make test"},
			}},
			"2026-08-31T10:00:01Z",
		)).
		AddRaw(testjsonl.ClaudeToolResultUserJSON(
			"toolu_1", "make: command not found", "2026-08-31T10:00:02Z",
		)).
		AddClaudeAssistantUsage(
			"2026-08-31T10:00:03Z", "the build tool is missing",
			testjsonl.ClaudeAssistantUsage{
				MessageID: "msg-1", RequestID: "req-1",
				Model: "claude-sonnet-4-6", InputTokens: 100, OutputTokens: 10,
			},
		).
		String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func toolCallsOf(messages []db.Message) []db.ToolCall {
	var calls []db.ToolCall
	for _, message := range messages {
		calls = append(calls, message.ToolCalls...)
	}
	return calls
}

func joinedContent(messages []db.Message) string {
	var text strings.Builder
	for _, message := range messages {
		text.WriteString(message.Content)
		text.WriteString("\n")
	}
	return text.String()
}

func TestTranscriptsArchiveContentSyncKeepsTranscriptWithoutToolPayloads(
	t *testing.T,
) {
	claudeRoot := t.TempDir()
	const sessionID = "transcripts-claude-session"
	writeTranscriptsFixture(t, claudeRoot, sessionID)

	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeRoot},
		},
		Machine:        "local",
		ArchiveContent: config.ArchiveContentTranscripts,
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	stored, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.NotNil(t, stored.FirstMessage)
	assert.Equal(t, "please run the tests", *stored.FirstMessage)

	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	joined := joinedContent(messages)
	assert.Contains(t, joined, "the build tool is missing")
	assert.Contains(t, joined, "[Bash]",
		"the transcript still shows that a tool ran")
	assert.NotContains(t, joined, "make test",
		"the command itself is a tool input and leaves the archive")
	calls := toolCallsOf(messages)
	require.Len(t, calls, 1)
	assert.Equal(t, "Bash", calls[0].ToolName)
	assert.Empty(t, calls[0].InputJSON)
	assert.Empty(t, calls[0].ResultContent)
	assert.Equal(t, len("make: command not found"), calls[0].ResultContentLength)

	// Signals come from the rows as stored, so a recompute from the archive
	// reproduces the write-time values instead of resetting them.
	assert.Zero(t, stored.ToolFailureSignalCount,
		"failure text is not stored, so it cannot count at write time")
	require.NoError(t, engine.BackfillSignalComputer()(t.Context(), sessionID))
	recomputed, err := database.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, recomputed)
	assert.Equal(t, stored.ToolFailureSignalCount, recomputed.ToolFailureSignalCount)
	assert.Equal(t, stored.Outcome, recomputed.Outcome)
}

// TestResyncProjectsArchivedSessionsOntoArchiveContent switches an existing
// full archive to a narrower policy and rebuilds it. The session's source file
// is gone, so the rebuild restores it through the orphan copy path rather than
// the parser, and that path must apply the same projection.
func TestResyncProjectsArchivedSessionsOntoArchiveContent(t *testing.T) {
	for _, policy := range []config.ArchiveContent{
		config.ArchiveContentTranscripts, config.ArchiveContentUsage,
	} {
		t.Run(string(policy), func(t *testing.T) {
			claudeRoot := t.TempDir()
			sessionID := "archived-" + string(policy)
			sourcePath := writeTranscriptsFixture(t, claudeRoot, sessionID)
			// A rebuild refuses to swap in an empty archive, so keep one live
			// session on disk beside the one whose source disappears.
			writeTranscriptsFixture(t, claudeRoot, "live-"+string(policy))
			agentDirs := map[parser.AgentType][]string{
				parser.AgentClaude: {claudeRoot},
			}

			dbPath := filepath.Join(t.TempDir(), "sessions.db")
			full, err := db.Open(dbPath)
			require.NoError(t, err)
			fullEngine := sync.NewEngine(full, sync.EngineConfig{
				AgentDirs: agentDirs, Machine: "local",
			})
			require.Equal(t, 2, fullEngine.SyncAll(t.Context(), nil).Synced)
			fullEngine.Close()
			require.NoError(t, full.Close())
			require.NoError(t, os.Remove(sourcePath))

			database, err := db.OpenWithArchiveContent(dbPath, policy)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			engine := sync.NewEngine(database, sync.EngineConfig{
				AgentDirs: agentDirs, Machine: "local", ArchiveContent: policy,
			})
			t.Cleanup(engine.Close)
			stats := engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "warnings: %v", stats.Warnings)
			require.Equal(t, 1, stats.OrphanedCopied)

			stored, err := database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, stored, "archived session must survive the rebuild")
			messages, err := database.GetAllMessages(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotEmpty(t, messages)
			calls := toolCallsOf(messages)

			switch policy {
			case config.ArchiveContentTranscripts:
				require.NotNil(t, stored.FirstMessage)
				assert.Contains(t, joinedContent(messages), "the build tool is missing")
				require.Len(t, calls, 1)
				assert.Empty(t, calls[0].InputJSON)
				assert.Empty(t, calls[0].ResultContent)
				assert.Equal(t, len("make: command not found"), calls[0].ResultContentLength)
			case config.ArchiveContentUsage:
				assert.Nil(t, stored.FirstMessage)
				assert.Empty(t, calls)
				billed := 0
				for _, message := range messages {
					assert.Empty(t, message.Content, "ordinal %d", message.Ordinal)
					assert.Equal(t, "assistant", message.Role, "ordinal %d", message.Ordinal)
					if len(message.TokenUsage) > 0 {
						billed++
					}
				}
				assert.Equal(t, 1, billed)
			}
		})
	}
}

// TestResyncCopiesDerivedTextOnlyOutsideUsagePolicy covers insights and
// recall entries, which resync copies from the old archive rather than
// rebuilding from source files. Both carry transcript-derived text.
func TestResyncCopiesDerivedTextOnlyOutsideUsagePolicy(t *testing.T) {
	for _, tc := range []struct {
		policy config.ArchiveContent
		kept   bool
	}{
		{policy: config.ArchiveContentTranscripts, kept: true},
		{policy: config.ArchiveContentUsage, kept: false},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			claudeRoot := t.TempDir()
			sessionID := "derived-" + string(tc.policy)
			writeTranscriptsFixture(t, claudeRoot, sessionID)
			agentDirs := map[parser.AgentType][]string{
				parser.AgentClaude: {claudeRoot},
			}

			dbPath := filepath.Join(t.TempDir(), "sessions.db")
			full, err := db.Open(dbPath)
			require.NoError(t, err)
			fullEngine := sync.NewEngine(full, sync.EngineConfig{
				AgentDirs: agentDirs, Machine: "local",
			})
			require.Equal(t, 1, fullEngine.SyncAll(t.Context(), nil).Synced)
			fullEngine.Close()
			_, err = full.InsertInsight(db.Insight{
				Type: "daily", DateFrom: "2026-08-31", DateTo: "2026-08-31",
				Agent: "claude", Content: "summary quoting private transcript text",
			})
			require.NoError(t, err)
			_, err = full.InsertRecallEntry(db.RecallEntry{
				ID: "entry-1", Type: "fact", Scope: "project", Status: "accepted",
				Title: "build tool", Body: "the build tool is missing",
				Project: "project", Agent: "claude", SourceSessionID: sessionID,
			})
			require.NoError(t, err)
			require.NoError(t, full.Close())

			database, err := db.OpenWithArchiveContent(dbPath, tc.policy)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			engine := sync.NewEngine(database, sync.EngineConfig{
				AgentDirs: agentDirs, Machine: "local", ArchiveContent: tc.policy,
			})
			t.Cleanup(engine.Close)
			stats := engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "warnings: %v", stats.Warnings)

			insights, err := database.ListInsights(t.Context(), db.InsightFilter{})
			require.NoError(t, err)
			entry, err := database.GetRecallEntry(t.Context(), "entry-1")
			require.NoError(t, err)
			if tc.kept {
				assert.Len(t, insights, 1)
				assert.NotNil(t, entry)
			} else {
				assert.Empty(t, insights)
				assert.Nil(t, entry)
			}
		})
	}
}
