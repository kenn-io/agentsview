package ingest

import (
	"context"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// PairAndFilterContext attaches result carriers to calls and removes empty
// carrier messages without renumbering the remaining transcript.
func PairAndFilterContext(
	ctx context.Context, messages []db.Message, blocked map[string]bool,
) ([]db.Message, error) {
	if err := PairToolResultsContext(ctx, messages, blocked); err != nil {
		return nil, err
	}
	if err := PairToolResultEventSummariesContext(
		ctx, messages, blocked,
	); err != nil {
		return nil, err
	}
	filtered := messages[:0]
	for _, message := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if message.Role == "user" && len(message.ToolResults) > 0 &&
			strings.TrimSpace(message.Content) == "" {
			continue
		}
		filtered = append(filtered, message)
	}
	return filtered, ctx.Err()
}

// PairToolResultsContext attaches transient result carriers by tool-use ID.
func PairToolResultsContext(
	ctx context.Context, messages []db.Message, blocked map[string]bool,
) error {
	calls := make(map[string]*db.ToolCall)
	for i := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		for j := range messages[i].ToolCalls {
			call := &messages[i].ToolCalls[j]
			if call.ToolUseID != "" {
				calls[call.ToolUseID] = call
			}
		}
	}
	for _, message := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, result := range message.ToolResults {
			call, ok := calls[result.ToolUseID]
			if !ok {
				continue
			}
			call.ResultContentLength = result.ContentLength
			if !blocked[call.Category] {
				call.ResultContent = parser.DecodeContent(result.ContentRaw)
				call.ResultContentLength = db.ResolveResultContentLength(
					call.ResultContent, result.ContentLength,
				)
			}
		}
	}
	return ctx.Err()
}

// PairToolResultEventSummariesContext derives the canonical call summary from
// structured result events.
func PairToolResultEventSummariesContext(
	ctx context.Context, messages []db.Message, blocked map[string]bool,
) error {
	for i := range messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		for j := range messages[i].ToolCalls {
			call := &messages[i].ToolCalls[j]
			if len(call.ResultEvents) == 0 {
				continue
			}
			summary, err := SummarizeToolResultEventsContext(
				ctx, call.ResultEvents,
			)
			if err != nil {
				return err
			}
			call.ResultContentLength = len(summary)
			if blocked[call.Category] {
				call.ResultContent = ""
				for k := range call.ResultEvents {
					call.ResultEvents[k].Content = ""
				}
				continue
			}
			call.ResultContent = summary
		}
	}
	return ctx.Err()
}

// SummarizeToolResultEventsContext returns the latest content per agent while
// preserving first-seen agent order and the last anonymous update.
func SummarizeToolResultEventsContext(
	ctx context.Context, events []db.ToolResultEvent,
) (string, error) {
	if len(events) == 0 {
		return "", ctx.Err()
	}
	type agentSummary struct {
		content string
	}
	latestByAgent := make(map[string]agentSummary)
	orderedAgents := make([]string, 0, len(events))
	lastAnonymous := ""
	allHaveAgentID := true
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if strings.TrimSpace(event.Content) == "" {
			continue
		}
		agentID := strings.TrimSpace(event.AgentID)
		if agentID == "" {
			allHaveAgentID = false
			lastAnonymous = event.Content
			continue
		}
		if _, exists := latestByAgent[agentID]; !exists {
			orderedAgents = append(orderedAgents, agentID)
		}
		latestByAgent[agentID] = agentSummary{content: event.Content}
	}
	if len(latestByAgent) <= 1 {
		if len(latestByAgent) == 1 {
			summary := latestByAgent[orderedAgents[0]].content
			if lastAnonymous != "" {
				return summary + "\n\n" + lastAnonymous, ctx.Err()
			}
			return summary, ctx.Err()
		}
		return lastAnonymous, ctx.Err()
	}
	parts := make([]string, 0, len(orderedAgents)+1)
	for _, agentID := range orderedAgents {
		parts = append(parts, agentID+":\n"+latestByAgent[agentID].content)
	}
	if !allHaveAgentID && lastAnonymous != "" {
		parts = append(parts, lastAnonymous)
	}
	return strings.Join(parts, "\n\n"), ctx.Err()
}
