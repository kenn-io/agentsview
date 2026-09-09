package parser

// codexSeedSink assigns message ordinals while discarding normalized content.
// Prefix reconstruction uses it to recover occurrence-qualified pending-call
// coordinates without retaining the transcript in memory.
type codexSeedSink struct {
	nextOrdinal    int
	hadReservation bool
}

func newCodexSeedSink() *codexSeedSink {
	return &codexSeedSink{}
}

func (s *codexSeedSink) AppendMessage(ParsedMessage) int {
	ordinal := s.nextOrdinal
	s.nextOrdinal++
	return ordinal
}

func (s *codexSeedSink) ReserveOrdinal() int {
	ordinal := s.nextOrdinal
	s.nextOrdinal++
	s.hadReservation = true
	return ordinal
}

func (*codexSeedSink) InsertMessage(ParsedMessage) int { return 0 }

func (*codexSeedSink) AppendToolResultEvent(
	string, *ParsedToolCallPosition, ParsedToolResultEvent,
) {
}

func (*codexSeedSink) SetCallSubagentSessionID(string, *ParsedToolCallPosition, string) {}

func (*codexSeedSink) ApplyTokenUsageToLastAssistant(string) bool { return false }

func (*codexSeedSink) InsertOrphanMessage(string, ParsedMessage) bool { return true }

func (*codexSeedSink) Finalize() {}

func (*codexSeedSink) Messages() []ParsedMessage { return nil }

func (*codexSeedSink) ToolCallUpdates() []ParsedToolCallUpdate { return nil }
