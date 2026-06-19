package signals

// ScoreInput holds all signals needed to compute a health score.
// Populated by the caller from outcome, tool health, and context
// pressure results.
type ScoreInput struct {
	Outcome                string
	OutcomeConfidence      string
	HasToolCalls           bool
	FailureSignalCount     int
	RetryCount             int
	EditChurnCount         int
	ConsecutiveFailMax     int
	HasContextData         bool
	CompactionCount        int
	MidTaskCompactionCount int
	PressureMax            *float64
	Heuristics             HeuristicSignals
}

// ScoreResult holds the computed health score and its breakdown.
type ScoreResult struct {
	Score     *int           // nil = not scored
	Grade     string         // "", "A", "B", "C", "D", "F"
	Basis     []string       // which categories contributed
	Penalties map[string]int // signal_name -> penalty applied
}

// ComputeHealthScore computes a penalty-based health score from
// session signals. Starts at 100, subtracts penalties, floors at 0.
// Pure computation, no DB access.
func ComputeHealthScore(in ScoreInput) ScoreResult {
	basis := buildBasis(in)

	if !canScore(in, basis) {
		return ScoreResult{}
	}

	penalties := computePenalties(in)

	score := 100
	for _, p := range penalties {
		score -= p
	}
	if score < 0 {
		score = 0
	}

	var penaltyMap map[string]int
	if len(penalties) > 0 {
		penaltyMap = penalties
	}

	return ScoreResult{
		Score:     &score,
		Grade:     gradeFromScore(score),
		Basis:     basis,
		Penalties: penaltyMap,
	}
}

func buildBasis(in ScoreInput) []string {
	basis := []string{"outcome"}
	if in.HasToolCalls {
		basis = append(basis, "tool_health")
	}
	if in.HasContextData {
		basis = append(basis, "context_pressure")
	}
	if hasPromptQualitySignals(in.Heuristics) {
		basis = append(basis, "prompt_quality")
	}
	if in.Heuristics.NoCodeContextCount > 0 {
		basis = append(basis, "context_quality")
	}
	if in.Heuristics.RunawayToolLoopCount > 0 {
		basis = append(basis, "workflow_quality")
	}
	return basis
}

// canScore returns false when there's insufficient data to produce
// a meaningful score: unknown/low outcome with no other signals.
func canScore(in ScoreInput, basis []string) bool {
	if in.Outcome != "unknown" ||
		in.OutcomeConfidence != "low" {
		return true
	}
	return len(basis) > 1
}

func computePenalties(in ScoreInput) map[string]int {
	penalties := map[string]int{}

	applyOutcomePenalty(in.Outcome, penalties)
	applyToolPenalties(in, penalties)
	applyContextPenalties(in, penalties)
	applyHeuristicPenalties(in, penalties)

	return penalties
}

func hasPromptQualitySignals(s HeuristicSignals) bool {
	return s.ShortPromptCount > 0 ||
		s.UnstructuredStart ||
		s.MissingSuccessCriteriaCount > 0 ||
		s.MissingVerificationCount > 0 ||
		s.DuplicatePromptCount > 0
}

func applyOutcomePenalty(
	outcome string,
	penalties map[string]int,
) {
	switch outcome {
	case "errored":
		penalties["outcome_errored"] = 30
	case "abandoned":
		penalties["outcome_abandoned"] = 15
	}
}

func applyToolPenalties(
	in ScoreInput,
	penalties map[string]int,
) {
	if p := capPenalty(in.FailureSignalCount*3, 30); p > 0 {
		penalties["tool_failure_signals"] = p
	}
	if p := capPenalty(in.RetryCount*5, 25); p > 0 {
		penalties["tool_retries"] = p
	}
	if p := capPenalty(in.EditChurnCount*4, 20); p > 0 {
		penalties["edit_churn"] = p
	}
	if in.ConsecutiveFailMax >= 3 {
		penalties["consecutive_failures"] = 10
	}
}

func applyContextPenalties(
	in ScoreInput,
	penalties map[string]int,
) {
	if in.CompactionCount >= 2 {
		extra := in.CompactionCount - 1
		if p := capPenalty(extra*5, 15); p > 0 {
			penalties["compactions"] = p
		}
	}
	// Mid-task compactions are weighted heavier than ordinary
	// boundaries: each one represents a strong signal that the
	// agent lost active context and is repeating earlier work.
	if in.MidTaskCompactionCount > 0 {
		if p := capPenalty(
			in.MidTaskCompactionCount*8, 18,
		); p > 0 {
			penalties["mid_task_compactions"] = p
		}
	}
	if in.PressureMax != nil && *in.PressureMax > 0.9 {
		penalties["context_pressure_high"] = 10
	}
}

func applyHeuristicPenalties(
	in ScoreInput,
	penalties map[string]int,
) {
	s := in.Heuristics
	if s.UnstructuredStart {
		penalties["constraintless_first_prompt"] = 1
	}
	if s.MissingSuccessCriteriaCount > 0 && s.UnstructuredStart {
		penalties["missing_success_criteria"] = 1
	}
	if isStuckReask(in) {
		if p := capPenalty(s.DuplicatePromptCount*2, 4); p > 0 {
			penalties["stuck_repeated_prompts"] = p
		}
	}
	if s.NoCodeContextCount > 0 {
		penalties["code_task_without_context"] = 4
	}
	if p := capPenalty(s.RunawayToolLoopCount*5, 5); p > 0 {
		penalties["repeated_failing_tool_cycles"] = p
	}
}

func isStuckReask(in ScoreInput) bool {
	if in.Heuristics.DuplicatePromptCount <= 0 {
		return false
	}
	return in.Outcome == "errored" ||
		in.Outcome == "abandoned" ||
		in.FailureSignalCount > 0 ||
		in.RetryCount > 0 ||
		in.ConsecutiveFailMax >= 3 ||
		in.Heuristics.RunawayToolLoopCount > 0
}

func capPenalty(raw, max int) int {
	if raw > max {
		return max
	}
	return raw
}

func gradeFromScore(score int) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 75:
		return "B"
	case score >= 60:
		return "C"
	case score >= 40:
		return "D"
	default:
		return "F"
	}
}
