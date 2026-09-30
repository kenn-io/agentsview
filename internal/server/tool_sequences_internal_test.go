package server

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/signals"
)

func TestBuildSessionToolSequences_UTF8AndCallCap(t *testing.T) {
	input := strings.Repeat("é", 300)
	result := strings.Repeat("界", 400)
	rows := make([]signals.ToolCallRow, 12)
	for i := range rows {
		tool, content := "Grep", "No matches found"
		if i == len(rows)-1 {
			tool, content = "Read", result
		}
		rows[i] = signals.ToolCallRow{
			ToolName: tool, Category: tool, ToolUseID: string(rune('a' + i)),
			MessageOrdinal: i + 1, CallIndex: 0, InputJSON: input,
			ResultContent: content, ResultContentLength: len(content),
			EventStatus: "completed",
		}
	}
	status := string(parser.TerminationClean)
	got := buildSessionToolSequences(&db.Session{ID: "session", TerminationStatus: &status}, rows, nil)
	require.Len(t, got.Sequences, 1)
	sequence := got.Sequences[0]
	assert.Equal(t, "recovered", sequence.Ending)
	assert.Equal(t, 12, sequence.TotalCalls)
	assert.Equal(t, 2, sequence.OmittedCalls)
	assert.Equal(t, 2, got.OmittedCalls)
	require.Len(t, sequence.Calls, 10)
	assert.Equal(t, rows[8].ToolUseID, sequence.Calls[8].ToolUseID)
	assert.Equal(t, rows[11].ToolUseID, sequence.Calls[9].ToolUseID)

	inputCall := sequence.Calls[0]
	assert.Len(t, inputCall.InputPreview, 512)
	assert.True(t, utf8.ValidString(inputCall.InputPreview))
	assert.Equal(t, len(input)-512, inputCall.InputOmittedBytes)
	resultCall := sequence.Calls[9]
	assert.LessOrEqual(t, len(resultCall.ResultPreview), 1024)
	assert.True(t, utf8.ValidString(resultCall.ResultPreview))
	assert.Equal(t, len(result)-len(resultCall.ResultPreview), *resultCall.ResultOmittedBytes)
}

func TestBuildSessionToolSequences_ResultLengthSemantics(t *testing.T) {
	tests := []struct {
		name      string
		row       signals.ToolCallRow
		wantBytes *int
		wantOmit  *int
	}{
		{
			name: "unknown image marker keeps measured retained bytes",
			row: signals.ToolCallRow{
				ResultContent: "[image]", ResultContentLength: 7,
				ResultContentUnknown: true, EventStatus: "completed",
			},
			wantBytes: new(7), wantOmit: new(0),
		},
		{
			name: "withheld positive length",
			row: signals.ToolCallRow{
				ResultContentLength: 42, EventStatus: "completed",
			},
			wantBytes: new(42), wantOmit: new(42),
		},
		{
			name:      "completed known empty",
			row:       signals.ToolCallRow{EventStatus: "completed"},
			wantBytes: new(0), wantOmit: new(0),
		},
		{
			name:      "failed known empty",
			row:       signals.ToolCallRow{EventStatus: "errored"},
			wantBytes: new(0), wantOmit: new(0),
		},
		{name: "no result evidence remains null", row: signals.ToolCallRow{}},
		{
			name: "unknown result cannot become known empty",
			row:  signals.ToolCallRow{EventStatus: "completed", ResultContentUnknown: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := projectSessionToolSequenceCall(tt.row, signals.ToolCallOutcome{})
			assert.Equal(t, tt.wantBytes, got.ResultBytes)
			assert.Equal(t, tt.wantOmit, got.ResultOmittedBytes)
		})
	}
}

func TestBuildSessionToolSequences_TimingUsesOrdinalAndUniqueIDs(t *testing.T) {
	zero, first, second, recovered := int64(0), int64(17), int64(29), int64(41)
	status := string(parser.TerminationClean)
	rows := []signals.ToolCallRow{
		{ToolName: "Grep", Category: "Grep", ToolUseID: "reused", MessageOrdinal: 1, InputJSON: `{}`, ResultContent: "No matches found"},
		{ToolName: "Grep", Category: "Grep", ToolUseID: "reused", MessageOrdinal: 2, InputJSON: `{}`, ResultContent: "No matches found"},
		{ToolName: "Read", Category: "Read", ToolUseID: "read", MessageOrdinal: 3, InputJSON: `{}`, ResultContent: "text"},
	}
	timing := &db.SessionTiming{Turns: []db.TurnTiming{
		{Ordinal: 1, Calls: []db.CallTiming{{ToolUseID: "reused", DurationMs: &zero}}},
		{Ordinal: 2, Calls: []db.CallTiming{{ToolUseID: "reused", DurationMs: &first}}},
		{Ordinal: 3, Calls: []db.CallTiming{{ToolUseID: "read", DurationMs: &recovered}}},
	}}
	got := buildSessionToolSequences(&db.Session{ID: "session", TerminationStatus: &status}, rows, timing)
	require.Len(t, got.Sequences, 1)
	assert.Equal(t, &zero, got.Sequences[0].Calls[0].DurationMs)
	assert.Equal(t, &first, got.Sequences[0].Calls[1].DurationMs)
	assert.Equal(t, &recovered, got.Sequences[0].Calls[2].DurationMs)

	rows = append(rows[:1], rows[2:]...)
	rows[1].MessageOrdinal = 2
	timing = &db.SessionTiming{Turns: []db.TurnTiming{{
		Ordinal: 1,
		Calls: []db.CallTiming{
			{ToolUseID: "reused", DurationMs: &first},
			{ToolUseID: "reused", DurationMs: &second},
		},
	}, {Ordinal: 2, Calls: []db.CallTiming{{ToolUseID: "read", DurationMs: &recovered}}}}}
	got = buildSessionToolSequences(&db.Session{ID: "session", TerminationStatus: &status}, rows, timing)
	require.Len(t, got.Sequences, 1)
	assert.Nil(t, got.Sequences[0].Calls[0].DurationMs)
	assert.Equal(t, &recovered, got.Sequences[0].Calls[1].DurationMs)
}
