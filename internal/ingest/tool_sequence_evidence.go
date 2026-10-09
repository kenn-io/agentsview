package ingest

import (
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/signals"
)

// ToolResultContentUnknown classifies individual retained results before agent
// labels and separators are added for display. Selection matches the summary's
// latest nonempty result per agent, including the anonymous result.
func ToolResultContentUnknown(call db.ToolCall) bool {
	events := make([]signals.ResultContentEvidence, len(call.ResultEvents))
	for i, event := range call.ResultEvents {
		events[i] = signals.ResultContentEvidence{AgentID: event.AgentID, Content: event.Content}
	}
	return signals.ToolResultContentUnknown(call.ResultContent, len(events), events)
}
