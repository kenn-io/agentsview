package ingest

import (
	"context"
	"slices"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/timeutil"
)

// ConvertSessionContext converts parser session metadata into a storage row.
func ConvertSessionContext(
	ctx context.Context, parsed parser.ParsedSession,
	messages []parser.ParsedMessage,
) (db.Session, error) {
	hasTotal, hasPeak, err := parsed.TokenCoverageContext(ctx, messages)
	if err != nil {
		return db.Session{}, err
	}
	parent := stringPtr(parsed.ParentSessionID)
	session := db.Session{
		ID: parsed.ID, Project: parsed.Project, Machine: parsed.Machine,
		MessageCount: parsed.MessageCount, UserMessageCount: parsed.UserMessageCount,
		ParentSessionID: parent, ParserParentSessionID: cloneStringPtr(parent),
		RelationshipType:     string(parsed.RelationshipType),
		TotalOutputTokens:    parsed.TotalOutputTokens,
		PeakContextTokens:    parsed.PeakContextTokens,
		HasTotalOutputTokens: hasTotal, HasPeakContextTokens: hasPeak,
		Cwd: parsed.Cwd, GitBranch: parsed.GitBranch,
		SourceSessionID:      parsed.SourceSessionID,
		SourceVersion:        parsed.SourceVersion,
		TranscriptFidelity:   parsed.TranscriptFidelity,
		ParserMalformedLines: parsed.MalformedLines,
		IsTruncated:          parsed.IsTruncated,
		TerminationStatus:    stringPtr(string(parsed.TerminationStatus)),
		FilePath:             stringPtr(parsed.File.Path), FileSize: int64Ptr(parsed.File.Size),
		FileMtime:         int64Ptr(parsed.File.Mtime),
		NextOrdinal:       NextParsedOrdinal(0, messages),
		LastEntryUUID:     stringPtr(LastParsedSourceUUID("", messages)),
		ClaudeLinearParse: parsed.ClaudeLinearParse,
		FileInode:         int64Ptr(parsed.File.Inode),
		FileDevice:        int64Ptr(parsed.File.Device),
		FileHash:          stringPtr(parsed.File.Hash),
	}
	db.ApplyParsedSessionIdentity(&session, parsed)
	if parsed.FirstMessage != "" {
		session.FirstMessage = &parsed.FirstMessage
	}
	session.SessionName = db.ParsedSessionName(parsed)
	session.PreserveSessionName = parsed.Agent == parser.AgentCodex &&
		!parsed.SessionNamePresent
	if !parsed.StartedAt.IsZero() {
		session.StartedAt = timeutil.Ptr(parsed.StartedAt)
	}
	if !parsed.EndedAt.IsZero() {
		session.EndedAt = timeutil.Ptr(parsed.EndedAt)
	}
	return session, ctx.Err()
}

// ConvertMessagesContext converts messages and applies result pairing and
// carrier filtering.
func ConvertMessagesContext(
	ctx context.Context, sessionID string, agent parser.AgentType,
	parsed []parser.ParsedMessage, blocked map[string]bool,
) ([]db.Message, error) {
	messages := make([]db.Message, len(parsed))
	for i, message := range parsed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hasContext, hasOutput := message.TokenPresence()
		toolCalls, err := ConvertToolCallsContext(
			ctx, sessionID, message.ToolCalls,
		)
		if err != nil {
			return nil, err
		}
		if IsCodexFormatAgent(agent) {
			for j := range toolCalls {
				for k := range toolCalls[j].ResultEvents {
					db.PrepareToolResultEvent(&toolCalls[j].ResultEvents[k])
				}
			}
		}
		toolResults, err := ConvertToolResultsContext(ctx, message.ToolResults)
		if err != nil {
			return nil, err
		}
		messages[i] = db.Message{
			SessionID: sessionID, Ordinal: message.Ordinal,
			Role: string(message.Role), Content: message.Content,
			ThinkingText: message.ThinkingText,
			Timestamp:    timeutil.Format(message.Timestamp),
			HasThinking:  message.HasThinking, HasToolUse: message.HasToolUse,
			ContentLength: message.ContentLength, IsSystem: message.IsSystem,
			Model: message.Model, ReasoningEffort: message.ReasoningEffort,
			ProviderID: message.ProviderID, TokenUsage: message.TokenUsage,
			ContextTokens: message.ContextTokens, OutputTokens: message.OutputTokens,
			HasContextTokens: hasContext, HasOutputTokens: hasOutput,
			ClaudeMessageID: message.ClaudeMessageID,
			ClaudeRequestID: message.ClaudeRequestID,
			SourceType:      message.SourceType, SourceSubtype: message.SourceSubtype,
			PromptSource: message.PromptSource, SourceUUID: message.SourceUUID,
			SourceParentUUID:  message.SourceParentUUID,
			IsSidechain:       message.IsSidechain,
			IsCompactBoundary: message.IsCompactBoundary,
			ToolCalls:         toolCalls, ToolResults: toolResults,
		}
	}
	return PairAndFilterContext(ctx, messages, blocked)
}

// ConvertUsageEventsContext converts, stamps the final session ID, and
// validates parser usage rows.
func ConvertUsageEventsContext(
	ctx context.Context, sessionID string, parsed []parser.ParsedUsageEvent,
) ([]db.UsageEvent, db.ValidationStats, error) {
	events := make([]db.UsageEvent, 0, len(parsed))
	for _, event := range parsed {
		if err := ctx.Err(); err != nil {
			return nil, db.ValidationStats{}, err
		}
		events = append(events, db.UsageEvent{
			SessionID: sessionID, MessageOrdinal: event.MessageOrdinal,
			Source: event.Source, Model: event.Model, ProviderID: event.ProviderID,
			InputTokens: event.InputTokens, OutputTokens: event.OutputTokens,
			CacheCreationInputTokens: event.CacheCreationInputTokens,
			CacheReadInputTokens:     event.CacheReadInputTokens,
			ReasoningTokens:          event.ReasoningTokens, Cost: event.Cost,
			CostStatus: event.CostStatus, CostSource: event.CostSource,
			OccurredAt: event.OccurredAt, DedupKey: event.DedupKey,
		})
	}
	stats, err := db.ValidateAndSanitizeContext(ctx, nil, nil, events)
	return events, stats, err
}

// ConvertToolCallsContext converts parsed tool calls into normalized rows.
func ConvertToolCallsContext(
	ctx context.Context, sessionID string, parsed []parser.ParsedToolCall,
) ([]db.ToolCall, error) {
	if len(parsed) == 0 {
		return nil, ctx.Err()
	}
	calls := make([]db.ToolCall, len(parsed))
	for i, call := range parsed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		filePath := call.FilePath
		if filePath == "" {
			filePath = parser.ResolveFilePathFromJSON(call.InputJSON)
		}
		resultEvents, err := ConvertToolResultEventsContext(ctx, call.ResultEvents)
		if err != nil {
			return nil, err
		}
		calls[i] = db.ToolCall{
			SessionID: sessionID, ToolName: call.ToolName,
			Category: call.Category, ToolUseID: call.ToolUseID,
			InputJSON: call.InputJSON, Rendering: call.Rendering,
			FilePath: filePath, CallIndex: i, SkillName: call.SkillName,
			SubagentSessionID: call.SubagentSessionID,
			ResultEvents:      resultEvents,
		}
	}
	return calls, ctx.Err()
}

// ConvertToolResultEventsContext converts structured result events.
func ConvertToolResultEventsContext(
	ctx context.Context, parsed []parser.ParsedToolResultEvent,
) ([]db.ToolResultEvent, error) {
	if len(parsed) == 0 {
		return nil, ctx.Err()
	}
	events := make([]db.ToolResultEvent, len(parsed))
	for i, event := range parsed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		events[i] = db.ToolResultEvent{
			RawContentDigest: event.RawContentDigest, ToolUseID: event.ToolUseID,
			AgentID: event.AgentID, SubagentSessionID: event.SubagentSessionID,
			Source: event.Source, Status: event.Status, Content: event.Content,
			ContentLength: len(event.Content), Timestamp: timeutil.Format(event.Timestamp),
			EventIndex: i,
		}
	}
	return events, ctx.Err()
}

// ConvertToolResultsContext converts transient result carriers for pairing.
func ConvertToolResultsContext(
	ctx context.Context, parsed []parser.ParsedToolResult,
) ([]db.ToolResult, error) {
	if len(parsed) == 0 {
		return nil, ctx.Err()
	}
	results := make([]db.ToolResult, len(parsed))
	for i, result := range parsed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results[i] = db.ToolResult{
			ToolUseID: result.ToolUseID, ContentLength: result.ContentLength,
			ContentRaw: result.ContentRaw,
		}
	}
	return results, ctx.Err()
}

// IsCodexFormatAgent reports agents using Codex rollout result identity rules.
func IsCodexFormatAgent(agent parser.AgentType) bool {
	return agent == parser.AgentCodex || agent == parser.AgentTraeX
}

// NextParsedOrdinal returns the resume ordinal after the final parsed row.
func NextParsedOrdinal(current int, messages []parser.ParsedMessage) int {
	if len(messages) == 0 {
		return current
	}
	return messages[len(messages)-1].Ordinal + 1
}

// LastParsedSourceUUID returns the last available source UUID for resumption.
func LastParsedSourceUUID(
	current string, messages []parser.ParsedMessage,
) string {
	for _, message := range slices.Backward(messages) {
		if message.SourceUUID != "" {
			return message.SourceUUID
		}
	}
	return current
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func int64Ptr(value int64) *int64 {
	if value == 0 {
		return nil
	}
	return &value
}

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
