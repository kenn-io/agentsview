package signals

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeHealthScore(t *testing.T) {
	tests := []struct {
		name          string
		input         ScoreInput
		wantScore     *int
		wantGrade     string
		wantBasis     []string
		wantPenalties map[string]int
	}{
		{
			name: "perfect session",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasToolCalls:      true,
			},
			wantScore:     new(100),
			wantGrade:     "A",
			wantBasis:     []string{"outcome", "tool_health"},
			wantPenalties: nil,
		},
		{
			name: "errored outcome",
			input: ScoreInput{
				Outcome:           "errored",
				OutcomeConfidence: "medium",
				HasToolCalls:      true,
			},
			wantScore: new(70),
			wantGrade: "C",
			wantBasis: []string{"outcome", "tool_health"},
			wantPenalties: map[string]int{
				"outcome_errored": 30,
			},
		},
		{
			name: "abandoned outcome",
			input: ScoreInput{
				Outcome:           "abandoned",
				OutcomeConfidence: "high",
				HasToolCalls:      true,
			},
			wantScore: new(85),
			wantGrade: "B",
			wantBasis: []string{"outcome", "tool_health"},
			wantPenalties: map[string]int{
				"outcome_abandoned": 15,
			},
		},
		{
			name: "tool failures capped at 30",
			input: ScoreInput{
				Outcome:            "completed",
				OutcomeConfidence:  "high",
				HasToolCalls:       true,
				FailureSignalCount: 20,
			},
			wantScore: new(70),
			wantGrade: "C",
			wantBasis: []string{"outcome", "tool_health"},
			wantPenalties: map[string]int{
				"tool_failure_signals": 30,
			},
		},
		{
			name: "retries 3x",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasToolCalls:      true,
				RetryCount:        3,
			},
			wantScore: new(85),
			wantGrade: "B",
			wantBasis: []string{"outcome", "tool_health"},
			wantPenalties: map[string]int{
				"tool_retries": 15,
			},
		},
		{
			name: "edit churn",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasToolCalls:      true,
				EditChurnCount:    2,
			},
			wantScore: new(92),
			wantGrade: "A",
			wantBasis: []string{"outcome", "tool_health"},
			wantPenalties: map[string]int{
				"edit_churn": 8,
			},
		},
		{
			name: "consecutive failure streak",
			input: ScoreInput{
				Outcome:            "completed",
				OutcomeConfidence:  "high",
				HasToolCalls:       true,
				ConsecutiveFailMax: 3,
			},
			wantScore: new(90),
			wantGrade: "A",
			wantBasis: []string{"outcome", "tool_health"},
			wantPenalties: map[string]int{
				"consecutive_failures": 10,
			},
		},
		{
			name: "consecutive fail streak below threshold",
			input: ScoreInput{
				Outcome:            "completed",
				OutcomeConfidence:  "high",
				HasToolCalls:       true,
				ConsecutiveFailMax: 2,
			},
			wantScore:     new(100),
			wantGrade:     "A",
			wantBasis:     []string{"outcome", "tool_health"},
			wantPenalties: nil,
		},
		{
			name: "context pressure compactions and high pressure",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasContextData:    true,
				CompactionCount:   3,
				PressureMax:       new(0.95),
			},
			wantScore: new(80),
			wantGrade: "B",
			wantBasis: []string{"outcome", "context_pressure"},
			wantPenalties: map[string]int{
				"compactions":           10,
				"context_pressure_high": 10,
			},
		},
		{
			name: "first compaction free",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasContextData:    true,
				CompactionCount:   1,
			},
			wantScore:     new(100),
			wantGrade:     "A",
			wantBasis:     []string{"outcome", "context_pressure"},
			wantPenalties: nil,
		},
		{
			name: "floor at zero",
			input: ScoreInput{
				Outcome:            "errored",
				OutcomeConfidence:  "medium",
				HasToolCalls:       true,
				HasContextData:     true,
				FailureSignalCount: 20,
				RetryCount:         10,
				EditChurnCount:     10,
				ConsecutiveFailMax: 5,
				CompactionCount:    10,
				PressureMax:        new(0.99),
			},
			wantScore: new(0),
			wantGrade: "F",
			wantBasis: []string{
				"outcome", "tool_health", "context_pressure",
			},
			wantPenalties: map[string]int{
				"outcome_errored":       30,
				"tool_failure_signals":  30,
				"tool_retries":          25,
				"edit_churn":            20,
				"consecutive_failures":  10,
				"compactions":           15,
				"context_pressure_high": 10,
			},
		},
		{
			name: "unknown low no other basis returns nil",
			input: ScoreInput{
				Outcome:           "unknown",
				OutcomeConfidence: "low",
			},
			wantScore:     nil,
			wantGrade:     "",
			wantBasis:     nil,
			wantPenalties: nil,
		},
		{
			name: "unknown low with tool data gets scored",
			input: ScoreInput{
				Outcome:           "unknown",
				OutcomeConfidence: "low",
				HasToolCalls:      true,
			},
			wantScore:     new(100),
			wantGrade:     "A",
			wantBasis:     []string{"outcome", "tool_health"},
			wantPenalties: nil,
		},
		{
			name: "unknown low with context data gets scored",
			input: ScoreInput{
				Outcome:           "unknown",
				OutcomeConfidence: "low",
				HasContextData:    true,
			},
			wantScore:     new(100),
			wantGrade:     "A",
			wantBasis:     []string{"outcome", "context_pressure"},
			wantPenalties: nil,
		},
		{
			name: "unknown high confidence still scored",
			input: ScoreInput{
				Outcome:           "unknown",
				OutcomeConfidence: "high",
			},
			wantScore:     new(100),
			wantGrade:     "A",
			wantBasis:     []string{"outcome"},
			wantPenalties: nil,
		},
		{
			name: "completed no tool calls",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
			},
			wantScore:     new(100),
			wantGrade:     "A",
			wantBasis:     []string{"outcome"},
			wantPenalties: nil,
		},
		{
			name: "pressure at exactly 0.9 no penalty",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasContextData:    true,
				PressureMax:       new(0.9),
			},
			wantScore:     new(100),
			wantGrade:     "A",
			wantBasis:     []string{"outcome", "context_pressure"},
			wantPenalties: nil,
		},
		{
			name: "grade boundary D at 40",
			input: ScoreInput{
				Outcome:            "errored",
				OutcomeConfidence:  "medium",
				HasToolCalls:       true,
				FailureSignalCount: 10,
			},
			wantScore: new(40),
			wantGrade: "D",
			wantBasis: []string{"outcome", "tool_health"},
			wantPenalties: map[string]int{
				"outcome_errored":      30,
				"tool_failure_signals": 30,
			},
		},
		{
			name: "retry cap at 25",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasToolCalls:      true,
				RetryCount:        10,
			},
			wantScore: new(75),
			wantGrade: "B",
			wantBasis: []string{"outcome", "tool_health"},
			wantPenalties: map[string]int{
				"tool_retries": 25,
			},
		},
		{
			name: "edit churn cap at 20",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasToolCalls:      true,
				EditChurnCount:    10,
			},
			wantScore: new(80),
			wantGrade: "B",
			wantBasis: []string{"outcome", "tool_health"},
			wantPenalties: map[string]int{
				"edit_churn": 20,
			},
		},
		{
			name: "compaction cap at 15",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasContextData:    true,
				CompactionCount:   10,
			},
			wantScore: new(85),
			wantGrade: "B",
			wantBasis: []string{"outcome", "context_pressure"},
			wantPenalties: map[string]int{
				"compactions": 15,
			},
		},
		{
			name: "heuristic penalties are conservative and capped",
			input: ScoreInput{
				Outcome:           "completed",
				OutcomeConfidence: "high",
				HasToolCalls:      true,
				Heuristics: HeuristicSignals{
					ShortPromptCount:            10,
					UnstructuredStart:           true,
					MissingSuccessCriteriaCount: 2,
					MissingVerificationCount:    2,
					DuplicatePromptCount:        10,
					NoCodeContextCount:          1,
					RunawayToolLoopCount:        3,
				},
			},
			wantScore: new(85),
			wantGrade: "B",
			wantBasis: []string{
				"outcome", "tool_health", "prompt_quality",
				"context_quality", "workflow_quality",
			},
			wantPenalties: map[string]int{
				"constraintless_first_prompt":  1,
				"missing_success_criteria":     1,
				"stuck_repeated_prompts":       4,
				"code_task_without_context":    4,
				"repeated_failing_tool_cycles": 5,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeHealthScore(tt.input)

			// Check score.
			if tt.wantScore == nil {
				assert.Nil(t, got.Score)
			} else {
				require.NotNil(t, got.Score)
				assert.Equal(t, *tt.wantScore, *got.Score)
			}

			// Check grade.
			assert.Equal(t, tt.wantGrade, got.Grade)

			// Check basis.
			assert.True(t, slicesEqual(got.Basis, tt.wantBasis),
				"Basis = %v, want %v", got.Basis, tt.wantBasis)

			// Check penalties.
			assert.True(t, mapsEqual(got.Penalties, tt.wantPenalties),
				"Penalties = %v, want %v", got.Penalties, tt.wantPenalties)
		})
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestComputeHealthScore_MidTaskCompactionPenalty(t *testing.T) {
	tests := []struct {
		name             string
		midTaskCount     int
		wantPenaltyKey   string
		wantPenaltyValue int
	}{
		{
			name:           "no mid-task none applied",
			midTaskCount:   0,
			wantPenaltyKey: "",
		},
		{
			name:             "single mid-task scaled",
			midTaskCount:     1,
			wantPenaltyKey:   "mid_task_compactions",
			wantPenaltyValue: 8,
		},
		{
			name:             "multiple mid-task scaled",
			midTaskCount:     2,
			wantPenaltyKey:   "mid_task_compactions",
			wantPenaltyValue: 16,
		},
		{
			name:             "mid-task capped at 18",
			midTaskCount:     5,
			wantPenaltyKey:   "mid_task_compactions",
			wantPenaltyValue: 18,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := ComputeHealthScore(ScoreInput{
				Outcome:                "completed",
				OutcomeConfidence:      "high",
				HasContextData:         true,
				MidTaskCompactionCount: tc.midTaskCount,
			})
			if tc.wantPenaltyKey == "" {
				_, ok := res.Penalties["mid_task_compactions"]
				assert.False(t, ok, "unexpected mid-task penalty: %v", res.Penalties)
				return
			}
			got, ok := res.Penalties[tc.wantPenaltyKey]
			require.True(t, ok, "missing penalty %q in %v", tc.wantPenaltyKey, res.Penalties)
			assert.Equal(t, tc.wantPenaltyValue, got)
		})
	}
}
