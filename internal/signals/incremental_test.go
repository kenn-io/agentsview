package signals

import (
	"fmt"
	"math/rand"
	"slices"
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
		calls, boundaries, "", "", nil, nil, 0, 0, 0,
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
					calls, boundaries, "", "", nil, nil, 0, 0, 0,
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
		calls, nil, "", "", nil, nil, 0, 0, 0,
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
		calls, []int{5, 20}, "assistant", "done", nil, nil, 12, 4000, 11,
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
		calls, nil, "", "", nil, nil, 0, 0, 0,
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
		calls, nil, "", "", nil, nil, 0, 0, 0,
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

// TestIncrementalFinalFailureStreakAcrossWindow pins the final-failure
// streak for a run longer than the trailing window: the fold must carry
// the pre-window run length forward instead of reporting the window size.
func TestIncrementalFinalFailureStreakAcrossWindow(t *testing.T) {
	mk := func(ordinal int, fail bool) ToolCallRow {
		status := "completed"
		if fail {
			status = "errored"
		}
		return ToolCallRow{
			ToolName:       "exec_command",
			Category:       "Bash",
			InputJSON:      `{"command":"npm test"}`,
			MessageOrdinal: ordinal,
			EventStatus:    status,
		}
	}
	calls := make([]ToolCallRow, 0, 41)
	for i := range 40 {
		calls = append(calls, mk(i, true))
	}
	state := SeedIncrementalState(
		calls, nil, "", "", nil, nil, 0, 0, 0,
	)
	row := ToolHealthRow{FailureCount: 40}

	// The 40-failure list itself: the final streak must be 40, not the
	// 35-fact trailing window size.
	var next IncrementalState
	_, got, ok := state.FoldToolHealth(nil, nil, row)
	require.True(t, ok)
	full := fullToolHealth(calls)
	assert.Equal(t, full.FinalFailureStreak, got.FinalFailureStreak,
		"final failure streak across the trailing window")
	assert.Equal(t, full.ConsecutiveFailureMax, got.ConsecutiveFailureMax)

	// Append a success and fold one call at a time: every fold must match
	// the full recompute over the grown list.
	appended := []ToolCallRow{mk(40, false), mk(41, false)}
	for i := range appended {
		full = fullToolHealth(append(slices.Clone(calls), appended[:i+1]...))
		next, got, ok = state.FoldToolHealth(appended[i:i+1], nil, row)
		require.True(t, ok, "append step %d", i)
		assert.Equal(t, full.FinalFailureStreak, got.FinalFailureStreak,
			"append step %d: final streak", i)
		assert.Equal(t, full.ConsecutiveFailureMax, got.ConsecutiveFailureMax,
			"append step %d: max streak", i)
		assert.Equal(t, full.FailureCount, got.FailureCount,
			"append step %d: failure count", i)
		row = ToolHealthRow{FailureCount: got.FailureCount}
		state = next
	}
}

// TestIncrementalEditChurnOrdinalZero pins churn detection when the first
// edit sits at ordinal 0: the sentinel must distinguish "no prior edit"
// from "prior edit at ordinal 0" so three edits at 0,1,2 count one churn.
func TestIncrementalEditChurnOrdinalZero(t *testing.T) {
	mk := func(ordinal int) ToolCallRow {
		return ToolCallRow{
			ToolName:       "edit_file",
			Category:       "Edit",
			InputJSON:      `{"file_path":"/src/a.go"}`,
			MessageOrdinal: ordinal,
			EventStatus:    "completed",
		}
	}
	full := ComputeToolHealth([]ToolCallRow{mk(0), mk(1), mk(2)})
	require.Equal(t, 1, full.EditChurnCount,
		"full compute must count one churn for edits at 0,1,2")

	state := SeedIncrementalState(
		[]ToolCallRow{mk(0), mk(1)}, nil, "", "", nil, nil, 0, 0, 0,
	)
	next, got, ok := state.FoldToolHealth(
		[]ToolCallRow{mk(2)}, nil, ToolHealthRow{},
	)
	require.True(t, ok)
	assert.Equal(t, full.EditChurnCount, got.EditChurnCount,
		"incremental fold must count one churn for edits at 0,1,2")
	_ = next
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
		calls, []int{10}, "", "", nil, nil, 0, 0, 0,
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

func TestFoldToolHealthRunawayMutableWindowHeals(t *testing.T) {
	calls := make([]ToolCallRow, 12)
	for i := range calls {
		calls[i] = ToolCallRow{
			ToolName:       "exec_command",
			Category:       "Bash",
			InputJSON:      `{"command":"run"}`,
			ResultContent:  "failed",
			EventStatus:    "errored",
			MessageOrdinal: i,
			CallIndex:      0,
		}
	}
	state := SeedIncrementalState(calls, nil, "", "", nil, nil, 0, 0, 0)

	pos := func(i int) CallPos {
		return CallPos{MessageOrdinal: i, CallIndex: 0}
	}
	healthy := func(i int) ToolFact {
		return ToolFact{
			CallPos:        pos(i),
			Failure:        false,
			ExactSignature: ExactToolSignature(calls[i]),
			CommandClass:   CommandClass(calls[i]),
		}
	}

	// Heal one failure: the trailing window still qualifies, but it is a
	// mutable window and must not latch.
	next, out, ok := state.FoldToolHealth(
		nil, map[CallPos]ToolFact{pos(0): healthy(0)}, ToolHealthRow{},
	)
	require.True(t, ok)
	assert.Equal(t, 1, out.RunawayToolLoopCount)
	assert.False(t, next.RunawayHistorical,
		"a qualifying window inside the mutable trailing region must not latch")

	// Heal all but one more failure: the window no longer qualifies, and
	// the previously reported runaway must clear.
	modified := make(map[CallPos]ToolFact)
	for i := 1; i < 11; i++ {
		modified[pos(i)] = healthy(i)
	}
	next, out, ok = next.FoldToolHealth(nil, modified, ToolHealthRow{})
	require.True(t, ok)
	assert.Equal(t, 0, out.RunawayToolLoopCount,
		"a healed trailing window must clear the runaway signal")
	assert.False(t, next.RunawayHistorical)
}

func TestFoldToolHealthRunawayHistoricalStaysLatched(t *testing.T) {
	calls := make([]ToolCallRow, 47)
	for i := range calls {
		calls[i] = ToolCallRow{
			ToolName:       "exec_command",
			Category:       "Bash",
			InputJSON:      `{"command":"run"}`,
			ResultContent:  "failed",
			EventStatus:    "errored",
			MessageOrdinal: i,
			CallIndex:      0,
		}
	}
	state := SeedIncrementalState(calls, nil, "", "", nil, nil, 0, 0, 0)
	require.True(t, state.RunawayHistorical,
		"the seeded archive already has a fully exited runaway window")

	// Heal the entire trailing window: the mutable windows clear, but the
	// window that exited the trailing region stays latched.
	modified := make(map[CallPos]ToolFact)
	for i := 35; i < 47; i++ {
		modified[CallPos{MessageOrdinal: i, CallIndex: 0}] = ToolFact{
			CallPos:        CallPos{MessageOrdinal: i, CallIndex: 0},
			Failure:        false,
			ExactSignature: ExactToolSignature(calls[i]),
			CommandClass:   CommandClass(calls[i]),
		}
	}
	next, _, ok := state.FoldToolHealth(nil, modified, ToolHealthRow{})
	require.True(t, ok)
	assert.True(t, next.RunawayHistorical,
		"windows that fully exited the trailing window stay latched")
}

func TestSeedRunawayWindowCrossingRetainedBoundaryStaysHistorical(t *testing.T) {
	calls := make([]ToolCallRow, 47)
	for i := range calls {
		calls[i] = ToolCallRow{
			ToolName:       "read_file",
			Category:       "Read",
			InputJSON:      fmt.Sprintf(`{"path":"/tmp/%d"}`, i),
			ResultContent:  "ok",
			MessageOrdinal: i,
		}
	}
	// The qualifying 12-call window starts before the retained-tail cut
	// (47-35=12) and ends after it. All six failures are already older than
	// the final 12-call mutable region, so the signal must be historical.
	for i := 6; i < 18; i++ {
		calls[i].ToolName = "exec_command"
		calls[i].Category = "Bash"
		calls[i].InputJSON = `{"command":"run"}`
		if i >= 12 {
			calls[i].ResultContent = "failed"
			calls[i].EventStatus = "errored"
		}
	}

	state := SeedIncrementalState(calls, nil, "", "", nil, nil, 0, 0, 0)
	require.True(t, state.RunawayHistorical,
		"an immutable runaway window crossing the retained boundary must latch")

	appended := make([]ToolCallRow, 36)
	for i := range appended {
		appended[i] = ToolCallRow{
			ToolName:       "read_file",
			Category:       "Read",
			InputJSON:      fmt.Sprintf(`{"path":"/healthy/%d"}`, i),
			ResultContent:  "ok",
			MessageOrdinal: len(calls) + i,
		}
	}
	next, out, ok := state.FoldToolHealth(appended, nil, ToolHealthRow{})
	require.True(t, ok)
	assert.True(t, next.RunawayHistorical)
	assert.Equal(t, 1, out.RunawayToolLoopCount)
}

func TestFoldRunawayWindowCrossingNewRetainedBoundaryLatches(t *testing.T) {
	calls := make([]ToolCallRow, 35)
	for i := range calls {
		calls[i] = ToolCallRow{
			ToolName:       "read_file",
			Category:       "Read",
			InputJSON:      fmt.Sprintf(`{"path":"/initial/%d"}`, i),
			ResultContent:  "ok",
			MessageOrdinal: i,
		}
	}
	// This qualifying window is positions [12,24). At seed time the final
	// call is still inside the 12-call mutable region, so it must remain
	// reevaluable instead of being latched prematurely.
	for i := 18; i < 24; i++ {
		calls[i].ToolName = "exec_command"
		calls[i].Category = "Bash"
		calls[i].InputJSON = `{"command":"run"}`
		calls[i].ResultContent = "failed"
		calls[i].EventStatus = "errored"
	}
	state := SeedIncrementalState(calls, nil, "", "", nil, nil, 0, 0, 0)
	require.False(t, state.RunawayHistorical)

	appendHealthy := func(start, count int) []ToolCallRow {
		rows := make([]ToolCallRow, count)
		for i := range rows {
			rows[i] = ToolCallRow{
				ToolName:       "read_file",
				Category:       "Read",
				InputJSON:      fmt.Sprintf(`{"path":"/healthy/%d"}`, start+i),
				ResultContent:  "ok",
				MessageOrdinal: start + i,
			}
		}
		return rows
	}

	// Eighteen appends move the retained-tail cut to absolute position 18,
	// through the middle of the qualifying [12,24) window. The window has
	// simultaneously become older than the mutable region and must be latched
	// before its left half is discarded.
	firstAppend := appendHealthy(len(calls), 18)
	next, out, ok := state.FoldToolHealth(firstAppend, nil, ToolHealthRow{})
	require.True(t, ok)
	assert.True(t, next.RunawayHistorical)
	assert.Equal(t, 1, out.RunawayToolLoopCount)

	// Once the original window has completely left retained facts, later
	// healthy appends must not erase the historical signal.
	secondAppend := appendHealthy(len(calls)+len(firstAppend), 35)
	final, out, ok := next.FoldToolHealth(secondAppend, nil, ToolHealthRow{})
	require.True(t, ok)
	assert.True(t, final.RunawayHistorical)
	assert.Equal(t, 1, out.RunawayToolLoopCount)
}

func TestIncrementalStateUnmarshalInitializesMutableMaps(t *testing.T) {
	var state IncrementalState
	require.NoError(t, state.UnmarshalBinary([]byte(
		`{"codec_version":3,"total_calls":0}`,
	)))
	require.NotNil(t, state.EditLast)
	require.NotNil(t, state.ModelCounts)
	require.NotNil(t, state.ModelFirstSeen)

	next, _, ok := state.FoldToolHealth([]ToolCallRow{{
		Category:       "Edit",
		InputJSON:      `{"file_path":"main.go"}`,
		MessageOrdinal: 1,
	}}, nil, ToolHealthRow{})
	require.True(t, ok)
	require.Contains(t, next.EditLast, "main.go",
		"the first edit append must not panic on a decoded empty map")
}
