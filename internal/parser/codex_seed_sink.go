package parser

import (
	"context"
	"slices"
)

// codexSeedSink assigns message ordinals while discarding normalized content.
// Prefix reconstruction uses it to recover occurrence-qualified pending-call
// coordinates without retaining the transcript in memory.
type codexSeedSink struct {
	nextOrdinal    int
	hadReservation bool
	// turnAssistants holds the non-system assistant messages since the last
	// user message, oldest first. It lets ApplyTokenUsageToLastAssistant
	// answer as CodexCollectingSink does, so a usage-only row takes the same
	// ordinal in the prefix scan as in a full parse.
	turnAssistants []codexSeedAssistant
}

// codexSeedAssistant is what the usage attach rule reads from a message.
type codexSeedAssistant struct {
	ordinal       int
	hasUsage      bool
	reasoningOnly bool
}

func newCodexSeedSink() *codexSeedSink {
	return &codexSeedSink{}
}

func (s *codexSeedSink) AppendMessage(m ParsedMessage) int {
	ordinal := s.nextOrdinal
	s.nextOrdinal++
	switch {
	case m.Role == RoleUser:
		s.turnAssistants = s.turnAssistants[:0]
	case m.Role == RoleAssistant && !m.IsSystem:
		s.turnAssistants = append(s.turnAssistants, codexSeedAssistant{
			ordinal:       ordinal,
			hasUsage:      m.TokenUsage != nil,
			reasoningOnly: codexReasoningOnly(&m),
		})
	}
	return ordinal
}

func (s *codexSeedSink) ReserveOrdinal() int {
	ordinal := s.nextOrdinal
	s.nextOrdinal++
	s.hadReservation = true
	return ordinal
}

// InsertMessage places m at its reserved ordinal. A user message there is a
// turn boundary for every earlier assistant message, as in
// CodexCollectingSink.
func (s *codexSeedSink) InsertMessage(m ParsedMessage) int {
	if m.Role == RoleUser {
		s.turnAssistants = slices.DeleteFunc(s.turnAssistants, func(a codexSeedAssistant) bool {
			return a.ordinal < m.Ordinal
		})
	}
	return 0
}

func (*codexSeedSink) AppendToolResultEvent(context.Context,
	string, *ParsedToolCallPosition, ParsedToolResultEvent,
) {
}

func (*codexSeedSink) SetCallSubagentSessionID(string, *ParsedToolCallPosition, string) {}

// ApplyTokenUsageToLastAssistant applies CodexCollectingSink's attach rule
// to the recorded messages: the newest message without usage takes it, but a
// reasoning row only while no newer response has passed it.
func (s *codexSeedSink) ApplyTokenUsageToLastAssistant(string) bool {
	newerResponse := false
	for i := len(s.turnAssistants) - 1; i >= 0; i-- {
		a := &s.turnAssistants[i]
		if !a.hasUsage && (!newerResponse || !a.reasoningOnly) {
			a.hasUsage = true
			return true
		}
		newerResponse = true
	}
	return false
}

func (s *codexSeedSink) InsertOrphanMessage(_ string, m ParsedMessage) bool {
	s.InsertMessage(m)
	return true
}

func (*codexSeedSink) Finalize() {}

func (*codexSeedSink) Messages() []ParsedMessage { return nil }

func (*codexSeedSink) ToolCallUpdates() []ParsedToolCallUpdate { return nil }
