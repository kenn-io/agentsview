package parser

import (
	"encoding/json"
	"sort"

	"github.com/tidwall/gjson"
)

// codexCollectingSink is the in-memory codexSessionSink implementation
// that reproduces the pre-streaming decoder behavior byte for byte: all
// messages stay in a slice, call ids index into it, and deferred updates
// accumulate in memory. It exists so the slice-based parser remains
// available to tests and other providers while the streaming sink lands.
type codexCollectingSink struct {
	messages             []ParsedMessage
	callRefs             map[string]codexToolCallRef
	toolCallUpdates      []ParsedToolCallUpdate
	orphanNotificationIx map[string]int
	nextOrdinal          int
}

func newCodexCollectingSink(startOrdinal int) *codexCollectingSink {
	return &codexCollectingSink{
		callRefs:             make(map[string]codexToolCallRef),
		orphanNotificationIx: make(map[string]int),
		nextOrdinal:          startOrdinal,
	}
}

func (s *codexCollectingSink) AppendMessage(m ParsedMessage) {
	m.Ordinal = s.nextOrdinal
	s.nextOrdinal++
	s.messages = append(s.messages, m)
	for callIdx := range m.ToolCalls {
		callID := m.ToolCalls[callIdx].ToolUseID
		if callID == "" {
			continue
		}
		s.callRefs[callID] = codexToolCallRef{
			messageIndex: len(s.messages) - 1,
			callIndex:    callIdx,
		}
	}
}

func (s *codexCollectingSink) ReserveOrdinal() int {
	ord := s.nextOrdinal
	s.nextOrdinal++
	return ord
}

func (s *codexCollectingSink) InsertMessage(m ParsedMessage) int {
	idx := len(s.messages)
	for i, existing := range s.messages {
		if existing.Ordinal > m.Ordinal ||
			(existing.Ordinal == m.Ordinal &&
				!m.Timestamp.IsZero() &&
				(existing.Timestamp.IsZero() ||
					m.Timestamp.Before(existing.Timestamp))) {
			idx = i
			break
		}
	}
	s.messages = append(s.messages, ParsedMessage{})
	copy(s.messages[idx+1:], s.messages[idx:])
	s.messages[idx] = m
	for callID, ref := range s.callRefs {
		if ref.messageIndex >= idx {
			ref.messageIndex++
			s.callRefs[callID] = ref
		}
	}
	return idx
}

func (s *codexCollectingSink) AppendToolResultEvent(
	callID string, ev ParsedToolResultEvent,
) {
	if callID == "" {
		return
	}
	ref, ok := s.callRefs[callID]
	if !ok ||
		ref.messageIndex < 0 || ref.messageIndex >= len(s.messages) {
		s.appendToolCallUpdate(callID, ev)
		return
	}
	if ref.callIndex < 0 ||
		ref.callIndex >= len(s.messages[ref.messageIndex].ToolCalls) {
		return
	}
	tc := &s.messages[ref.messageIndex].ToolCalls[ref.callIndex]
	if ev.ToolUseID == "" {
		ev.ToolUseID = tc.ToolUseID
	}
	if ev.SubagentSessionID == "" && ev.AgentID != "" {
		ev.SubagentSessionID = codexSubagentSessionID(ev.AgentID)
	}
	if hasEquivalentCallResultEvent(tc.ResultEvents, ev) {
		return
	}
	tc.ResultEvents = append(tc.ResultEvents, ev)
}

func (s *codexCollectingSink) appendToolCallUpdate(
	callID string, ev ParsedToolResultEvent,
) {
	if ev.ToolUseID == "" {
		ev.ToolUseID = callID
	}
	for i := range s.toolCallUpdates {
		update := &s.toolCallUpdates[i]
		if update.ToolUseID != callID {
			continue
		}
		if hasEquivalentCallResultEvent(update.ResultEvents, ev) {
			return
		}
		update.ResultEvents = append(update.ResultEvents, ev)
		return
	}
	s.toolCallUpdates = append(s.toolCallUpdates, ParsedToolCallUpdate{
		ToolUseID:    callID,
		ResultEvents: []ParsedToolResultEvent{ev},
	})
}

func (s *codexCollectingSink) SetCallSubagentSessionID(
	callID, sessionID string,
) {
	if callID == "" || sessionID == "" {
		return
	}
	ref, ok := s.callRefs[callID]
	if !ok ||
		ref.messageIndex < 0 || ref.messageIndex >= len(s.messages) {
		return
	}
	if ref.callIndex < 0 ||
		ref.callIndex >= len(s.messages[ref.messageIndex].ToolCalls) {
		return
	}
	s.messages[ref.messageIndex].ToolCalls[ref.callIndex].
		SubagentSessionID = sessionID
}

// ApplyTokenUsageToLastAssistant applies normalized token usage to the
// last assistant message without usage, scanning back to the current
// turn's user boundary. Returns false when no target exists.
func (s *codexCollectingSink) ApplyTokenUsageToLastAssistant(
	raw string,
) bool {
	for i := len(s.messages) - 1; i >= 0; i-- {
		if s.messages[i].Role == RoleUser {
			break
		}
		if s.messages[i].Role == RoleAssistant &&
			s.messages[i].TokenUsage == nil {
			applyCodexTokenUsage(&s.messages[i], raw)
			return true
		}
	}
	return false
}

func (s *codexCollectingSink) InsertOrphanMessage(
	key string, m ParsedMessage,
) bool {
	if _, ok := s.orphanNotificationIx[key]; ok {
		return false
	}
	s.orphanNotificationIx[key] = s.InsertMessage(m)
	return true
}

func (s *codexCollectingSink) Finalize() {
	sort.SliceStable(s.messages, func(i, j int) bool {
		if s.messages[i].Ordinal == s.messages[j].Ordinal {
			return i < j
		}
		return s.messages[i].Ordinal < s.messages[j].Ordinal
	})
	for i := range s.messages {
		s.messages[i].Ordinal = i
	}
}

func (s *codexCollectingSink) Messages() []ParsedMessage {
	return s.messages
}

func (s *codexCollectingSink) ToolCallUpdates() []ParsedToolCallUpdate {
	return s.toolCallUpdates
}

// hasEquivalentCallResultEvent reports whether events already contains a
// result equivalent to candidate: same agent, status, and content.
func hasEquivalentCallResultEvent(
	events []ParsedToolResultEvent, candidate ParsedToolResultEvent,
) bool {
	for _, existing := range events {
		if existing.AgentID == candidate.AgentID &&
			existing.Status == candidate.Status &&
			existing.Content == candidate.Content {
			return true
		}
	}
	return false
}

// applyCodexTokenUsage normalizes Codex token usage fields into the
// Anthropic-style shape expected by the usage and cost queries. Codex
// reports input_tokens as the full input count (cached portion included),
// while the downstream cost formula treats input_tokens as the uncached
// remainder and bills cache_read_input_tokens separately. Subtracting
// cached here prevents double-counting the cached portion at the full
// input rate.
//
//	input_tokens - cached_input_tokens -> input_tokens  (uncached)
//	output_tokens                      -> output_tokens
//	cached_input_tokens                -> cache_read_input_tokens
func applyCodexTokenUsage(msg *ParsedMessage, raw string) {
	usage := gjson.Parse(raw)
	totalInput := int(usage.Get("input_tokens").Int())
	cached := int(usage.Get("cached_input_tokens").Int())
	output := int(usage.Get("output_tokens").Int())

	uncached := max(totalInput-cached, 0)

	normalized := map[string]int{
		"input_tokens":            uncached,
		"output_tokens":           output,
		"cache_read_input_tokens": cached,
	}
	j, err := json.Marshal(normalized)
	if err != nil {
		return
	}
	msg.TokenUsage = j
	msg.OutputTokens = output
	msg.HasOutputTokens = output > 0
	msg.ContextTokens = uncached + cached
	msg.HasContextTokens = totalInput > 0 || cached > 0
}
