package friction

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// formatRange formats a tool-call span in UTC, as in jilog health.rs:154-156.
func formatRange(first, last time.Time) string {
	return first.UTC().Format("15:04") + "-" + last.UTC().Format("15:04")
}

// rangeSuffix omits the span when either endpoint time is unknown.
func rangeSuffix(first, last time.Time) string {
	if first.IsZero() || last.IsZero() {
		return ""
	}
	return " " + formatRange(first, last)
}

type iterationEvent struct {
	ordinal int
	user    bool
	call    int // index in PatternInput.Calls when !user
}

// detectIterationRunaway ports jilog health.rs:220-278. A real user turn
// resets the consecutive tool-call count; the longest qualifying stretch
// wins. Sub-agent sessions are exempt. Ordinals restore event order when
// the caller's user-turn and call slices are interleaved.
func detectIterationRunaway(subjectID string, isSubAgent bool, in PatternInput) (Signal, bool) {
	if isSubAgent {
		return Signal{}, false
	}

	events := make([]iterationEvent, 0, len(in.UserOrdinals)+len(in.Calls))
	for _, ordinal := range in.UserOrdinals {
		events = append(events, iterationEvent{ordinal: ordinal, user: true})
	}
	for i, call := range in.Calls {
		events = append(events, iterationEvent{ordinal: call.MessageOrdinal, call: i})
	}
	slices.SortStableFunc(events, func(a, b iterationEvent) int {
		if order := cmp.Compare(a.ordinal, b.ordinal); order != 0 {
			return order
		}
		switch {
		case a.user && !b.user:
			return -1
		case b.user && !a.user:
			return 1
		default:
			return 0
		}
	})

	bestCount, bestFirst, bestLast := 0, -1, -1
	count, first, last := 0, -1, -1
	flush := func() {
		if count >= IterationRunawayMinToolCalls && count > bestCount {
			bestCount, bestFirst, bestLast = count, first, last
		}
	}
	for _, event := range events {
		if event.user {
			flush()
			count, first, last = 0, -1, -1
			continue
		}
		count++
		if first < 0 {
			first = event.call
		}
		last = event.call
	}
	flush()
	if bestCount == 0 {
		return Signal{}, false
	}

	callTime := func(i int) time.Time {
		if i < len(in.CallTimes) {
			return in.CallTimes[i]
		}
		return time.Time{}
	}
	start, end := callTime(bestFirst), callTime(bestLast)
	return Signal{
		Kind:        KindPattern,
		SubjectID:   subjectID,
		SubjectKind: SubjectSession,
		Detector:    PatternDetector(PatternIterationRunaway),
		Label:       PatternIterationRunaway,
		Text:        fmt.Sprintf("iteration runaway: %d tool calls with no intervening user message", bestCount),
		Evidence:    fmt.Sprintf("%d tool calls without a user message%s", bestCount, rangeSuffix(start, end)),
		Ordinal:     new(in.Calls[bestFirst].MessageOrdinal),
		OccurredAt:  start,
	}, true
}
