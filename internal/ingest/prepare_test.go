package ingest_test

import (
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
