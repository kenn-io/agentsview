package clickhouse

import (
	"cmp"
	"slices"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/signals"
)

// dashboardFactsVersion names the definition of the dashboard facts push
// stores on sessions and tool_calls rows. Reads use the facts only on rows
// at this version and scan messages for the rest, so a row from an older
// writer stays correct. Bump it when a fact's definition changes; the next
// push from each archive then republishes every session.
const dashboardFactsVersion = 1

// chFactsKnown selects sessions rows whose dashboard facts are current.
var chFactsKnown = "dashboard_facts_version = " + strconv.Itoa(dashboardFactsVersion)

// dashboardFacts holds one session's counts that analytics panels would
// otherwise compute from its messages and tool calls on every read. Each
// count matches the predicate of the scan it replaces.
type dashboardFacts struct {
	// userMessages counts role 'user' rows that are neither system rows
	// nor tool results (activity and autonomy).
	userMessages      int
	assistantMessages int
	thinkingMessages  int
	// toolUseMessages counts assistant rows with tool use (autonomy).
	toolUseMessages int
	toolCalls       int
	// frustrationMarkers counts the user prompts signals.IsFrustrationMarker
	// flags, over the rows the signals scan reads.
	frustrationMarkers int
	// velocity is the session's velocity contribution over its messages
	// without Claude hooks, with timestamps at the stored precision.
	velocity db.SessionVelocity
}

func computeDashboardFacts(msgs []db.Message) dashboardFacts {
	var f dashboardFacts
	hooks := parser.ClaudeHookSubtypes()
	ordered := slices.SortedStableFunc(slices.Values(msgs), func(a, b db.Message) int {
		return cmp.Compare(a.Ordinal, b.Ordinal)
	})
	timings := make([]db.TimingMessage, 0, len(ordered))
	for _, m := range ordered {
		prompt := m.Role == "user" && m.SourceSubtype != string(parser.SourceSubtypeToolResult)
		if prompt && !m.IsSystem {
			f.userMessages++
			if signals.IsFrustrationMarker(m.Content) {
				f.frustrationMarkers++
			}
		}
		if m.Role == "assistant" {
			f.assistantMessages++
			if m.HasToolUse {
				f.toolUseMessages++
			}
		}
		if m.HasThinking {
			f.thinkingMessages++
		}
		f.toolCalls += len(m.ToolCalls)
		if slices.Contains(hooks, m.SourceSubtype) {
			continue
		}
		timing := db.TimingMessage{Role: m.Role, ContentLength: m.ContentLength}
		if t := timeValue(m.Timestamp); t != nil {
			timing.Time, timing.Valid = storedDateTime64(*t), true
		}
		timings = append(timings, timing)
	}
	f.velocity = db.ComputeSessionVelocity(timings)
	return f
}

// storedDateTime64 returns t as a DateTime64(6) column reads it back. The
// driver stores UnixNano divided by 1000, so velocity facts computed at
// push see the same instants a scan of the stored messages sees.
func storedDateTime64(t time.Time) time.Time {
	var nanos int64
	if !t.IsZero() {
		nanos = t.UnixNano()
	}
	return time.UnixMicro(nanos / 1000).UTC()
}

// row returns the sessions values from dashboard_facts_version through
// fact_velocity_assistant_chars, in table order.
func (f dashboardFacts) row() []any {
	turnCycles := f.velocity.TurnCycles
	if turnCycles == nil {
		turnCycles = []float64{}
	}
	return []any{
		int64(dashboardFactsVersion),
		int64(f.userMessages), int64(f.assistantMessages), int64(f.thinkingMessages),
		int64(f.toolUseMessages), int64(f.toolCalls), int64(f.frustrationMarkers),
		int64(f.velocity.Messages), turnCycles,
		f.velocity.FirstResponse, f.velocity.ActiveSeconds,
		int64(f.velocity.AssistantChars),
	}
}
