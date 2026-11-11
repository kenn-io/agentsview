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
	// usageTargets holds the ordinals of assistant messages without token
	// usage since the last user message. It lets ApplyTokenUsageToLastAssistant
	// answer as CodexCollectingSink does, so a usage-only row takes the same
	// ordinal in the prefix scan as in a full parse.
	usageTargets []int
}

func newCodexSeedSink() *codexSeedSink {
	return &codexSeedSink{}
}

func (s *codexSeedSink) AppendMessage(m ParsedMessage) int {
	ordinal := s.nextOrdinal
	s.nextOrdinal++
	switch {
	case m.Role == RoleUser:
		s.usageTargets = s.usageTargets[:0]
	case m.Role == RoleAssistant && m.TokenUsage == nil:
		s.usageTargets = append(s.usageTargets, ordinal)
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
		s.usageTargets = slices.DeleteFunc(s.usageTargets, func(ordinal int) bool {
			return ordinal < m.Ordinal
		})
	}
	return 0
}

func (*codexSeedSink) AppendToolResultEvent(context.Context,
	string, *ParsedToolCallPosition, ParsedToolResultEvent,
) {
}

func (*codexSeedSink) SetCallSubagentSessionID(string, *ParsedToolCallPosition, string) {}

func (s *codexSeedSink) ApplyTokenUsageToLastAssistant(string) bool {
	if len(s.usageTargets) == 0 {
		return false
	}
	s.usageTargets = s.usageTargets[:len(s.usageTargets)-1]
	return true
}

func (s *codexSeedSink) InsertOrphanMessage(_ string, m ParsedMessage) bool {
	s.InsertMessage(m)
	return true
}

func (*codexSeedSink) Finalize() {}

func (*codexSeedSink) Messages() []ParsedMessage { return nil }

func (*codexSeedSink) ToolCallUpdates() []ParsedToolCallUpdate { return nil }
