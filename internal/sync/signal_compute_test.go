package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/signals"
)

func TestExtractToolCallRows(t *testing.T) {
	msgs := []db.Message{
		{Ordinal: 0, Role: "user", Content: "do stuff"},
		{
			Ordinal: 1,
			Role:    "assistant",
			ToolCalls: []db.ToolCall{
				{
					ToolName:      "Bash",
					Category:      "Bash",
					InputJSON:     `{"command":"ls"}`,
					ResultContent: "/tmp",
					ResultEvents: []db.ToolResultEvent{
						{Status: "completed", EventIndex: 0},
					},
				},
				{
					ToolName:      "Edit",
					Category:      "Edit",
					InputJSON:     `{"file":"/a.go"}`,
					ResultContent: "ok",
					// Multiple events: latest wins.
					ResultEvents: []db.ToolResultEvent{
						{Status: "running", EventIndex: 0},
						{Status: "errored", EventIndex: 1},
					},
				},
			},
		},
		{
			Ordinal: 2,
			Role:    "assistant",
			ToolCalls: []db.ToolCall{
				{
					ToolName:  "Read",
					Category:  "Read",
					InputJSON: `{"file":"/b.go"}`,
				},
			},
		},
	}

	got := extractToolCallRows(msgs)
	want := []signals.ToolCallRow{
		{
			ToolName:       "Bash",
			Category:       "Bash",
			InputJSON:      `{"command":"ls"}`,
			ResultContent:  "/tmp",
			MessageOrdinal: 1,
			CallIndex:      0,
			EventStatus:    "completed",
		},
		{
			ToolName:       "Edit",
			Category:       "Edit",
			InputJSON:      `{"file":"/a.go"}`,
			ResultContent:  "ok",
			MessageOrdinal: 1,
			CallIndex:      1,
			EventStatus:    "errored",
		},
		{
			ToolName:       "Read",
			Category:       "Read",
			InputJSON:      `{"file":"/b.go"}`,
			MessageOrdinal: 2,
			CallIndex:      0,
		},
	}
	assert.Equal(t, want, got)
}

func TestExtractContextTokens(t *testing.T) {
	msgs := []db.Message{
		{Ordinal: 0, Role: "user"},
		{
			Ordinal: 1, Role: "assistant",
			ContextTokens: 1000, HasContextTokens: true,
		},
		{Ordinal: 2, Role: "user"},
		{
			Ordinal: 3, Role: "assistant",
			ContextTokens: 2000, HasContextTokens: true,
		},
		// Zero/missing tokens are still emitted (caller cares).
		{Ordinal: 4, Role: "assistant"},
	}
	got := extractContextTokens(msgs)
	want := []signals.ContextTokenRow{
		{ContextTokens: 1000, HasContextTokens: true},
		{ContextTokens: 2000, HasContextTokens: true},
		{ContextTokens: 0, HasContextTokens: false},
	}
	assert.Equal(t, want, got)
}

func TestExtractCompactBoundaryOrdinals(t *testing.T) {
	msgs := []db.Message{
		{Ordinal: 0, Role: "user"},
		{Ordinal: 1, Role: "assistant"},
		{Ordinal: 2, Role: "user", IsCompactBoundary: true},
		{Ordinal: 3, Role: "assistant"},
		{Ordinal: 4, Role: "user", IsCompactBoundary: true},
	}
	got := extractCompactBoundaryOrdinals(msgs)
	want := []int{2, 4}
	assert.Equal(t, want, got)

	assert.Nil(t, extractCompactBoundaryOrdinals(nil), "extractCompactBoundaryOrdinals(nil) should return nil")
}

func TestExtractMostCommonModel(t *testing.T) {
	msgs := []db.Message{
		{Role: "user"},
		{Role: "assistant", Model: "claude-sonnet-4-5"},
		{Role: "assistant", Model: "claude-sonnet-4-5"},
		{Role: "assistant", Model: "claude-opus-4-6"},
		{Role: "assistant", Model: ""}, // ignored
	}
	assert.Equal(t, "claude-sonnet-4-5", extractMostCommonModel(msgs))

	// Tie broken by chronological-first.
	tied := []db.Message{
		{Role: "assistant", Model: "claude-sonnet-4-5"},
		{Role: "assistant", Model: "claude-opus-4-6"},
	}
	assert.Equal(t, "claude-sonnet-4-5", extractMostCommonModel(tied), "tied")

	assert.Empty(t, extractMostCommonModel(nil), "empty")
}

func TestExtractLastMessageRole(t *testing.T) {
	msgs := []db.Message{
		{Ordinal: 0, Role: "user", Content: "hi"},
		{Ordinal: 1, Role: "assistant", Content: "hello"},
		{Ordinal: 2, Role: "user", Content: "thanks"},
		{Ordinal: 3, Role: "user", Content: "system noise", IsSystem: true},
	}
	role, content := extractLastMessageRole(msgs)
	assert.Equal(t, "user", role)
	assert.Equal(t, "thanks", content)

	role, content = extractLastMessageRole(nil)
	assert.Empty(t, role, "nil case role")
	assert.Empty(t, content, "nil case content")
}

func TestComputeSignalsFromMessages_Errors(t *testing.T) {
	// Session with a final tool failure: outcome should be
	// "errored" (recent enough to be pending), penalties should
	// reflect the failure streak, and HasToolCalls is true.
	endedAt := "2099-12-31T00:00:00Z"
	sess := db.Session{
		ID:                "s1",
		MessageCount:      4,
		EndedAt:           &endedAt,
		PeakContextTokens: 50_000,
	}
	msgs := []db.Message{
		{Ordinal: 0, Role: "user", Content: "go"},
		{
			Ordinal: 1, Role: "assistant", Model: "claude-sonnet-4-5",
			ContextTokens: 10_000, HasContextTokens: true,
			ToolCalls: []db.ToolCall{{
				ToolName: "Bash", Category: "Bash",
				ResultEvents: []db.ToolResultEvent{
					{Status: "errored", EventIndex: 0},
				},
			}},
		},
		{
			Ordinal: 2, Role: "assistant", Model: "claude-sonnet-4-5",
			ContextTokens: 12_000, HasContextTokens: true,
			ToolCalls: []db.ToolCall{{
				ToolName: "Bash", Category: "Bash",
				ResultEvents: []db.ToolResultEvent{
					{Status: "errored", EventIndex: 0},
				},
			}},
		},
		{Ordinal: 3, Role: "assistant", Content: "I give up"},
	}

	got := computeSignalsFromMessages(sess, msgs)

	assert.True(t, got.HasToolCalls, "HasToolCalls = false, want true")
	assert.True(t, got.HasContextData, "HasContextData = false, want true")
	assert.NotZero(t, got.ToolFailureSignalCount, "ToolFailureSignalCount = 0, want > 0")
	assert.NotZero(t, got.FinalFailureStreak, "FinalFailureStreak = 0, want > 0")
	require.NotNil(t, got.HealthScore, "HealthScore is nil; want a value")
	assert.Less(t, *got.HealthScore, 100, "HealthScore = %d, want < 100", *got.HealthScore)
	require.NotNil(t, got.HealthGrade, "HealthGrade = nil, want non-empty")
	assert.NotEmpty(t, *got.HealthGrade, "HealthGrade = %v, want non-empty", got.HealthGrade)
	assert.Equal(t, "assistant", got.EndedWithRole)
}

func TestComputeSignalsFromMessages_ExplicitBoundariesOverrideHeuristic(t *testing.T) {
	// Two explicit boundaries should win over zero token-drops.
	sess := db.Session{ID: "s1", MessageCount: 5}
	msgs := []db.Message{
		{Ordinal: 0, Role: "user"},
		{Ordinal: 1, Role: "assistant", Model: "claude-sonnet-4-5"},
		{Ordinal: 2, Role: "user", IsCompactBoundary: true},
		{Ordinal: 3, Role: "assistant", Model: "claude-sonnet-4-5"},
		{Ordinal: 4, Role: "user", IsCompactBoundary: true},
	}
	got := computeSignalsFromMessages(sess, msgs)
	assert.Equal(t, 2, got.CompactionCount)
}

func TestSignalsIgnoreToolResultPrompts(t *testing.T) {
	messages := []db.Message{
		{Ordinal: 0, Role: "user", Content: "help"},
		{Ordinal: 1, Role: "assistant", Content: "Finished successfully."},
		{Ordinal: 2, Role: "user", SourceSubtype: "tool_result", Content: "WHY IS THIS STILL BROKEN"},
	}
	got := computeSignalsFromMessages(db.Session{MessageCount: 3}, messages)
	assert.Equal(t, "assistant", got.EndedWithRole)
	assert.Equal(t, 1, got.QualitySignals.ShortPromptCount)
	assert.Zero(t, signals.CountFrustrationMarkers(extractHeuristicMessages(messages)))
	orphan := computeSignalsFromMessages(db.Session{MessageCount: 1}, messages[2:])
	assert.Empty(t, orphan.EndedWithRole)
	assert.Zero(t, orphan.QualitySignals.ShortPromptCount)
}
