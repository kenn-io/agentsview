package db

import (
	"sort"
	"time"
)

type ActivityTotals struct {
	ToolMs         int64 `json:"tool_ms"`
	UnattributedMs int64 `json:"unattributed_ms"`
}

type TurnActivity struct {
	MessageID      int64  `json:"message_id"`
	Ordinal        int    `json:"ordinal"`
	StartedAt      string `json:"started_at"`
	DurationMs     int64  `json:"duration_ms"`
	ToolMs         int64  `json:"tool_ms"`
	UnattributedMs int64  `json:"unattributed_ms"`
	Running        bool   `json:"running"`
}

type activityInterval struct {
	start int64
	end   int64
}

func timingTimestamp(value string) (int64, bool) {
	t, err := time.Parse(time.RFC3339Nano, value)
	return t.UnixMilli(), err == nil
}

func parseClosedInterval(startValue, endValue string) (activityInterval, bool) {
	start, startErr := time.Parse(time.RFC3339Nano, startValue)
	end, endErr := time.Parse(time.RFC3339Nano, endValue)
	if startErr != nil || endErr != nil || end.Before(start) {
		return activityInterval{}, false
	}
	return activityInterval{start.UnixMilli(), end.UnixMilli()}, true
}

func executionInterval(call CallRow) (activityInterval, bool) {
	return parseClosedInterval(call.ExecutionStart, call.ExecutionEnd)
}

func measuredCallInterval(call CallRow) (activityInterval, bool) {
	if call.SubagentSessionID != nil {
		if interval, ok := parseClosedInterval(call.SubagentStart, call.SubagentEnd); ok {
			return interval, true
		}
	}
	return executionInterval(call)
}

// activityPrompts returns visible user prompts in timestamp order.
func activityPrompts(turns []TurnRow) ([]TurnActivity, []int64) {
	activity := []TurnActivity{}
	var starts []int64
	for _, row := range turns {
		if row.Role != "user" || row.IsSystem || row.IsSystemPrefixed ||
			row.SourceSubtype == "tool_result" || row.ContentLength <= 0 {
			continue
		}
		start, ok := timingTimestamp(row.Timestamp)
		if !ok || (len(starts) > 0 && start < starts[len(starts)-1]) {
			continue
		}
		starts = append(starts, start)
		activity = append(activity, TurnActivity{
			MessageID: row.MessageID, Ordinal: int(row.Ordinal),
			StartedAt: row.Timestamp,
		})
	}

	return activity, starts
}

// assembleTurnActivity clips aggregates to the session, retaining raw call evidence.
func assembleTurnActivity(
	out *SessionTiming, sess *Session, turns []TurnRow, calls []CallRow, now time.Time,
) []*activityInterval {
	var starts []int64
	out.Activity, starts = activityPrompts(turns)

	var lower, upper int64
	var hasLower, hasUpper bool
	if sess.StartedAt != nil {
		lower, hasLower = timingTimestamp(*sess.StartedAt)
	}
	if !hasLower && len(starts) > 0 {
		lower, hasLower = starts[0], true
	}
	if sess.EndedAt != nil {
		upper, hasUpper = timingTimestamp(*sess.EndedAt)
	}
	if out.Running {
		upper, hasUpper = now.UnixMilli(), true
	}
	if len(starts) > 0 && !hasUpper {
		upper, hasUpper = starts[len(starts)-1], true
	}

	var measured []activityInterval
	byCategory := map[string][]activityInterval{}
	counts := map[string]int{}
	intervals := make([]*activityInterval, len(calls))
	for i, call := range calls {
		counts[call.Category]++
		interval, ok := measuredCallInterval(call)
		if !ok {
			continue
		}
		intervals[i] = &interval
		clipped := interval
		if hasLower {
			clipped.start = max(clipped.start, lower)
		}
		if hasUpper {
			clipped.end = min(clipped.end, upper)
		}
		if clipped.end < clipped.start {
			continue
		}
		measured = append(measured, clipped)
		byCategory[call.Category] = append(byCategory[call.Category], clipped)
	}
	out.ToolDurationMs = activityUnionMs(measured)
	if len(starts) == 0 {
		out.ActivityTotals.ToolMs = out.ToolDurationMs
	}
	for category, count := range counts {
		out.ByCategory = append(out.ByCategory, CategoryTotal{
			Category: category, CallCount: count,
			DurationMs: activityUnionMs(byCategory[category]),
		})
	}
	sort.Slice(out.ByCategory, func(i, j int) bool {
		if out.ByCategory[i].DurationMs == out.ByCategory[j].DurationMs {
			return out.ByCategory[i].Category < out.ByCategory[j].Category
		}
		return out.ByCategory[i].DurationMs > out.ByCategory[j].DurationMs
	})
	if len(starts) == 0 {
		return intervals
	}
	for i := range out.Activity {
		start, end := starts[i], upper
		if hasLower {
			start = max(start, lower)
		}
		if i+1 < len(starts) {
			end = min(end, starts[i+1])
		}
		row := &out.Activity[i]
		row.DurationMs = max(0, end-start)
		row.Running = out.Running && i == len(starts)-1
		var clipped []activityInterval
		for _, interval := range measured {
			lo, hi := max(interval.start, start), min(interval.end, end)
			if hi > lo {
				clipped = append(clipped, activityInterval{lo, hi})
			}
		}
		row.ToolMs = activityUnionMs(clipped)
		// Message content has no stored endpoints for thinking or generation.
		row.UnattributedMs = row.DurationMs - row.ToolMs
		out.ActivityTotals.ToolMs += row.ToolMs
		out.ActivityTotals.UnattributedMs += row.UnattributedMs
	}
	return intervals
}

func activityUnionMs(intervals []activityInterval) int64 {
	if len(intervals) == 0 {
		return 0
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start < intervals[j].start })
	merged := intervals[0]
	var total int64
	for _, interval := range intervals[1:] {
		if interval.start <= merged.end {
			merged.end = max(merged.end, interval.end)
		} else {
			total += merged.end - merged.start
			merged = interval
		}
	}
	return total + merged.end - merged.start
}
