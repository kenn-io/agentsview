package signals

import "slices"

// ToolObservationChange carries a point change; an absent call preserves its outcome and repeat.
type ToolObservationChange struct {
	Position CallPos
	Call     *ToolCallOutcome
	Ending   *ToolSequenceEnding
}

// FoldToolSequences replays only the fixed tail and appended calls, including a crossing start.
func (s *IncrementalState) FoldToolSequences(appended []ToolCallRow, modified map[CallPos]ToolFact, complete bool) (ToolSequenceContinuation, []ToolObservationChange) {
	changes := map[CallPos]ToolObservationChange{}
	old := s.SequencePrefix
	if old.Active != nil {
		changes[old.Active.Start] = ToolObservationChange{Position: old.Active.Start}
	}
	for _, fact := range s.Trailing {
		_, recovered := old.Step(fact.Sequence)
		if recovered != nil {
			changes[recovered.Start] = ToolObservationChange{Position: recovered.Start}
		}
		if old.Active != nil {
			changes[old.Active.Start] = ToolObservationChange{Position: old.Active.Start}
		}
	}
	tail := mergeFacts(s.Trailing, factsFor(appended))
	for i := range tail {
		if fact, ok := modified[tail[i].CallPos]; ok {
			tail[i] = fact
		}
	}
	next := s.SequencePrefix
	cut := max(0, len(tail)-TrailingFactCount)
	prefix := next
	for i, fact := range tail {
		observed, recovered := next.Step(fact.Sequence)
		change := changes[fact.CallPos]
		change.Position = fact.CallPos
		change.Call = &observed
		changes[fact.CallPos] = change
		if recovered != nil {
			change := changes[recovered.Start]
			change.Position = recovered.Start
			change.Ending = new(ToolSequenceEndingRecovered)
			changes[recovered.Start] = change
		}
		if i+1 == cut {
			prefix = next
		}
	}
	if next.Active != nil {
		change := changes[next.Active.Start]
		change.Position = next.Active.Start
		change.Ending = new(next.Ending(complete))
		changes[next.Active.Start] = change
	}
	out := make([]ToolObservationChange, 0, len(changes))
	for _, change := range changes {
		out = append(out, change)
	}
	slices.SortFunc(out, func(a, b ToolObservationChange) int {
		if a.Position.MessageOrdinal != b.Position.MessageOrdinal {
			return a.Position.MessageOrdinal - b.Position.MessageOrdinal
		}
		return a.Position.CallIndex - b.Position.CallIndex
	})
	return prefix, out
}
