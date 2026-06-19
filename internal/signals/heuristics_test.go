package signals

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAnalyzeHeuristics_PromptQuality(t *testing.T) {
	tests := []struct {
		name string
		in   HeuristicInput
		want HeuristicSignals
	}{
		{
			name: "ignores short control prompts",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{Role: "user", Content: "yes"},
				{Role: "user", Content: "continue"},
			}},
			want: HeuristicSignals{},
		},
		{
			name: "counts only short task-start prompts",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{Role: "user", Content: "fix bug"},
				{Role: "user", Content: "add tests"},
			}},
			want: HeuristicSignals{
				ShortPromptCount:            1,
				UnstructuredStart:           true,
				MissingSuccessCriteriaCount: 1,
				NoCodeContextCount:          1,
			},
		},
		{
			name: "structured first prompt avoids start penalty",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "Fix internal/signals/score.go\n\n" +
						"- Must preserve existing grades\n" +
						"- Run go test ./internal/signals\n" +
						"Expected result: tests pass",
				},
			}},
			want: HeuristicSignals{},
		},
		{
			name: "code task missing verification language",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "Implement the backend scorer in the " +
						"codebase. Success means the score changes " +
						"only for repeated prompts.",
				},
			}},
			want: HeuristicSignals{
				MissingVerificationCount: 1,
				NoCodeContextCount:       1,
			},
		},
		{
			name: "non code conversation is not penalized",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "What are useful ways to think about " +
						"technical debt in a planning meeting?",
				},
			}},
			want: HeuristicSignals{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnalyzeHeuristics(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAnalyzeHeuristics_ShortStartsIgnoreRecentSteering(t *testing.T) {
	in := HeuristicInput{Messages: []HeuristicMessage{
		{
			Role:      "user",
			Content:   "Please fix the parser bug in internal/parser.go.",
			Timestamp: "2026-05-27T10:00:00Z",
		},
		{
			Role:      "assistant",
			Content:   "I changed the parser.",
			Timestamp: "2026-05-27T10:05:00Z",
		},
		{
			Role:      "user",
			Content:   "add tests",
			Timestamp: "2026-05-27T10:06:00Z",
		},
		{
			Role:      "assistant",
			Content:   "Done.",
			Timestamp: "2026-05-27T10:10:00Z",
		},
		{
			Role:      "user",
			Content:   "fix docs",
			Timestamp: "2026-05-27T11:00:01Z",
		},
	}}

	got := AnalyzeHeuristics(in)
	assert.Equal(t, 1, got.ShortPromptCount)
}

func TestAnalyzeHeuristics_RepeatedPrompts(t *testing.T) {
	in := HeuristicInput{Messages: []HeuristicMessage{
		{
			Role: "user",
			Content: "Please fix the failing tests in the backend " +
				"scorer and keep the changes small.",
		},
		{
			Role:    "assistant",
			Content: "I'll inspect the scorer.",
		},
		{
			Role: "user",
			Content: "Please fix failing backend scorer tests and " +
				"keep the changes small.",
		},
		{
			Role:    "user",
			Content: "yes",
		},
	}}

	got := AnalyzeHeuristics(in)
	assert.Equal(t, 1, got.DuplicatePromptCount)
}

func TestAnalyzeHeuristics_CodeContext(t *testing.T) {
	tests := []struct {
		name string
		in   HeuristicInput
		want int
	}{
		{
			name: "code task without prompt or tool context",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "Fix the backend test failure in the " +
						"codebase.",
				},
			}},
			want: 1,
		},
		{
			name: "file reference is context",
			in: HeuristicInput{Messages: []HeuristicMessage{
				{
					Role: "user",
					Content: "Fix the backend test failure in " +
						"internal/signals/score.go.",
				},
			}},
			want: 0,
		},
		{
			name: "grep tool activity is context",
			in: HeuristicInput{
				Messages: []HeuristicMessage{
					{
						Role: "user",
						Content: "Fix the backend test failure in " +
							"the codebase.",
					},
				},
				ToolRows: []ToolCallRow{
					{Category: "Grep", ToolName: "Grep"},
				},
			},
			want: 0,
		},
		{
			name: "glob tool activity is context",
			in: HeuristicInput{
				Messages: []HeuristicMessage{
					{
						Role: "user",
						Content: "Fix the backend test failure in " +
							"the codebase.",
					},
				},
				ToolRows: []ToolCallRow{
					{Category: "Glob", ToolName: "Glob"},
				},
			},
			want: 0,
		},
		{
			name: "test command is context",
			in: HeuristicInput{
				Messages: []HeuristicMessage{
					{
						Role: "user",
						Content: "Fix the backend test failure in " +
							"the codebase.",
					},
				},
				ToolRows: []ToolCallRow{
					{
						Category:  "Bash",
						ToolName:  "Bash",
						InputJSON: `{"command":"go test ./internal/signals"}`,
					},
				},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnalyzeHeuristics(tt.in)
			assert.Equal(t, tt.want, got.NoCodeContextCount)
		})
	}
}

func TestAnalyzeHeuristics_RunawayToolLoop(t *testing.T) {
	t.Run("repeated failing exact calls", func(t *testing.T) {
		calls := make([]ToolCallRow, 12)
		for i := range calls {
			calls[i] = ToolCallRow{
				Category:      "Bash",
				ToolName:      "Bash",
				InputJSON:     `{"command":"npm test"}`,
				EventStatus:   "errored",
				ResultContent: "exit status 1\nFAIL",
			}
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 1, got.RunawayToolLoopCount)
	})

	t.Run("repeated successful harness calls are not runaway", func(t *testing.T) {
		calls := make([]ToolCallRow, 12)
		for i := range calls {
			calls[i] = ToolCallRow{
				Category:      "Bash",
				ToolName:      "Bash",
				InputJSON:     `{"command":"npm test"}`,
				ResultContent: "PASS",
			}
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 0, got.RunawayToolLoopCount)
	})

	t.Run("ordinary varied calls", func(t *testing.T) {
		calls := []ToolCallRow{
			{Category: "Read", ToolName: "Read", InputJSON: `{"file_path":"a.go"}`},
			{Category: "Grep", ToolName: "Grep", InputJSON: `{"pattern":"x"}`},
			{Category: "Edit", ToolName: "Edit", InputJSON: `{"file_path":"a.go"}`},
			{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"go test ./..."}`},
			{Category: "Read", ToolName: "Read", InputJSON: `{"file_path":"b.go"}`},
			{Category: "Edit", ToolName: "Edit", InputJSON: `{"file_path":"b.go"}`},
			{Category: "Glob", ToolName: "Glob", InputJSON: `{"pattern":"*.go"}`},
			{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"go test ./internal/db"}`},
			{Category: "Read", ToolName: "Read", InputJSON: `{"file_path":"c.go"}`},
			{Category: "Edit", ToolName: "Edit", InputJSON: `{"file_path":"c.go"}`},
			{Category: "Grep", ToolName: "Grep", InputJSON: `{"pattern":"z"}`},
			{Category: "Bash", ToolName: "Bash", InputJSON: `{"command":"go test ./internal/signals"}`},
		}
		got := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
		assert.Equal(t, 0, got.RunawayToolLoopCount)
	})
}

func TestNormalizePromptRemovesCodeFences(t *testing.T) {
	got := normalizePrompt("Fix this:\n```go\nfunc main() {}\n```\nPlease")
	want := "fix this: please"
	assert.Equal(t, want, got)
}

func TestCountFrustrationMarkers(t *testing.T) {
	msgs := []HeuristicMessage{
		{Role: "user", Content: "WHY WONT THIS WORK???!!!"},
		{Role: "user", Content: "this is broken after the retry"},
		{Role: "assistant", Content: "I will inspect it."},
		{Role: "user", Content: "Please run the focused test again."},
		{
			Role:    "user",
			Content: "```text\nFUCK\n```\nPlease handle the log.",
		},
	}

	assert.Equal(t, 2, CountFrustrationMarkers(msgs))
}
