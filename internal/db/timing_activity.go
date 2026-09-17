package db

import (
	"sort"
	"time"
)

type ActivityTotals struct {
	ThinkingMs     int64 `json:"thinking_ms"`
	GenerationMs   int64 `json:"generation_ms"`
	ToolMs         int64 `json:"tool_ms"`
	UnattributedMs int64 `json:"unattributed_ms"`
}

type TurnActivity struct {
	MessageID      int64  `json:"message_id"`
	Ordinal        int    `json:"ordinal"`
	StartedAt      string `json:"started_at"`
	DurationMs     int64  `json:"duration_ms"`
	ThinkingMs     int64  `json:"thinking_ms"`
	GenerationMs   int64  `json:"generation_ms"`
	ToolMs         int64  `json:"tool_ms"`
	UnattributedMs int64  `json:"unattributed_ms"`
	Precision      string `json:"precision"`
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
	if interval, ok := executionInterval(call); ok {
		return interval, true
	}
	if call.SubagentSessionID == nil {
		return activityInterval{}, false
	}
	return parseClosedInterval(call.SubagentStart, call.SubagentEnd)
}

// assembleTurnActivity shares clipped evidence with call labels and category totals.
func assembleTurnActivity(out *SessionTiming, sess *Session, turns []TurnRow, calls []CallRow, now time.Time) []*activityInterval {
	out.Activity = []TurnActivity{}
	var starts []int64
	for _, row := range turns {
		if row.Role != "user" || row.IsSystem || row.SourceSubtype == "tool_result" || row.ContentLength <= 0 {
			continue
		}
		start, ok := timingTimestamp(row.Timestamp)
		if !ok || (len(starts) > 0 && start < starts[len(starts)-1]) {
			continue
		}
		starts = append(starts, start)
		out.Activity = append(out.Activity, TurnActivity{
			MessageID: row.MessageID, Ordinal: int(row.Ordinal), StartedAt: row.Timestamp,
			Precision: "message_only",
		})
	}

	var lower, upper int64
	var hasLower, hasUpper bool
	if sess.StartedAt != nil {
		lower, hasLower = timingTimestamp(*sess.StartedAt)
	}
	if len(starts) > 0 {
		lower, hasLower = starts[0], true
	}
	if sess.EndedAt != nil {
		upper, hasUpper = timingTimestamp(*sess.EndedAt)
	}
	intervals := make([]*activityInterval, len(calls))
	for i, call := range calls {
		interval, ok := measuredCallInterval(call)
		if !ok {
			continue
		}
		intervals[i] = &interval
		if !hasUpper || interval.end > upper {
			upper, hasUpper = interval.end, true
		}
	}
	if out.Running {
		upper, hasUpper = now.UnixMilli(), true
	}
	if len(starts) > 0 && (!hasUpper || upper < starts[len(starts)-1]) {
		upper, hasUpper = starts[len(starts)-1], true
	}

	var measured []activityInterval
	var measuredTotals []activityInterval
	byCategory := map[string][]activityInterval{}
	counts := map[string]int{}
	for i, call := range calls {
		counts[call.Category]++
		interval := intervals[i]
		if interval == nil {
			continue
		}
		measuredTotals = append(measuredTotals, *interval)
		byCategory[call.Category] = append(byCategory[call.Category], *interval)
		clipped := *interval
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
	}
	out.ToolDurationMs = activityUnionMs(measuredTotals)
	out.ActivityTotals.ToolMs = activityUnionMs(measured)
	if len(starts) == 0 {
		out.ActivityTotals.ToolMs = out.ToolDurationMs
	}
	for category, count := range counts {
		out.ByCategory = append(out.ByCategory, CategoryTotal{
			Category: category, CallCount: count, DurationMs: activityUnionMs(byCategory[category]),
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
		end := upper
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		row := &out.Activity[i]
		row.DurationMs = end - starts[i]
		row.Running = out.Running && i == len(starts)-1
		var clipped []activityInterval
		for _, interval := range measured {
			start, stop := max(interval.start, starts[i]), min(interval.end, end)
			if stop > start {
				clipped = append(clipped, activityInterval{start, stop})
			}
		}
		row.ToolMs = activityUnionMs(clipped)
		// Message content has no stored endpoints for thinking or generation.
		row.UnattributedMs = row.DurationMs - row.ToolMs
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
