package signals

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolSequenceContinuationBoundedReplay(t *testing.T) {
	calls := make([]ToolCallRow, 0, 2000)
	for i := range 2000 {
		calls = append(calls, ToolCallRow{MessageOrdinal: i, ToolName: "Grep", Category: "Grep", InputJSON: `{"pattern":"alpha"}`, ResultContent: "No matches found"})
	}
	state := SeedIncrementalState(calls, nil, "", "", nil, nil, 0, 0, 0)
	require.Len(t, state.Trailing, TrailingFactCount)
	require.NotNil(t, state.SequencePrefix.Active)
	assert.Equal(t, CallPos{}, state.SequencePrefix.Active.Start)
	stored := sequenceObservationMap(calls, false)
	fold := func(appended []ToolCallRow, modified map[CallPos]ToolFact, complete bool) {
		t.Helper()
		health := ComputeToolHealth(calls)
		next, _, ok := state.FoldToolHealth(appended, modified, ToolHealthRow{FailureCount: health.FailureSignalCount, RetryCount: health.RetryCount, EditChurnCount: health.EditChurnCount})
		require.True(t, ok)
		prefix, changes := state.FoldToolSequences(appended, modified, complete)
		next.SequencePrefix = prefix
		for _, change := range changes {
			old := stored[change.Position]
			if change.Call != nil {
				old.call = *change.Call
			}
			old.ending = ""
			if change.Ending != nil {
				old.ending = *change.Ending
			}
			stored[change.Position] = old
		}
		for i := range calls {
			if fact, ok := modified[CallPos{calls[i].MessageOrdinal, calls[i].CallIndex}]; ok {
				calls[i].ResultContent = ""
				calls[i].ContentOutcome = fact.Sequence.Outcome
			}
		}
		calls = append(calls, appended...)
		assert.Equal(t, sequenceObservationMap(calls, complete), stored)
		require.LessOrEqual(t, len(next.Trailing), TrailingFactCount)
		state = next
	}
	fold(nil, nil, true)
	fold(nil, nil, false)
	fold([]ToolCallRow{{MessageOrdinal: 2000, ToolName: "Read", Category: "Read", ResultContent: "content"}, {MessageOrdinal: 2000, CallIndex: 1, ToolName: "Grep", Category: "Grep", ResultContent: "No matches found"}}, nil, false)
	// Replacing the recovery joins a sequence whose start has already left the window.
	changed := calls[len(calls)-2]
	changed.ResultContent = "No matches found"
	changed.ContentOutcome = ToolOutcomeEmpty
	fact := factsFor([]ToolCallRow{changed})[0]
	fold(nil, map[CallPos]ToolFact{fact.CallPos: fact}, true)
	// A new recovery separates the crossing sequence from the next same-message start.
	fold([]ToolCallRow{{MessageOrdinal: 2001, ToolName: "Read", ResultContent: "content"}, {MessageOrdinal: 2001, CallIndex: 1, ToolName: "Grep", ResultContent: "No matches found"}}, nil, false)
	changed = calls[len(calls)-1]
	changed.ContentOutcome = ToolOutcomeContent
	fact = factsFor([]ToolCallRow{changed})[0]
	fold(nil, map[CallPos]ToolFact{fact.CallPos: fact}, true)
	// The same-message content is insufficient to recover the pending start.
	assert.Equal(t, ToolSequenceEndingRecovered, stored[CallPos{}].ending)
	old := factsFor([]ToolCallRow{calls[0]})[0]
	_, _, ok := state.FoldToolHealth(nil, map[CallPos]ToolFact{old.CallPos: old}, ToolHealthRow{})
	assert.False(t, ok)
	blob, err := state.MarshalBinary()
	require.NoError(t, err)
	require.Less(t, len(blob), 30000)
}

type storedSequenceObservation struct {
	call   ToolCallOutcome
	ending ToolSequenceEnding
}

func sequenceObservationMap(calls []ToolCallRow, complete bool) map[CallPos]storedSequenceObservation {
	full := ExtractToolSequences(calls, complete)
	out := map[CallPos]storedSequenceObservation{}
	for _, call := range full.Calls {
		out[CallPos{call.MessageOrdinal, call.CallIndex}] = storedSequenceObservation{call: call}
	}
	for _, sequence := range full.Sequences {
		call := full.Calls[sequence.Start]
		pos := CallPos{call.MessageOrdinal, call.CallIndex}
		observation := out[pos]
		observation.ending = sequence.Ending
		out[pos] = observation
	}
	return out
}

func TestToolSequenceRepeatsIncludeSuccessfulCalls(t *testing.T) {
	calls := []ToolCallRow{
		{MessageOrdinal: 0, ToolName: "Grep", InputJSON: `{"q":"a"}`, ResultContent: "matches"},
		{MessageOrdinal: 1, ToolName: "Grep", InputJSON: `{"q":"a"}`, ResultContent: "matches"},
		{MessageOrdinal: 2, ToolName: "Grep", InputJSON: `{ "q": "a" }`, ResultContent: "matches"},
		{MessageOrdinal: 3, ToolName: "Grep", InputJSON: `{"q":"b"}`, ResultContent: "No matches found"},
		{MessageOrdinal: 4, ToolName: "Grep", InputJSON: `{"q":"b"}`, ResultContent: "No matches found"},
	}
	state := SeedIncrementalState(calls[:1], nil, "", "", nil, nil, 0, 0, 0)
	_, changes := state.FoldToolSequences(calls[1:], nil, true)
	count := 0
	for _, change := range changes {
		if change.Call != nil && change.Call.Repeat != ToolRepeatNone {
			count++
		}
	}
	assert.Equal(t, 3, count)
}
