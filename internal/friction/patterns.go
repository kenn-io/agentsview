package friction

import (
	"fmt"
	"math"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/signals"
)

// DetectPatterns turns the reused quality signals into per-occurrence
// pattern signals (spec §6.6, D14) and appends the ported
// iteration_runaway last, as jilog's detect_health_patterns does
// (health.rs:59-66).
func DetectPatterns(
	subjectID string, isSubAgent bool, in PatternInput,
) []Signal {
	times := make(map[signals.CallPos]time.Time, len(in.Calls))
	for i, c := range in.Calls {
		if i < len(in.CallTimes) {
			times[signals.CallPos{
				MessageOrdinal: c.MessageOrdinal, CallIndex: c.CallIndex,
			}] = in.CallTimes[i]
		}
	}
	span := func(first, last signals.CallPos) (string, time.Time) {
		return rangeSuffix(times[first], times[last]), times[first]
	}
	var out []Signal
	add := func(kind, description, evidence string, ordinal *int, at time.Time) {
		out = append(out, Signal{
			Kind: KindPattern, SubjectID: subjectID, SubjectKind: SubjectSession,
			Detector: PatternDetector(kind), Label: kind,
			Text: description, Evidence: evidence,
			Ordinal: ordinal, OccurredAt: at,
		})
	}

	for _, r := range signals.RetryRuns(in.Calls) {
		tool := r.ToolName
		if tool == "" {
			tool = "unknown"
		}
		rng, at := span(r.First, r.Last)
		add(PatternRetryLoop,
			fmt.Sprintf("retry loop: `%s` called %d times with identical arguments", tool, r.Count),
			fmt.Sprintf("`%s` x%d identical arguments%s", tool, r.Count, rng),
			new(r.First.MessageOrdinal), at)
	}
	if first, last, n, ok := signals.RunawayToolLoopSpan(in.Calls); ok {
		rng, at := span(first, last)
		add(PatternRunawayLoop,
			fmt.Sprintf("runaway tool loop: %d tool calls with repeated failures", n),
			fmt.Sprintf("%d tool calls%s", n, rng),
			new(first.MessageOrdinal), at)
	}
	for _, c := range signals.EditChurnFiles(in.Calls) {
		file := baseName(c.FilePath)
		rng, at := span(c.First, c.Last)
		add(PatternEditChurn,
			fmt.Sprintf("edit churn: `%s` edited %d times within 10 messages", file, c.Count),
			fmt.Sprintf("`%s` x%d edits%s", file, c.Count, rng),
			new(c.First.MessageOrdinal), at)
	}
	if n := in.MidTaskCompactions; n > 0 && len(in.CompactBoundaries) > 0 {
		var first, last time.Time
		if len(in.BoundaryTimes) > 0 {
			first = in.BoundaryTimes[0]
		}
		if lastIndex := len(in.CompactBoundaries) - 1; lastIndex < len(in.BoundaryTimes) {
			last = in.BoundaryTimes[lastIndex]
		}
		add(PatternMidTaskCompaction,
			fmt.Sprintf("mid-task compaction: %d compactions during active work", n),
			fmt.Sprintf("%d compactions%s", n, rangeSuffix(first, last)),
			new(in.CompactBoundaries[0]), first)
	}
	if p := in.PressureMax; p != nil && *p > signals.HighContextPressure {
		pct := int(math.Round(*p * 100))
		evidence := fmt.Sprintf("peak %d%%", pct)
		if !in.PressureAt.IsZero() {
			evidence += " at " + in.PressureAt.UTC().Format("15:04")
		}
		add(PatternContextPressure,
			fmt.Sprintf("context pressure: peak %d%% of the model window", pct),
			evidence, nil, in.PressureAt)
	}
	if s, ok := detectIterationRunaway(subjectID, isSubAgent, in); ok {
		s.SubjectKind = SubjectSession
		out = append(out, s)
	}
	return out
}

// baseName keeps only the last path element, for either separator, so
// no directory reaches titles or issue bodies (spec §6.6, §21).
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
