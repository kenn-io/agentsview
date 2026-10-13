package parser

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// codexCloudUsageEvents turns a thread's token records, in file order,
// into usage events. Each event is the increase in the running total since
// the previous record. An increase equal to the record's last model call
// is exactly one request; any other increase spans several calls and is a
// thread-level "session" event, priced without request bands. A decrease
// in any counter starts a new baseline whose first record adds its full
// total. totalOutput sums the output increases across baselines, and
// peakContext is the largest last-call input.
func codexCloudUsageEvents(
	threadID string, recs []mirror.TokenUsageData, fallbackModel string,
) (events []ParsedUsageEvent, totalOutput int64, peakContext int64) {
	recs = codexCloudDistinctTokens(recs)
	baselines := codexCloudBaselines(recs)
	for i, rec := range recs {
		peakContext = max(peakContext, codexCloudMapTokens(rec.Last).context())
		// A new baseline is not logged: every reparse of the mirror would
		// log every past reset again.
		increase := rec.Total
		if i > 0 && baselines[i] == baselines[i-1] {
			increase = codexCloudTokenIncrease(recs[i-1].Total, rec.Total)
		}
		fields := codexCloudMapTokens(increase)
		if fields == (codexCloudTokenFields{}) {
			continue
		}
		source := "session"
		if increase == rec.Last {
			source = usagefacts.CodexCloudRequestSource
		}
		totalOutput += fields.output
		events = append(events, ParsedUsageEvent{
			SessionID:                codexCloudSessionPrefix + threadID,
			Source:                   source,
			Model:                    cmp.Or(rec.Model, fallbackModel, "unknown"),
			InputTokens:              int(fields.input),
			OutputTokens:             int(fields.output),
			CacheCreationInputTokens: int(fields.cacheWrite),
			CacheReadInputTokens:     int(fields.cacheRead),
			OccurredAt:               rec.ObservedAt.UTC().Format(time.RFC3339Nano),
			// The key holds the whole observed breakdown, not only its
			// Total: a usage event key must be unique per session and
			// source, and two increases in one baseline can share a Total
			// only if their breakdowns differ.
			DedupKey: fmt.Sprintf("%s:%d:%d/%d/%d/%d/%d/%d", threadID, baselines[i],
				rec.Total.Total, rec.Total.Input, rec.Total.CachedInput,
				rec.Total.CacheWriteInput, rec.Total.Output, rec.Total.ReasoningOutput),
		})
	}
	return events, totalOutput, peakContext
}

// codexCloudDistinctTokens drops repeats of an earlier token record. The
// monitor can append one batch twice when a save fails after its records
// were written; the repeat would read as a falling total and count the
// running total again. One joined connection observes each update once,
// so the join ID, observation time, and counts identify a record.
func codexCloudDistinctTokens(recs []mirror.TokenUsageData) []mirror.TokenUsageData {
	type identity struct {
		joinID, turnID string
		observed       int64
		total, last    mirror.TokenCounts
	}
	seen := make(map[identity]bool, len(recs))
	distinct := make([]mirror.TokenUsageData, 0, len(recs))
	for _, rec := range recs {
		id := identity{rec.JoinID, rec.TurnID, rec.ObservedAt.UnixNano(), rec.Total, rec.Last}
		if seen[id] {
			continue
		}
		seen[id] = true
		distinct = append(distinct, rec)
	}
	return distinct
}

// codexCloudBaselines numbers each record's baseline: it starts at 0 and
// grows by one whenever any counter falls below the previous record's.
func codexCloudBaselines(recs []mirror.TokenUsageData) []int {
	baselines := make([]int, len(recs))
	for i := 1; i < len(recs); i++ {
		baselines[i] = baselines[i-1]
		if !recs[i].Total.AtLeast(recs[i-1].Total) {
			baselines[i]++
		}
	}
	return baselines
}

func codexCloudTokenIncrease(prev, cur mirror.TokenCounts) mirror.TokenCounts {
	return mirror.TokenCounts{
		Input:           cur.Input - prev.Input,
		CachedInput:     cur.CachedInput - prev.CachedInput,
		CacheWriteInput: cur.CacheWriteInput - prev.CacheWriteInput,
		Output:          cur.Output - prev.Output,
		ReasoningOutput: cur.ReasoningOutput - prev.ReasoningOutput,
		Total:           cur.Total - prev.Total,
	}
}

type codexCloudTokenFields struct{ input, cacheWrite, cacheRead, output int64 }

// codexCloudMapTokens maps OpenAI counters the way applyCodexTokenUsage
// does: input includes cached reads and cache writes, so uncached input is
// what remains, floored at zero, and cache writes are capped at it.
// Reasoning stays inside output.
func codexCloudMapTokens(c mirror.TokenCounts) codexCloudTokenFields {
	uncached := max(c.Input-c.CachedInput, 0)
	cacheWrite := min(max(c.CacheWriteInput, 0), uncached)
	return codexCloudTokenFields{
		input: uncached - cacheWrite, cacheWrite: cacheWrite,
		cacheRead: c.CachedInput, output: c.Output,
	}
}

func (f codexCloudTokenFields) context() int64 {
	return f.input + f.cacheWrite + f.cacheRead
}

// codexCloudCoverage counts the finished turns that no token record
// covers. A finished turn is captured when a token record for a later turn
// is in the same baseline, because totals are cumulative, or when a token
// record for the turn and its turn-completion record share a join ID. A
// turn-completion record arrives after every token update of its join, so
// sharing the join ID is enough; no clocks are compared.
func codexCloudCoverage(
	turns []mirror.Turn, tokens []mirror.TokenUsageData, completions []mirror.TurnCompletedData,
) ParsedCaptureCoverage {
	// A repeated batch would read as a counter reset and split baselines.
	tokens = codexCloudDistinctTokens(tokens)
	var coverage ParsedCaptureCoverage
	for _, rec := range tokens {
		if coverage.LastCaptureAt == nil || rec.ObservedAt.After(*coverage.LastCaptureAt) {
			observed := rec.ObservedAt
			coverage.LastCaptureAt = &observed
		}
	}

	order := codexCloudCoverageOrder(turns, tokens, completions)
	position := make(map[string]int, len(order))
	for i, id := range order {
		position[id] = i
	}
	finished := map[string]bool{}
	for _, turn := range turns {
		switch turn.Status {
		case "completed", "interrupted", "failed":
			finished[turn.ID] = true
		}
	}
	type turnJoin struct{ turnID, joinID string }
	completedOn := map[turnJoin]bool{}
	for _, c := range completions {
		// The monitor saw the turn finish even if the listing has not
		// caught up yet.
		finished[c.TurnID] = true
		completedOn[turnJoin{c.TurnID, c.JoinID}] = true
	}

	baselines := codexCloudBaselines(tokens)
	for pos, id := range order {
		if !finished[id] {
			continue
		}
		// A turn belongs to the baseline of the last record, in file
		// order, for it or an earlier turn; turns before every record
		// belong to the first baseline. A later baseline's first total
		// starts from a fresh counter, so it cannot cover this turn.
		baseline := 0
		for i, rec := range tokens {
			if position[rec.TurnID] <= pos {
				baseline = baselines[i]
			}
		}
		captured := false
		for i, rec := range tokens {
			if (position[rec.TurnID] > pos && baselines[i] == baseline) ||
				(rec.TurnID == id && completedOn[turnJoin{id, rec.JoinID}]) {
				captured = true
				break
			}
		}
		if !captured {
			coverage.UncapturedTurns++
		}
	}
	return coverage
}

// codexCloudCoverageOrder orders turns oldest first: listed turns by start
// time, then listing order for turns without one, as the transcript does.
// Turns named only by monitor records follow, by when they were first
// observed: live capture can see a running turn before sync lists it, and
// such a turn is the newest.
func codexCloudCoverageOrder(
	turns []mirror.Turn, tokens []mirror.TokenUsageData, completions []mirror.TurnCompletedData,
) []string {
	listed := slices.Clone(turns)
	slices.SortStableFunc(listed, func(a, b mirror.Turn) int {
		ta, tb := mirror.Seconds(a.StartedAt), mirror.Seconds(b.StartedAt)
		if c := cmp.Compare(codexCloudRank(ta.IsZero()), codexCloudRank(tb.IsZero())); c != 0 {
			return c
		}
		return ta.Compare(tb)
	})
	seen := make(map[string]bool, len(listed))
	order := make([]string, 0, len(listed))
	for _, turn := range listed {
		if !seen[turn.ID] {
			seen[turn.ID] = true
			order = append(order, turn.ID)
		}
	}
	type unlisted struct {
		id       string
		observed time.Time
	}
	var extra []unlisted
	note := func(id string, observed time.Time) {
		if !seen[id] {
			seen[id] = true
			extra = append(extra, unlisted{id, observed})
		}
	}
	for _, rec := range tokens {
		note(rec.TurnID, rec.ObservedAt)
	}
	for _, c := range completions {
		note(c.TurnID, c.ObservedAt)
	}
	slices.SortStableFunc(extra, func(a, b unlisted) int { return a.observed.Compare(b.observed) })
	for _, e := range extra {
		order = append(order, e.id)
	}
	return order
}
