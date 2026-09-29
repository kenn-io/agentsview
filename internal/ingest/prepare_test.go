package ingest_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/signals"
)

func TestPrepareCandidatePairsToolResultsAndFiltersCarrier(t *testing.T) {
	parsed := parser.ParseResult{
		Session: parser.ParsedSession{
			ID: "session-1", Agent: parser.AgentClaude,
		},
		Messages: []parser.ParsedMessage{
			{
				Ordinal: 2, Role: parser.RoleAssistant, Content: "checking",
				ToolCalls: []parser.ParsedToolCall{{
					ToolUseID: "call-1", ToolName: "Bash", Category: "Bash",
				}},
			},
			{
				Ordinal: 3, Role: parser.RoleUser,
				ToolResults: []parser.ParsedToolResult{{
					ToolUseID: "call-1", ContentLength: 99,
					ContentRaw: `"command output"`,
				}},
			},
		},
	}

	candidate, err := ingest.PrepareCandidate(
		t.Context(), parsed, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	require.Len(t, candidate.Messages, 1)
	require.Len(t, candidate.Messages[0].ToolCalls, 1)
	assert.Equal(t, 2, candidate.Messages[0].Ordinal,
		"filtering a result carrier must preserve parsed ordinals")
	assert.Equal(t, "command output",
		candidate.Messages[0].ToolCalls[0].ResultContent)
	assert.Equal(t, len("command output"),
		candidate.Messages[0].ToolCalls[0].ResultContentLength)
	assert.Equal(t, 1, candidate.Session.MessageCount)
}

func TestToolSequencePairedImageResults(t *testing.T) {
	for _, tt := range []struct {
		raw    string
		ending signals.ToolSequenceEnding
	}{
		{`[{"type":"text","text":"[image]"},{"type":"text","text":"[image]"}]`, signals.ToolSequenceEndingUnknown},
		{`[{"type":"text","text":"[image]"},{"type":"text","text":"file contents"}]`, signals.ToolSequenceEndingRecovered},
	} {
		messages := []db.Message{
			{Ordinal: 1, ToolCalls: []db.ToolCall{{ToolUseID: "empty", ToolName: "Grep", Category: "Grep", ResultContent: "No matches found"}}},
			{Ordinal: 2, ToolCalls: []db.ToolCall{{ToolUseID: "images", ToolName: "Read", Category: "Read"}}},
			{ToolResults: []db.ToolResult{{ToolUseID: "images", ContentRaw: tt.raw}}},
		}
		require.NoError(t, ingest.PairToolResultsContext(t.Context(), messages, nil))
		got := signals.ExtractToolSequences(ingest.ExtractToolCallRows(messages), true)
		require.Len(t, got.Sequences, 1)
		assert.Equal(t, tt.ending, got.Sequences[0].Ending, tt.raw)
	}
}

func TestFinalizeClampsRowDerivedTokens(t *testing.T) {
	parsed := parser.ParseResult{
		Session: parser.ParsedSession{
			ID: "session-1", Agent: parser.AgentVSCodeCopilot,
			TotalOutputTokens:    999_999_999,
			HasTotalOutputTokens: true,
		},
		UsageEvents: []parser.ParsedUsageEvent{{
			Source: "turn", Model: "model-1",
			InputTokens: -7, OutputTokens: 999_999_999,
		}},
	}
	candidate, err := ingest.PrepareCandidate(
		t.Context(), parsed, ingest.ContentOptions{},
	)
	require.NoError(t, err)

	prepared, err := ingest.Finalize(
		t.Context(), candidate, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	require.Len(t, prepared.UsageEvents, 1)
	assert.Zero(t, prepared.UsageEvents[0].InputTokens)
	assert.Equal(t, db.MaxPlausibleTokens,
		prepared.UsageEvents[0].OutputTokens)
	assert.Equal(t, db.MaxPlausibleTokens,
		prepared.Session.TotalOutputTokens,
		"row-derived total must follow the clamped row")
	assert.Equal(t, 2, prepared.Validation.TokensClamped)
}

func TestFinalizePreservesAuthoritativeSummaryTotals(t *testing.T) {
	const summaryTotal = 4_242_424
	const summaryPeak = 3_333_333
	parsed := parser.ParseResult{
		Session: parser.ParsedSession{
			ID: "session-1", Agent: parser.AgentHermes,
			TotalOutputTokens:    summaryTotal,
			HasTotalOutputTokens: true,
			PeakContextTokens:    summaryPeak,
			HasPeakContextTokens: true,
		},
		UsageEvents: []parser.ParsedUsageEvent{{
			Source: "session", Model: "model-1",
			InputTokens: -11, OutputTokens: 17,
		}},
	}
	candidate, err := ingest.PrepareCandidate(
		t.Context(), parsed, ingest.ContentOptions{},
	)
	require.NoError(t, err)

	prepared, err := ingest.Finalize(
		t.Context(), candidate, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	require.Len(t, prepared.UsageEvents, 1)
	assert.Zero(t, prepared.UsageEvents[0].InputTokens)
	assert.Equal(t, 17, prepared.UsageEvents[0].OutputTokens)
	assert.Equal(t, summaryTotal, prepared.Session.TotalOutputTokens)
	assert.Equal(t, summaryPeak, prepared.Session.PeakContextTokens)
}

func TestFinalizeKeepsUsageWithoutMessagesAndStampsFinalID(t *testing.T) {
	parsed := parser.ParseResult{
		Session: parser.ParsedSession{
			ID: "native-id", Agent: parser.AgentHermes,
			CountsAuthoritative: true,
		},
		UsageEvents: []parser.ParsedUsageEvent{{
			SessionID: "native-id", Source: "session", Model: "model-1",
			OutputTokens: 23,
		}},
	}
	candidate, err := ingest.PrepareCandidate(
		t.Context(), parsed, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	candidate.Session.ID = "tenant~native-id"

	prepared, err := ingest.Finalize(
		t.Context(), candidate, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	assert.Empty(t, prepared.Messages)
	require.Len(t, prepared.UsageEvents, 1)
	assert.Equal(t, "tenant~native-id", prepared.UsageEvents[0].SessionID)
	assert.Equal(t, 23, prepared.UsageEvents[0].OutputTokens)
}

func TestFinalizeProjectsStoredContentBeforeSecretFindings(t *testing.T) {
	parsed := parser.ParseResult{
		Session: parser.ParsedSession{
			ID: "session-1", Agent: parser.AgentClaude,
		},
		Messages: []parser.ParsedMessage{{
			Ordinal: 0, Role: parser.RoleAssistant, Content: "checking",
			Timestamp: time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC),
			ToolCalls: []parser.ParsedToolCall{{
				ToolUseID: "call-1", ToolName: "Bash", Category: "Bash",
				InputJSON: `{"token":"AKIA7QHWN2DKR4FYPLJM"}`,
			}},
		}},
	}

	fullCandidate, err := ingest.PrepareCandidate(
		t.Context(), parsed, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	full, err := ingest.Finalize(
		t.Context(), fullCandidate, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	require.NotEmpty(t, full.Findings,
		"the full-content control must prove the fixture is detectable")

	projectedCandidate, err := ingest.PrepareCandidate(
		t.Context(), parsed, ingest.ContentOptions{},
	)
	require.NoError(t, err)
	projected, err := ingest.Finalize(
		t.Context(), projectedCandidate, ingest.ContentOptions{
			ArchiveContent: config.ArchiveContentTranscripts,
		})
	require.NoError(t, err)
	require.Len(t, projected.Messages, 1)
	require.Len(t, projected.Messages[0].ToolCalls, 1)
	assert.Empty(t, projected.Messages[0].ToolCalls[0].InputJSON)
	assert.Empty(t, projected.Findings,
		"findings must describe only content retained by storage policy")
}

// parseCodebuffFixture parses one Codebuff session directory through the
// registered provider and returns its results keyed by session ID.
func parseCodebuffFixture(t *testing.T, chatMessages string) map[string]parser.ParseResult {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "proj", "chats", "2026-07-15T20-01-32.065Z")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "chat-messages.json"), []byte(chatMessages), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "run-state.json"),
		[]byte(`{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}`), 0o644))

	var factory parser.ProviderFactory
	for _, f := range parser.ProviderFactories() {
		if f.Definition().Type == parser.AgentCodebuff {
			factory = f
		}
	}
	require.NotNil(t, factory)
	provider := factory.NewProvider(parser.ProviderConfig{Roots: []string{root}})
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	out := map[string]parser.ParseResult{}
	for _, r := range outcome.Results {
		out[r.Result.Session.ID] = r.Result
	}
	return out
}

func toolCallsByName(messages []db.Message) map[string]db.ToolCall {
	out := map[string]db.ToolCall{}
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			out[tc.ToolName] = tc
		}
	}
	return out
}

// A Codebuff subagent's nested read_files output is an ordinary tool result
// in the subagent's own session, so the result-content policy drops it with
// no provider-specific plumbing.
func TestPrepareCandidateBlocksCodebuffSubagentReadOutput(t *testing.T) {
	results := parseCodebuffFixture(t, `[
		{"id":"ai-1","variant":"ai","timestamp":"03:04 PM","blocks":[
			{"type":"agent","agentId":"agent-1","agentType":"file-explorer",
			 "initialPrompt":"find the config","content":"config is in cfg.toml",
			 "blocks":[
				{"type":"tool","toolName":"read_files","toolCallId":"rf-1",
				 "input":{"paths":["cfg.toml"]},"output":"SECRET FILE CONTENTS"},
				{"type":"tool","toolName":"run_terminal_command","toolCallId":"sh-1",
				 "input":{"command":"ls"},"output":"file-a"}
			 ]}
		]}
	]`)
	parentID := "codebuff:proj:2026-07-15T20-01-32.065Z"
	childID := parentID + "__subagent__agent-1"
	require.Contains(t, results, parentID)
	require.Contains(t, results, childID)

	prepare := func(id string, blocked map[string]bool) map[string]db.ToolCall {
		t.Helper()
		candidate, err := ingest.PrepareCandidate(t.Context(), results[id],
			ingest.ContentOptions{BlockedResultCategories: blocked})
		require.NoError(t, err)
		return toolCallsByName(candidate.Messages)
	}

	open := prepare(childID, nil)
	assert.Equal(t, "SECRET FILE CONTENTS", open["read_files"].ResultContent,
		"without a policy the subagent's read output is stored")

	child := prepare(childID, map[string]bool{"Read": true})
	require.Contains(t, child, "read_files")
	assert.Equal(t, "Read", child["read_files"].Category)
	assert.Empty(t, child["read_files"].ResultContent,
		"a blocked Read category drops the subagent's file contents")
	assert.Equal(t, "file-a", child["run_terminal_command"].ResultContent,
		"unblocked categories keep their output")

	parent := prepare(parentID, map[string]bool{"Read": true})
	require.Contains(t, parent, "file-explorer")
	assert.Equal(t, childID, parent["file-explorer"].SubagentSessionID)
	assert.Equal(t, "config is in cfg.toml", parent["file-explorer"].ResultContent)
}
