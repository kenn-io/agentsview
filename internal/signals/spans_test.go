package signals

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func bashLs(ord, idx int) ToolCallRow {
	return ToolCallRow{
		ToolName: "Bash", InputJSON: `{"cmd":"ls"}`,
		MessageOrdinal: ord, CallIndex: idx,
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
