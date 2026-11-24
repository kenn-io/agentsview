package ingest

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type visualStudioCopilotArchiveReconcile struct {
	preserve bool
	merged   []db.Message
}

// ReconcileVisualStudioCopilotArchive exposes the pure message decision for
// compatibility callers that do not need session-field reconciliation.
func ReconcileVisualStudioCopilotArchive(
	parsed, stored []db.Message,
) (merged []db.Message, preserve bool) {
	decision := visualStudioCopilotArchiveDecision(parsed, stored)
	return decision.merged, decision.preserve
}

func visualStudioCopilotArchiveDecision(
	parsed, stored []db.Message,
) visualStudioCopilotArchiveReconcile {
	if len(stored) == 0 {
		return visualStudioCopilotArchiveReconcile{}
	}
	if parsed == nil {
		return visualStudioCopilotArchiveReconcile{preserve: true}
	}
	storedByKey := make(map[string][]int, len(stored))
	for i, message := range stored {
		key := visualStudioCopilotMessagePresenceKey(message)
		storedByKey[key] = append(storedByKey[key], i)
	}
	matchedStored := make([]bool, len(stored))
	updates := make(map[int]db.Message)
	additions := make([]db.Message, 0)
	hasIncomplete := false
	for _, parsedMessage := range parsed {
		key := visualStudioCopilotMessagePresenceKey(parsedMessage)
		candidates := storedByKey[key]
		if len(candidates) == 0 {
			additions = append(additions, parsedMessage)
			continue
		}
		storedIndex := candidates[0]
		storedByKey[key] = candidates[1:]
		matchedStored[storedIndex] = true
		storedMessage := stored[storedIndex]
		incomplete := VisualStudioCopilotMessageLooksIncomplete(
			parsedMessage, storedMessage,
		)
		if incomplete {
			hasIncomplete = true
		}
		if !incomplete && VisualStudioCopilotMessageHasArchiveUpdate(
			parsedMessage, storedMessage,
		) {
			updates[storedIndex] = parsedMessage
		}
	}
	var fallbackMatched bool
	additions, fallbackMatched = visualStudioCopilotResolveArchiveAdditions(
		stored, matchedStored, updates, additions, &hasIncomplete,
	)
	hasArchiveOnly := slices.Contains(matchedStored, false)
	if hasIncomplete || hasArchiveOnly || fallbackMatched {
		if len(updates) > 0 || len(additions) > 0 ||
			(fallbackMatched && !hasIncomplete) {
			return visualStudioCopilotArchiveReconcile{merged: visualStudioCopilotMergeArchiveMessages(
				stored, updates, additions,
			)}
		}
		return visualStudioCopilotArchiveReconcile{preserve: true}
	}
	return visualStudioCopilotArchiveReconcile{}
}

func visualStudioCopilotResolveArchiveAdditions(
	stored []db.Message,
	matchedStored []bool,
	updates map[int]db.Message,
	additions []db.Message,
	hasIncomplete *bool,
) ([]db.Message, bool) {
	matched := false
	unresolved := additions[:0]
	for _, parsedMessage := range additions {
		storedIndex, ok := visualStudioCopilotArchiveFallbackMatch(
			parsedMessage, stored, matchedStored,
		)
		if !ok {
			unresolved = append(unresolved, parsedMessage)
			continue
		}
		matched = true
		matchedStored[storedIndex] = true
		storedMessage := stored[storedIndex]
		incomplete := VisualStudioCopilotMessageLooksIncomplete(
			parsedMessage, storedMessage,
		)
		if incomplete {
			*hasIncomplete = true
			continue
		}
		update := visualStudioCopilotArchiveFallbackUpdate(
			parsedMessage, storedMessage,
		)
		if VisualStudioCopilotMessageHasArchiveUpdate(update, storedMessage) {
			updates[storedIndex] = update
		}
	}
	return unresolved, matched
}

func visualStudioCopilotArchiveFallbackMatch(
	parsed db.Message, stored []db.Message, matchedStored []bool,
) (int, bool) {
	match := -1
	for i, storedMessage := range stored {
		if matchedStored[i] ||
			!visualStudioCopilotMessagesFallbackMatch(parsed, storedMessage) {
			continue
		}
		if match != -1 {
			return 0, false
		}
		match = i
	}
	if match == -1 {
		return 0, false
	}
	return match, true
}

func visualStudioCopilotMessagesFallbackMatch(parsed, stored db.Message) bool {
	if parsed.Role != stored.Role {
		return false
	}
	if visualStudioCopilotMessagesShareToolIdentity(parsed, stored) {
		return true
	}
	return visualStudioCopilotMessagesShareContentIdentity(parsed, stored)
}

func visualStudioCopilotMessagesShareToolIdentity(parsed, stored db.Message) bool {
	if len(parsed.ToolCalls) == 0 || len(stored.ToolCalls) == 0 {
		return false
	}
	parsedIDs := make(map[string]string, len(parsed.ToolCalls))
	for _, call := range parsed.ToolCalls {
		id := strings.TrimSpace(call.ToolUseID)
		if id != "" {
			parsedIDs[id] = strings.TrimSpace(call.ToolName)
		}
	}
	for _, call := range stored.ToolCalls {
		id := strings.TrimSpace(call.ToolUseID)
		if id == "" {
			continue
		}
		parsedName, ok := parsedIDs[id]
		if !ok {
			continue
		}
		storedName := strings.TrimSpace(call.ToolName)
		if parsedName != "" && storedName != "" && parsedName != storedName {
			continue
		}
		return true
	}
	return false
}

func visualStudioCopilotMessagesShareContentIdentity(parsed, stored db.Message) bool {
	if len(parsed.ToolCalls) > 0 || len(stored.ToolCalls) > 0 {
		return false
	}
	switch parsed.Role {
	case string(parser.RoleAssistant), string(parser.RoleUser):
	default:
		return false
	}
	return parsed.Content != "" && parsed.Content == stored.Content
}

func visualStudioCopilotArchiveFallbackUpdate(parsed, stored db.Message) db.Message {
	update := parsed
	update.Timestamp = stored.Timestamp
	return update
}

func visualStudioCopilotMergeArchiveMessages(
	stored []db.Message, updates map[int]db.Message, additions []db.Message,
) []db.Message {
	merged := make([]db.Message, 0, len(stored)+len(additions))
	merged = append(merged, stored...)
	for index, message := range updates {
		merged[index] = message
	}
	merged = append(merged, additions...)
	if len(additions) > 0 {
		slices.SortStableFunc(merged, compareVisualStudioCopilotMessageOrder)
	}
	for i := range merged {
		merged[i].Ordinal = i
	}
	return merged
}

func compareVisualStudioCopilotMessageOrder(a, b db.Message) int {
	aTime, aOK := visualStudioCopilotMessageTime(a)
	bTime, bOK := visualStudioCopilotMessageTime(b)
	if aOK && bOK {
		switch {
		case aTime.Before(bTime):
			return -1
		case aTime.After(bTime):
			return 1
		default:
			return 0
		}
	}
	if aOK {
		return -1
	}
	if bOK {
		return 1
	}
	return 0
}

func visualStudioCopilotMessageTime(message db.Message) (time.Time, bool) {
	if message.Timestamp == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, message.Timestamp)
	return parsed, err == nil
}

func visualStudioCopilotMessagePresenceKey(message db.Message) string {
	if message.Timestamp != "" {
		return message.Role + "\x00time\x00" + message.Timestamp
	}
	if message.SourceUUID != "" {
		return message.Role + "\x00source\x00" + message.SourceUUID
	}
	return fmt.Sprintf("%s\x00ordinal\x00%d", message.Role, message.Ordinal)
}

// VisualStudioCopilotMessageLooksIncomplete compares a parsed row with its
// stored counterpart after applying the storage sanitizer.
func VisualStudioCopilotMessageLooksIncomplete(parsed, stored db.Message) bool {
	if parsed.Role != stored.Role {
		return false
	}
	p := sanitizedForArchiveCompare(parsed)
	if p.ContentLength < stored.ContentLength ||
		(stored.HasThinking && !p.HasThinking) ||
		(stored.HasOutputTokens &&
			(!p.HasOutputTokens || p.OutputTokens < stored.OutputTokens)) ||
		(stored.HasContextTokens &&
			(!p.HasContextTokens || p.ContextTokens < stored.ContextTokens)) ||
		len(p.ToolCalls) < len(stored.ToolCalls) ||
		countToolResultEvents(p.ToolCalls) < countToolResultEvents(stored.ToolCalls) {
		return true
	}
	return countToolResultContentLength(p.ToolCalls) <
		countToolResultContentLength(stored.ToolCalls)
}

func sanitizedForArchiveCompare(message db.Message) db.Message {
	db.SanitizeMessage(&message)
	return message
}

// VisualStudioCopilotMessageHasArchiveUpdate reports whether a complete parsed
// row adds richer content than its stored counterpart.
func VisualStudioCopilotMessageHasArchiveUpdate(parsed, stored db.Message) bool {
	if parsed.Role != stored.Role {
		return false
	}
	p := sanitizedForArchiveCompare(parsed)
	if p.ContentLength > stored.ContentLength ||
		(p.ContentLength == stored.ContentLength && p.Content != stored.Content) ||
		(p.HasThinking && (!stored.HasThinking || p.ThinkingText != stored.ThinkingText)) ||
		(p.HasOutputTokens &&
			(!stored.HasOutputTokens || p.OutputTokens > stored.OutputTokens)) ||
		(p.HasContextTokens &&
			(!stored.HasContextTokens || p.ContextTokens > stored.ContextTokens)) ||
		(string(p.TokenUsage) != "" && string(p.TokenUsage) != string(stored.TokenUsage)) {
		return true
	}
	return visualStudioCopilotToolCallsHaveArchiveUpdate(p.ToolCalls, stored.ToolCalls)
}

func visualStudioCopilotToolCallsHaveArchiveUpdate(parsed, stored []db.ToolCall) bool {
	if len(parsed) > len(stored) {
		return true
	}
	for i := 0; i < len(parsed) && i < len(stored); i++ {
		if visualStudioCopilotToolCallHasArchiveUpdate(parsed[i], stored[i]) {
			return true
		}
	}
	return false
}

func visualStudioCopilotToolCallHasArchiveUpdate(parsed, stored db.ToolCall) bool {
	if parsed.ResultContentLength > stored.ResultContentLength ||
		(parsed.ResultContentLength == stored.ResultContentLength &&
			parsed.ResultContent != "" && parsed.ResultContent != stored.ResultContent) ||
		len(parsed.ResultEvents) > len(stored.ResultEvents) {
		return true
	}
	for i := 0; i < len(parsed.ResultEvents) && i < len(stored.ResultEvents); i++ {
		parsedEvent := parsed.ResultEvents[i]
		storedEvent := stored.ResultEvents[i]
		if parsedEvent.ContentLength > storedEvent.ContentLength ||
			(parsedEvent.ContentLength == storedEvent.ContentLength &&
				parsedEvent.Content != "" && parsedEvent.Content != storedEvent.Content) ||
			(parsedEvent.Status != "" && parsedEvent.Status != storedEvent.Status) {
			return true
		}
	}
	return false
}

func countToolResultContentLength(calls []db.ToolCall) int {
	total := 0
	for _, call := range calls {
		total += call.ResultContentLength
		for _, event := range call.ResultEvents {
			total += event.ContentLength
		}
	}
	return total
}

func applyVisualStudioCopilotArchiveSessionFields(
	session *db.Session,
	archived *db.Session,
	parsedMessages, mergedMessages []db.Message,
) {
	if archived == nil {
		return
	}
	archiveExtendsBounds := sessionTimeBefore(archived.StartedAt, session.StartedAt) ||
		sessionTimeAfter(archived.EndedAt, session.EndedAt)
	if !visualStudioCopilotMergedFirstMessageFromParsed(
		parsedMessages, mergedMessages,
	) {
		session.FirstMessage = cloneStringPtr(archived.FirstMessage)
	}
	if archiveExtendsBounds || stringPtrEmpty(session.SessionName) {
		session.SessionName = cloneStringPtr(archived.SessionName)
	}
	session.StartedAt = earlierSessionTime(archived.StartedAt, session.StartedAt)
	session.EndedAt = laterSessionTime(archived.EndedAt, session.EndedAt)
}

func visualStudioCopilotMergedFirstMessageFromParsed(
	parsed, merged []db.Message,
) bool {
	if len(parsed) == 0 || len(merged) == 0 {
		return false
	}
	mergedFirst := merged[0]
	for _, parsedMessage := range parsed {
		if visualStudioCopilotMessagePresenceKey(parsedMessage) !=
			visualStudioCopilotMessagePresenceKey(mergedFirst) {
			continue
		}
		return !VisualStudioCopilotMessageLooksIncomplete(parsedMessage, mergedFirst) &&
			!VisualStudioCopilotMessageHasArchiveUpdate(mergedFirst, parsedMessage)
	}
	return false
}

func stringPtrEmpty(value *string) bool {
	return value == nil || strings.TrimSpace(*value) == ""
}

func sessionTimeBefore(a, b *string) bool {
	return sessionTimeCompares(a, b, time.Time.Before)
}

func sessionTimeAfter(a, b *string) bool {
	return sessionTimeCompares(a, b, time.Time.After)
}

func sessionTimeCompares(
	a, b *string, compare func(time.Time, time.Time) bool,
) bool {
	if a == nil || b == nil {
		return a != nil && b == nil
	}
	aTime, aErr := time.Parse(time.RFC3339Nano, *a)
	bTime, bErr := time.Parse(time.RFC3339Nano, *b)
	return aErr == nil && bErr == nil && compare(aTime, bTime)
}

func earlierSessionTime(a, b *string) *string {
	return chooseSessionTime(a, b, time.Time.Before)
}

func laterSessionTime(a, b *string) *string {
	return chooseSessionTime(a, b, time.Time.After)
}

func chooseSessionTime(
	a, b *string, chooseA func(time.Time, time.Time) bool,
) *string {
	switch {
	case a == nil:
		return cloneStringPtr(b)
	case b == nil:
		return cloneStringPtr(a)
	}
	aTime, aErr := time.Parse(time.RFC3339Nano, *a)
	bTime, bErr := time.Parse(time.RFC3339Nano, *b)
	switch {
	case aErr != nil:
		return cloneStringPtr(b)
	case bErr != nil:
		return cloneStringPtr(a)
	case chooseA(aTime, bTime):
		return cloneStringPtr(a)
	default:
		return cloneStringPtr(b)
	}
}
