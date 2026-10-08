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
	got := buildSessionToolSequences(&db.Session{ID: "session", TerminationStatus: &status}, rows)
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
			name:      "success status known empty",
			row:       signals.ToolCallRow{EventStatus: "success"},
			wantBytes: new(0), wantOmit: new(0),
		},
		{
			name: "running status stays unknown",
			row:  signals.ToolCallRow{EventStatus: "running"},
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

func TestBuildSessionToolSequences_BoundedRetainedOutput(t *testing.T) {
	for _, count := range []int{25, 250} {
		var rows []signals.ToolCallRow
		ordinal := 0
		for range count {
			for i := range 12 {
				ordinal++
				tool, content := "Grep", "No matches found"
				if i == 10 {
					tool = "Glob"
					content = "No files found"
				}
				if i == 11 {
					tool, content = "Read", "recovered"
				}
				rows = append(rows, signals.ToolCallRow{ToolName: tool, Category: tool, ToolUseID: "call", InputJSON: strings.Repeat("x", 2048), MessageOrdinal: ordinal, ResultContent: content})
			}
		}
		response := buildSessionToolSequences(&db.Session{}, rows)
		assert.Equal(t, count, response.TotalSequences)
		assert.Equal(t, count*12, response.TotalSequenceCalls)
		assert.Equal(t, count-20, response.OmittedSequences)
		assert.Equal(t, count*12-200, response.OmittedCalls)
		require.Len(t, response.Sequences, 20)
		assert.True(t, response.Sequences[0].ToolChanged)
		assert.Equal(t, 12, response.Sequences[0].Calls[9].Ordinal)
		assert.Equal(t, 240, response.Sequences[19].Calls[9].Ordinal)
		for _, sequence := range response.Sequences {
			require.Len(t, sequence.Calls, 10)
			for _, call := range sequence.Calls {
				assert.Len(t, call.InputPreview, 512)
			}
		}
	}
}
