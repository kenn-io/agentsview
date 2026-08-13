package signals

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRand returns a deterministic RNG so failures reproduce.
func newTestRand(t *testing.T, seed int64) *rand.Rand {
	t.Helper()
	return rand.New(rand.NewSource(seed))
}

var parityToolNames = []string{
	"exec_command", "edit_file", "write_file", "read_file", "grep_search",
}

var parityCommands = []string{
	"ls -la", "npm test", "go build ./...", "cat main.go", "git status",
}

var parityResultStatuses = []string{
	"", "", "", "completed", "completed", "completed", "errored", "cancelled",
}

func parityRow(rng *rand.Rand, ordinal int) ToolCallRow {
	name := parityToolNames[rng.Intn(len(parityToolNames))]
	category := "Other"
	switch name {
	case "exec_command":
		category = "Bash"
	case "edit_file":
		category = "Edit"
	case "write_file":
		category = "Write"
	case "read_file":
		category = "Read"
	case "grep_search":
		category = "Grep"
	}
	input := `{"command":"` + parityCommands[rng.Intn(len(parityCommands))] + `"}`
	if category == "Edit" || category == "Write" {
		input = fmt.Sprintf(
			`{"file_path":"/src/file_%d.go"}`, rng.Intn(4),
		)
	}
	row := ToolCallRow{
		ToolName:       name,
		Category:       category,
		InputJSON:      input,
		MessageOrdinal: ordinal,
		EventStatus:    parityResultStatuses[rng.Intn(len(parityResultStatuses))],
	}
	if row.EventStatus == "" && rng.Intn(8) == 0 {
		// Content-based failure for pending Bash calls.
		row.ResultContent = "command not found"
	}
	return row
}

// fullToolHealth computes the authoritative tool-health values the fold
// must reproduce, from the complete post-delta call list.
func fullToolHealth(calls []ToolCallRow) ToolHealthResult {
	h := ComputeToolHealth(calls)
	out := ToolHealthResult{
		FailureCount:          h.FailureSignalCount,
		ConsecutiveFailureMax: h.ConsecutiveFailureMax,
		RetryCount:            h.RetryCount,
		EditChurnCount:        h.EditChurnCount,
	}
	for i := len(calls) - 1; i >= 0; i-- {
		if IsFailure(calls[i]) {
			out.FinalFailureStreak++
		} else {
			break
		}
	}
	heuristics := AnalyzeHeuristics(HeuristicInput{ToolRows: calls})
	out.RunawayToolLoopCount = heuristics.RunawayToolLoopCount
	return out
}

func fullMidTask(calls []ToolCallRow, boundaries []int) int {
	ords := make([]ToolCallOrdinal, 0, len(calls))
	for _, c := range calls {
		ords = append(ords, ToolCallOrdinal{
			MessageOrdinal: c.MessageOrdinal,
			ToolName:       c.ToolName,
		})
	}
	return CountMidTaskCompactions(boundaries, ords)
}

// TestIncrementalFoldParityRandomized drives random append/modify deltas
// through the fold and compares every maintained value against the full
// recompute over the complete history.
func TestIncrementalFoldParityRandomized(t *testing.T) {
	rng := newTestRand(t, 20260813)
	initialCalls := 20 + rng.Intn(300)
	calls := make([]ToolCallRow, 0, initialCalls+300)
	boundaries := []int{}
	for i := range initialCalls {
		calls = append(calls, parityRow(rng, i))
		if rng.Intn(40) == 0 {
			boundaries = append(boundaries, i)
		}
	}

	state := SeedIncrementalState(
		calls, boundaries, "", "", nil, nil, 0, 0,
	)
	full := fullToolHealth(calls)
	row := ToolHealthRow{
		FailureCount:   full.FailureCount,
		RetryCount:     full.RetryCount,
		EditChurnCount: full.EditChurnCount,
	}
	midTask := fullMidTask(calls, boundaries)

	nextOrdinal := initialCalls
	callIndexByOrdinal := map[int]int{}
	for step := range 300 {
		appended := []ToolCallRow{}
		modified := map[CallPos]ToolFact{}
		if rng.Intn(3) == 0 && len(calls) >= ModifiedWindowSize {
			// Modify a trailing call: flip its result fact.
			idx := len(calls) - 1 - rng.Intn(ModifiedWindowSize)
			calls[idx].EventStatus = parityResultStatuses[rng.Intn(len(parityResultStatuses))]
			calls[idx].ResultContent = ""
			modified[CallPos{
				MessageOrdinal: calls[idx].MessageOrdinal,
				CallIndex:      calls[idx].CallIndex,
			}] = factsFor(calls[idx : idx+1])[0]
		} else {
			// Append 1-3 calls sharing one new message ordinal (the
			// realistic shape: several tool calls in one message).
			n := 1 + rng.Intn(3)
			addedBoundary := false
			ordinal := nextOrdinal
			nextOrdinal++
			for range n {
				row := parityRow(rng, ordinal)
				row.CallIndex = callIndexByOrdinal[ordinal]
				callIndexByOrdinal[ordinal]++
				if rng.Intn(30) == 0 {
					boundaries = append(boundaries, ordinal)
					addedBoundary = true
				}
				calls = append(calls, row)
				appended = append(appended, row)
			}
			if addedBoundary {
				// A delta containing a compact boundary falls back to a
				// full recompute, which reseeds the state.
				full = fullToolHealth(calls)
				state = SeedIncrementalState(
					calls, boundaries, "", "", nil, nil, 0, 0,
				)
				row = ToolHealthRow{
					FailureCount:   full.FailureCount,
					RetryCount:     full.RetryCount,
					EditChurnCount: full.EditChurnCount,
				}
				midTask = fullMidTask(calls, boundaries)
				continue
			}
		}

		nextState, got, ok := state.FoldToolHealth(appended, modified, row)
		require.True(t, ok, "step %d: fold must accept in-window delta", step)

		full = fullToolHealth(calls)
		want := ToolHealthResult{
			FailureCount:          full.FailureCount,
			ConsecutiveFailureMax: full.ConsecutiveFailureMax,
			RetryCount:            full.RetryCount,
			EditChurnCount:        full.EditChurnCount,
			RunawayToolLoopCount:  full.RunawayToolLoopCount,
			FinalFailureStreak:    full.FinalFailureStreak,
		}
		midTaskDelta := got.MidTaskCompactions
		got.MidTaskCompactions = 0
		want.MidTaskCompactions = 0
		assert.Equal(t, want, got, "step %d: tool health diverged", step)

		midTask += midTaskDelta
		wantMidTask := fullMidTask(calls, boundaries)
		assert.Equal(t, wantMidTask, midTask, "step %d: mid-task diverged", step)

		// The next round's row values are the maintained ones.
		row = ToolHealthRow{
			FailureCount:   got.FailureCount,
			RetryCount:     got.RetryCount,
			EditChurnCount: got.EditChurnCount,
		}
		state = nextState

		// State invariants.
		assert.LessOrEqual(t, len(state.Trailing), TrailingFactCount)
		assert.Equal(t, len(calls), state.TotalCalls)
		if len(state.Trailing) > 0 {
			last := state.Trailing[len(state.Trailing)-1].CallPos
			wantLast := CallPos{
				MessageOrdinal: calls[len(calls)-1].MessageOrdinal,
				CallIndex:      calls[len(calls)-1].CallIndex,
			}
			assert.Equal(t, wantLast, last, "step %d: window tail", step)
		}
	}
}

// TestIncrementalFoldRejectsOutOfWindowModification verifies the fallback
// contract: modifying a call older than the maintenance window returns
// ok=false and leaves the state untouched.
func TestIncrementalFoldRejectsOutOfWindowModification(t *testing.T) {
	rng := newTestRand(t, 7)
	calls := make([]ToolCallRow, 0, 60)
	for i := range 60 {
		calls = append(calls, parityRow(rng, i))
	}
	state := SeedIncrementalState(
		calls, nil, "", "", nil, nil, 0, 0,
	)
	full := fullToolHealth(calls)
	row := ToolHealthRow{
		FailureCount:   full.FailureCount,
		RetryCount:     full.RetryCount,
		EditChurnCount: full.EditChurnCount,
	}
	old := calls[2]
	calls[2].EventStatus = "errored"
	_, _, ok := state.FoldToolHealth(nil, map[CallPos]ToolFact{
		{MessageOrdinal: old.MessageOrdinal}: factsFor(calls[2:3])[0],
	}, row)
	assert.False(t, ok, "out-of-window modification must fall back")
}

// TestIncrementalStateRoundTrip checks JSON roundtrip and codec rejection.
func TestIncrementalStateRoundTrip(t *testing.T) {
	rng := newTestRand(t, 11)
	calls := make([]ToolCallRow, 0, 40)
	for i := range 40 {
		calls = append(calls, parityRow(rng, i))
	}
	state := SeedIncrementalState(
		calls, []int{5, 20}, "assistant", "done", nil, nil, 12, 4000,
	)
	blob, err := state.MarshalBinary()
	require.NoError(t, err)
	var restored IncrementalState
	require.NoError(t, restored.UnmarshalBinary(blob))
	assert.Equal(t, state, restored)

	bad := append([]byte(nil), blob...)
	require.Error(t, (&IncrementalState{}).UnmarshalBinary(bad[:len(bad)/2]))
	state.CodecVersion++
	blob, err = state.MarshalBinary()
	require.NoError(t, err)
	require.Error(t, (&IncrementalState{}).UnmarshalBinary(blob))
}

// TestIncrementalFoldLongCrossingRun pins the failure-run latch across a
// run far longer than the trailing window.
func TestIncrementalFoldLongCrossingRun(t *testing.T) {
	rng := newTestRand(t, 3)
	// 100 identical failing Bash calls: one 100-long failure run.
	calls := make([]ToolCallRow, 0, 100)
	for i := range 100 {
		calls = append(calls, ToolCallRow{
			ToolName:       "exec_command",
			Category:       "Bash",
			InputJSON:      `{"command":"npm test"}`,
			MessageOrdinal: i,
			EventStatus:    "errored",
		})
	}
	state := SeedIncrementalState(
		calls, nil, "", "", nil, nil, 0, 0,
	)
	row := ToolHealthRow{
		FailureCount:   100,
		RetryCount:     ComputeToolHealth(calls).RetryCount,
		EditChurnCount: 0,
	}
	// Append non-failing calls and flip nothing: the max must stay 100.
	for step := range 50 {
		appended := []ToolCallRow{{
			ToolName:       "exec_command",
			Category:       "Bash",
			InputJSON:      `{"command":"ls"}`,
			MessageOrdinal: 100 + step,
			EventStatus:    "completed",
		}}
		next, got, ok := state.FoldToolHealth(appended, nil, row)
		require.True(t, ok)
		assert.Equal(t, 100, got.ConsecutiveFailureMax, "step %d", step)
		assert.Equal(t, 100, got.FailureCount)
		assert.Equal(t, 0, got.FinalFailureStreak)
		row = ToolHealthRow{
			FailureCount:   got.FailureCount,
			RetryCount:     got.RetryCount,
			EditChurnCount: got.EditChurnCount,
		}
		state = next
	}
	// Flip the very last call to failure: final streak 1, max still 100.
	pos := CallPos{MessageOrdinal: 149}
	appended := []ToolCallRow{}
	modified := map[CallPos]ToolFact{pos: {
		CallPos: pos, Failure: true,
		ExactSignature: "exec_command\x00Bash\x00" + `{"command":"ls"}`,
		CommandClass:   "Bash:ls",
	}}
	next, got, ok := state.FoldToolHealth(appended, modified, row)
	require.True(t, ok)
	assert.Equal(t, 100, got.ConsecutiveFailureMax)
	assert.Equal(t, 1, got.FinalFailureStreak)
	_ = next
	_ = rng
}

// TestIncrementalFoldRetryAcrossSeed pins retry-run maintenance across the
// seed boundary.
func TestIncrementalFoldRetryAcrossSeed(t *testing.T) {
	mk := func(ordinal int) ToolCallRow {
		return ToolCallRow{
			ToolName:       "edit_file",
			Category:       "Edit",
			InputJSON:      `{"file_path":"/src/a.go"}`,
			MessageOrdinal: ordinal,
			EventStatus:    "completed",
		}
	}
	calls := []ToolCallRow{mk(0), mk(1)}
	state := SeedIncrementalState(
		calls, nil, "", "", nil, nil, 0, 0,
	)
	row := ToolHealthRow{}
	for step := range 6 {
		appended := []ToolCallRow{mk(2 + step)}
		next, got, ok := state.FoldToolHealth(appended, nil, row)
		require.True(t, ok)
		// Calls 0..(2+step) share name+input: a run of step+3 calls
		// yields runLen-1 = step+2 retries.
		assert.Equal(t, step+2, got.RetryCount, "step %d", step)
		row = ToolHealthRow{RetryCount: got.RetryCount}
		state = next
	}
}

// TestIncrementalFoldMidTaskAcrossSeed pins mid-task counting for a
// boundary seeded with an open after-window.
func TestIncrementalFoldMidTaskAcrossSeed(t *testing.T) {
	// Boundary at ordinal 10 with no calls after it; before-window has
	// exec_command and edit_file names.
	calls := make([]ToolCallRow, 0, 10)
	for i := range 10 {
		name := "exec_command"
		if i%2 == 1 {
			name = "edit_file"
		}
		calls = append(calls, ToolCallRow{
			ToolName:       name,
			Category:       "Bash",
			InputJSON:      `{"command":"ls"}`,
			MessageOrdinal: i,
		})
	}
	state := SeedIncrementalState(
		calls, []int{10}, "", "", nil, nil, 0, 0,
	)
	require.Len(t, state.PendingBoundaries, 1)
	row := ToolHealthRow{}
	// Appends after the boundary: exec_command then edit_file → overlap 2.
	appended := []ToolCallRow{
		{ToolName: "exec_command", Category: "Bash",
			InputJSON: `{"command":"ls"}`, MessageOrdinal: 11},
		{ToolName: "edit_file", Category: "Edit",
			InputJSON: `{"file_path":"/src/b.go"}`, MessageOrdinal: 12},
	}
	next, got, ok := state.FoldToolHealth(appended, nil, row)
	require.True(t, ok)
	assert.Equal(t, 1, got.MidTaskCompactions)
	assert.Empty(t, next.PendingBoundaries)
}
