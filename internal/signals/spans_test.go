package signals

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bashLs(ord, idx int) ToolCallRow {
	return ToolCallRow{
		ToolName: "Bash", InputJSON: `{"cmd":"ls"}`,
		MessageOrdinal: ord, CallIndex: idx,
	}
}

func failingNpmTest(i int) ToolCallRow {
	return ToolCallRow{
		Category: "Bash", ToolName: "Bash",
		InputJSON:      `{"command":"npm test"}`,
		EventStatus:    "errored",
		ResultContent:  "exit status 1\nFAIL",
		MessageOrdinal: i * 2, CallIndex: 0,
	}
}

func TestRunawayToolLoopSpan(t *testing.T) {
	stepCalls := func(n int, failing ...int) []ToolCallRow {
		calls := make([]ToolCallRow, n)
		for i := range calls {
			calls[i] = ToolCallRow{
				Category: "Bash", ToolName: "Bash",
				InputJSON:      fmt.Sprintf(`{"command":"step-%c"}`, rune('a'+i)),
				MessageOrdinal: i + 1,
			}
		}
		for _, i := range failing {
			calls[i].EventStatus = "errored"
		}
		return calls
	}
	exact := make([]ToolCallRow, 14)
	for i := range exact {
		exact[i] = failingNpmTest(i)
	}
	exact[13] = ToolCallRow{Category: "Read", ToolName: "Read", InputJSON: `{}`, MessageOrdinal: 26}

	tests := []struct {
		name        string
		calls       []ToolCallRow
		wantOK      bool
		first, last CallPos
		n           int
	}{
		{"under twelve calls", exact[:11], false, CallPos{}, CallPos{}, 0},
		{
			"exact run spans the whole equal-signature run",
			exact, true,
			CallPos{MessageOrdinal: 0}, CallPos{MessageOrdinal: 24}, 13,
		},
		{
			"six failures in first twelve-call window",
			stepCalls(13, 1, 3, 5, 7, 9, 11), true,
			CallPos{MessageOrdinal: 1}, CallPos{MessageOrdinal: 12}, 12,
		},
		{
			"window found later in the session",
			stepCalls(20, 9, 11, 13, 15, 17, 19), true,
			CallPos{MessageOrdinal: 9}, CallPos{MessageOrdinal: 20}, 12,
		},
		{"two failures is not runaway", stepCalls(12, 2, 5), false, CallPos{}, CallPos{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, last, n, ok := RunawayToolLoopSpan(tt.calls)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.first, first)
			assert.Equal(t, tt.last, last)
			assert.Equal(t, tt.n, n)
		})
	}
}

func TestHighContextPressureMatchesScorePenalty(t *testing.T) {
	tests := []struct {
		name     string
		pressure float64
		want     bool
	}{
		{"at threshold no penalty", HighContextPressure, false},
		{"just above threshold", HighContextPressure + 0.0001, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeHealthScore(ScoreInput{
				Outcome: "completed", OutcomeConfidence: "high",
				HasContextData: true, PressureMax: new(tt.pressure),
			})
			_, penalized := got.Penalties["context_pressure_high"]
			assert.Equal(t, tt.want, penalized)
		})
	}
}

func TestRetryRuns(t *testing.T) {
	tests := []struct {
		name  string
		calls []ToolCallRow
		want  []ToolRun
	}{
		{"empty", nil, nil},
		{"two identical is not a run", []ToolCallRow{bashLs(1, 0), bashLs(3, 0)}, nil},
		{
			"three identical across messages",
			[]ToolCallRow{bashLs(1, 0), bashLs(3, 0), bashLs(5, 1)},
			[]ToolRun{{
				ToolName: "Bash", Count: 3,
				First: CallPos{MessageOrdinal: 1, CallIndex: 0},
				Last:  CallPos{MessageOrdinal: 5, CallIndex: 1},
			}},
		},
		{
			"two separate runs",
			[]ToolCallRow{
				bashLs(1, 0), bashLs(2, 0), bashLs(3, 0),
				{ToolName: "Read", InputJSON: `{"f":"a"}`, MessageOrdinal: 4},
				{ToolName: "Edit", InputJSON: `{"f":"b"}`, MessageOrdinal: 5},
				{ToolName: "Edit", InputJSON: `{"f":"b"}`, MessageOrdinal: 6},
				{ToolName: "Edit", InputJSON: `{"f":"b"}`, MessageOrdinal: 7},
				{ToolName: "Edit", InputJSON: `{"f":"b"}`, MessageOrdinal: 8},
			},
			[]ToolRun{
				{ToolName: "Bash", Count: 3, First: CallPos{MessageOrdinal: 1}, Last: CallPos{MessageOrdinal: 3}},
				{ToolName: "Edit", Count: 4, First: CallPos{MessageOrdinal: 5}, Last: CallPos{MessageOrdinal: 8}},
			},
		},
		{
			"different input breaks run",
			[]ToolCallRow{
				bashLs(1, 0), bashLs(2, 0),
				{ToolName: "Bash", InputJSON: `{"cmd":"pwd"}`, MessageOrdinal: 3},
			},
			nil,
		},
		{
			"run at end of slice",
			[]ToolCallRow{
				{ToolName: "Read", InputJSON: `{}`, MessageOrdinal: 0},
				bashLs(1, 0), bashLs(2, 0), bashLs(3, 0), bashLs(4, 0),
			},
			[]ToolRun{{ToolName: "Bash", Count: 4, First: CallPos{MessageOrdinal: 1}, Last: CallPos{MessageOrdinal: 4}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RetryRuns(tt.calls))
		})
	}
}

// TestRetryRunsSumEqualsCountRetries pins spec §26.3:
// countRetries == Σ(Count-1) over the TestRetryCount fixtures
// (toolhealth_test.go:237-310), reproduced here unchanged.
func TestRetryRunsSumEqualsCountRetries(t *testing.T) {
	ls := ToolCallRow{ToolName: "Bash", InputJSON: `{"cmd":"ls"}`}
	pwd := ToolCallRow{ToolName: "Bash", InputJSON: `{"cmd":"pwd"}`}
	readLs := ToolCallRow{ToolName: "Read", InputJSON: `{"cmd":"ls"}`}
	readA := ToolCallRow{ToolName: "Read", InputJSON: `{"f":"a"}`}
	editB := ToolCallRow{ToolName: "Edit", InputJSON: `{"f":"b"}`}
	tests := []struct {
		name  string
		calls []ToolCallRow
		want  int
	}{
		{"2 identical not retry", []ToolCallRow{ls, ls}, 0},
		{"3 identical = 2 retries", []ToolCallRow{ls, ls, ls}, 2},
		{"5 identical = 4 retries", []ToolCallRow{ls, ls, ls, ls, ls}, 4},
		{"different tool breaks streak", []ToolCallRow{ls, ls, readLs, ls}, 0},
		{"different input breaks streak", []ToolCallRow{ls, ls, pwd}, 0},
		{"two groups", []ToolCallRow{ls, ls, ls, readA, editB, editB, editB, editB}, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum := 0
			for _, r := range RetryRuns(tt.calls) {
				sum += r.Count - 1
			}
			assert.Equal(t, tt.want, sum)
		})
	}
}

func edit(path string, ord, idx int) ToolCallRow {
	return ToolCallRow{
		ToolName: "Edit", Category: "Edit",
		InputJSON:      `{"file_path":"` + path + `","old_string":"x"}`,
		MessageOrdinal: ord, CallIndex: idx,
	}
}

func TestEditChurnFiles(t *testing.T) {
	tests := []struct {
		name  string
		calls []ToolCallRow
		want  []EditChurn
	}{
		{"no edits", []ToolCallRow{bashLs(1, 0)}, nil},
		{"two edits no churn", []ToolCallRow{edit("a.go", 1, 0), edit("a.go", 5, 0)}, nil},
		{
			"three edits within span",
			[]ToolCallRow{edit("a.go", 1, 0), edit("a.go", 5, 0), edit("a.go", 9, 0)},
			[]EditChurn{{FilePath: "a.go", Count: 3, First: CallPos{MessageOrdinal: 1}, Last: CallPos{MessageOrdinal: 9}}},
		},
		{
			"span of exactly ten is not churn",
			[]ToolCallRow{edit("a.go", 1, 0), edit("a.go", 5, 0), edit("a.go", 11, 0)},
			nil,
		},
		{
			"cluster extends while span stays under ten",
			[]ToolCallRow{
				edit("a.go", 1, 0), edit("a.go", 2, 0), edit("a.go", 3, 0),
				edit("a.go", 9, 1), edit("a.go", 30, 0),
			},
			[]EditChurn{{FilePath: "a.go", Count: 4, First: CallPos{MessageOrdinal: 1}, Last: CallPos{MessageOrdinal: 9, CallIndex: 1}}},
		},
		{
			"later cluster found when first edits are spread",
			[]ToolCallRow{
				edit("a.go", 1, 0), edit("a.go", 40, 0),
				edit("a.go", 41, 0), edit("a.go", 42, 0),
			},
			[]EditChurn{{FilePath: "a.go", Count: 3, First: CallPos{MessageOrdinal: 40}, Last: CallPos{MessageOrdinal: 42}}},
		},
		{
			"files reported in first-edit order",
			[]ToolCallRow{
				edit("b.go", 1, 0), edit("a.go", 2, 0),
				edit("b.go", 3, 0), edit("a.go", 4, 0),
				edit("b.go", 5, 0), edit("a.go", 6, 0),
			},
			[]EditChurn{
				{FilePath: "b.go", Count: 3, First: CallPos{MessageOrdinal: 1}, Last: CallPos{MessageOrdinal: 5}},
				{FilePath: "a.go", Count: 3, First: CallPos{MessageOrdinal: 2}, Last: CallPos{MessageOrdinal: 6}},
			},
		},
		{
			"write category counts, read does not",
			[]ToolCallRow{
				{ToolName: "Write", Category: "Write", InputJSON: `{"file_path":"w.go"}`, MessageOrdinal: 1},
				{ToolName: "Read", Category: "Read", InputJSON: `{"file_path":"w.go"}`, MessageOrdinal: 2},
				{ToolName: "Write", Category: "Write", InputJSON: `{"file_path":"w.go"}`, MessageOrdinal: 3},
				{ToolName: "Write", Category: "Write", InputJSON: `{"file_path":"w.go"}`, MessageOrdinal: 4},
			},
			[]EditChurn{{FilePath: "w.go", Count: 3, First: CallPos{MessageOrdinal: 1}, Last: CallPos{MessageOrdinal: 4}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, EditChurnFiles(tt.calls))
		})
	}
}

// TestChurnClusterAgreesWithHasChurnWindow checks the span finder against the
// unchanged predicate over random ordinal sequences, including unsorted ones.
func TestChurnClusterAgreesWithHasChurnWindow(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 2000 {
		n := rng.IntN(8)
		ords := make([]int, n)
		for j := range ords {
			ords[j] = rng.IntN(40)
		}
		_, _, ok := churnCluster(ords, 3, 10)
		require.Equal(t, hasChurnWindow(ords, 3, 10), ok, "case %d ords=%v", i, ords)
	}
}
