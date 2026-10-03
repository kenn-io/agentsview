package signals

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyToolOutcome(t *testing.T) {
	tests := []struct {
		name string
		call ToolCallRow
		want ToolOutcome
	}{
		{
			name: "success supported empty",
			call: ToolCallRow{ToolName: "read", Category: "Read", EventStatus: "success"},
			want: ToolOutcomeEmpty,
		},
		{
			name: "success omitted retained content",
			call: ToolCallRow{ToolName: "read", Category: "Read", EventStatus: "success", ResultContentLength: 17},
			want: ToolOutcomeUnknown,
		},
		{
			name: "status error",
			call: ToolCallRow{EventStatus: "errored", ResultContent: "ok"},
			want: ToolOutcomeErrored,
		},
		{
			name: "provider error status",
			call: ToolCallRow{EventStatus: "error", ResultContent: "ok"},
			want: ToolOutcomeErrored,
		},
		{
			name: "provider denied status",
			call: ToolCallRow{EventStatus: "denied", ResultContent: "ok"},
			want: ToolOutcomeErrored,
		},
		{
			name: "cancelled status",
			call: ToolCallRow{EventStatus: "cancelled"},
			want: ToolOutcomeErrored,
		},
		{
			name: "content",
			call: ToolCallRow{
				ToolName: "Bash", EventStatus: "completed",
				ResultContent: "ok", ResultContentLength: 2,
			},
			want: ToolOutcomeContent,
		},
		{
			name: "completed supported empty",
			call: ToolCallRow{
				ToolName: "Grep", Category: "Grep", EventStatus: "completed",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "completed aliased read empty",
			call: ToolCallRow{
				ToolName: "read", Category: "Read", EventStatus: "completed",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "completed aliased read file empty",
			call: ToolCallRow{
				ToolName: "read_file", Category: "Read", EventStatus: "completed",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "measured grep no matches",
			call: ToolCallRow{
				ToolName: "Grep", Category: "Grep",
				ResultContent: "No matches found",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "measured grep no files",
			call: ToolCallRow{
				ToolName: "Grep", Category: "Grep",
				ResultContent: "No files found",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "measured glob no files",
			call: ToolCallRow{
				ToolName: "Glob", Category: "Glob",
				ResultContent: "No files found",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "search style empty",
			call: ToolCallRow{
				ToolName: "search_web", Category: "Tool", EventStatus: "completed",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "missing completion evidence",
			call: ToolCallRow{
				ToolName: "Grep", Category: "Grep", ResultContentLength: 17,
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "empty content with retained length 17",
			call: ToolCallRow{
				ToolName: "Grep", Category: "Grep", EventStatus: "completed",
				ResultContentLength: 17,
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "unsupported completed empty",
			call: ToolCallRow{
				ToolName: "Bash", Category: "Tool", EventStatus: "completed",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "running status",
			call: ToolCallRow{
				ToolName: "Read", Category: "Read", EventStatus: "running",
				ResultContent: "text",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "future status",
			call: ToolCallRow{
				ToolName: "Read", Category: "Read", EventStatus: "future",
				ResultContent: "text",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "bash no matches is content",
			call: ToolCallRow{ToolName: "Bash", ResultContent: "No matches found"},
			want: ToolOutcomeContent,
		},
		{
			name: "embedded no matches is content",
			call: ToolCallRow{ToolName: "Grep", ResultContent: "No matches found\n\nFound 3 total occurrences across 2 files."},
			want: ToolOutcomeContent,
		},
		{
			name: "read no files is content",
			call: ToolCallRow{ToolName: "Read", ResultContent: "No files found"},
			want: ToolOutcomeContent,
		},
		{
			name: "same category different raw tool is content",
			call: ToolCallRow{
				ToolName: "ripgrep", Category: "Grep",
				ResultContent: "No matches found",
			},
			want: ToolOutcomeContent,
		},
		{
			name: "read exact wording is content",
			call: ToolCallRow{ToolName: "Read", ResultContent: "No matches found"},
			want: ToolOutcomeContent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyToolOutcome(tt.call))
		})
	}
}

func TestExtractToolSequences_Example(t *testing.T) {
	calls := []ToolCallRow{
		{
			ToolUseID: "empty-1", MessageOrdinal: 4, CallIndex: 0,
			ToolName: "Grep", Category: "Grep",
			InputJSON:   `{"path":"/tmp","query":"needle"}`,
			EventStatus: "completed",
		},
		{
			ToolUseID: "empty-2", MessageOrdinal: 5, CallIndex: 0,
			ToolName: "Grep", Category: "Grep",
			InputJSON:   `{"path":"/tmp","query":"needle"}`,
			EventStatus: "completed",
		},
		{
			ToolUseID: "switch-1", MessageOrdinal: 6, CallIndex: 0,
			ToolName: "Glob", Category: "Glob", ResultContent: "No files found",
		},
		{
			ToolUseID: "content-1", MessageOrdinal: 7, CallIndex: 1,
			ToolName: "Read", ResultContent: "package signals",
		},
	}

	got := ExtractToolSequences(calls, false)
	assert.Equal(t, []ToolCallOutcome{
		{
			ToolUseID: "empty-1", MessageOrdinal: 4, CallIndex: 0,
			ToolName: "Grep", Outcome: ToolOutcomeEmpty,
			Repeat: ToolRepeatNone,
		},
		{
			ToolUseID: "empty-2", MessageOrdinal: 5, CallIndex: 0,
			ToolName: "Grep", Outcome: ToolOutcomeEmpty,
			Repeat: ToolRepeatIdentical,
		},
		{
			ToolUseID: "switch-1", MessageOrdinal: 6, CallIndex: 0,
			ToolName: "Glob", Outcome: ToolOutcomeEmpty,
			Repeat: ToolRepeatNone, ToolChanged: true,
		},
		{
			ToolUseID: "content-1", MessageOrdinal: 7, CallIndex: 1,
			ToolName: "Read", Outcome: ToolOutcomeContent,
			Repeat: ToolRepeatNone, ToolChanged: true,
		},
	}, got.Calls)
	assert.Equal(t, []ToolSequence{{
		Start: 0, End: 4, Identical: true, ToolChanged: true,
		Ending: ToolSequenceEndingRecovered,
	}}, got.Sequences)
}

func TestExtractToolSequences_OpenCodeEmptyResults(t *testing.T) {
	for _, tt := range []struct {
		toolName string
		category string
		outcome  ToolOutcome
		ending   ToolSequenceEnding
	}{
		{"grep", "Grep", ToolOutcomeEmpty, ToolSequenceEndingAbandoned},
		{"glob", "Glob", ToolOutcomeEmpty, ToolSequenceEndingAbandoned},
		{"ripgrep", "Grep", ToolOutcomeContent, ToolSequenceEndingRecovered},
	} {
		t.Run(tt.toolName, func(t *testing.T) {
			got := ExtractToolSequences([]ToolCallRow{
				{MessageOrdinal: 1, ToolName: tt.toolName, Category: tt.category, EventStatus: "errored"},
				{
					MessageOrdinal: 2, ToolName: tt.toolName, Category: tt.category, EventStatus: "completed",
					ResultContent: "No files found", ResultContentLength: 14,
				},
			}, true)
			assert.Equal(t, tt.outcome, got.Calls[1].Outcome)
			assert.Equal(t, []ToolSequence{{Start: 0, End: 2, Ending: tt.ending}}, got.Sequences)
		})
	}
}

func TestExtractToolSequences_Repeats(t *testing.T) {
	calls := []ToolCallRow{
		{
			MessageOrdinal: 1,
			ToolName:       "Grep", InputJSON: `{"path":"/tmp","line":1}`,
			EventStatus: "errored",
		},
		{
			MessageOrdinal: 2,
			ToolName:       "Grep", InputJSON: `{"path":"/tmp","line":1}`,
			EventStatus: "errored",
		},
		{
			MessageOrdinal: 3,
			ToolName:       "Grep", InputJSON: "{\n  \"line\": 1,\n  \"path\": \"/tmp\"\n}",
			EventStatus: "errored",
		},
		{
			MessageOrdinal: 4,
			ToolName:       "Read", InputJSON: `{"path":"/tmp"}`,
			EventStatus: "errored",
		},
		{
			MessageOrdinal: 5,
			ToolName:       "Read", InputJSON: `{"path":"/tmp"}`,
			ResultContent: "content",
		},
	}

	got := ExtractToolSequences(calls, false)
	assert.Equal(t, []ToolRepeat{
		ToolRepeatNone, ToolRepeatIdentical, ToolRepeatNearIdentical,
		ToolRepeatNone, ToolRepeatIdentical,
	}, []ToolRepeat{
		got.Calls[0].Repeat, got.Calls[1].Repeat, got.Calls[2].Repeat,
		got.Calls[3].Repeat, got.Calls[4].Repeat,
	})
	assert.Equal(t, []bool{false, false, false, true, false}, []bool{
		got.Calls[0].ToolChanged, got.Calls[1].ToolChanged,
		got.Calls[2].ToolChanged, got.Calls[3].ToolChanged,
		got.Calls[4].ToolChanged,
	})
	assert.Equal(t, []ToolSequence{{
		Start: 0, End: 5, Identical: true, NearIdentical: true, ToolChanged: true,
		Ending: ToolSequenceEndingRecovered,
	}}, got.Sequences)
}

func TestExtractToolSequences_Endings(t *testing.T) {
	empty := ToolCallRow{MessageOrdinal: 1, ToolName: "Grep", Category: "Grep", EventStatus: "completed"}
	assert.Equal(t, []ToolSequence{{Start: 0, End: 1, Ending: ToolSequenceEndingAbandoned}}, ExtractToolSequences([]ToolCallRow{empty}, true).Sequences)
	assert.Equal(t, []ToolSequence{{Start: 0, End: 1, Ending: ToolSequenceEndingOpen}}, ExtractToolSequences([]ToolCallRow{empty}, false).Sequences)
	for _, tt := range []struct {
		name     string
		call     ToolCallRow
		complete bool
		ending   ToolSequenceEnding
	}{
		{"recovered", ToolCallRow{EventStatus: "success", ResultContent: "file contents"}, true, ToolSequenceEndingRecovered},
		{"completed empty", ToolCallRow{EventStatus: "success"}, true, ToolSequenceEndingAbandoned},
		{"unobserved content", ToolCallRow{EventStatus: "success", ResultContentUnknown: true}, true, ToolSequenceEndingUnknown},
		{"running", ToolCallRow{EventStatus: "running", ResultContent: "partial"}, true, ToolSequenceEndingUnknown},
		{"incomplete", ToolCallRow{ResultContentUnknown: true}, false, ToolSequenceEndingOpen},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.call.MessageOrdinal = 2
			tt.call.ToolName = "Read"
			tt.call.Category = "Read"
			got := ExtractToolSequences([]ToolCallRow{empty, tt.call}, tt.complete)
			assert.Equal(t, []ToolSequence{{Start: 0, End: 2, ToolChanged: true, Ending: tt.ending}}, got.Sequences)
		})
	}
	unknown := ToolCallRow{MessageOrdinal: 2, ToolName: "Read", EventStatus: "running"}
	assert.Empty(t, ExtractToolSequences([]ToolCallRow{unknown}, true).Sequences)
	got := ExtractToolSequences([]ToolCallRow{empty, unknown, {MessageOrdinal: 3, ToolName: "Bash", ResultContent: "done"}}, false)
	assert.Equal(t, []ToolSequence{{Start: 0, End: 3, ToolChanged: true, Ending: ToolSequenceEndingRecovered}}, got.Sequences)
}

func TestExtractToolSequences_Invariants(t *testing.T) {
	calls := []ToolCallRow{
		{
			ToolUseID: "duplicate", MessageOrdinal: 3, CallIndex: 2,
			ToolName: "Grep", Category: "Grep",
			InputJSON: `{"q":1}`, EventStatus: "completed",
		},
		{
			ToolUseID: "duplicate", MessageOrdinal: 4, CallIndex: 0,
			ToolName: "Grep", Category: "Grep",
			InputJSON: `{"q":1}`, EventStatus: "completed",
		},
		{
			MessageOrdinal: 5, CallIndex: 1, ToolName: "Bash",
			ResultContent: "answer",
		},
	}
	original := append([]ToolCallRow(nil), calls...)
	got := ExtractToolSequences(calls, false)
	assert.Equal(t, original, calls)
	assert.Equal(t, []string{"duplicate", "duplicate", ""}, []string{
		got.Calls[0].ToolUseID, got.Calls[1].ToolUseID, got.Calls[2].ToolUseID,
	})
	assert.Equal(t, []int{3, 4, 5}, []int{
		got.Calls[0].MessageOrdinal, got.Calls[1].MessageOrdinal,
		got.Calls[2].MessageOrdinal,
	})

	assert.Equal(t, ToolSequences{Calls: []ToolCallOutcome{}, Sequences: []ToolSequence{}}, ExtractToolSequences(nil, false))
	assert.Equal(t, ExtractToolSequences(nil, false), ExtractToolSequences([]ToolCallRow{}, false))
}

func TestNormalizeToolInputPreservesLargeNumbers(t *testing.T) {
	first, firstOK := normalizeToolInput(`{"n":90071992547409931234567890,"s":"x"}`)
	second, secondOK := normalizeToolInput(`{ "s": "x", "n": 90071992547409931234567890 }`)
	assert.True(t, firstOK)
	assert.True(t, secondOK)
	assert.Equal(t, first, second)
	assert.Contains(t, first, "90071992547409931234567890")
}

func TestClassifyToolRepeatMalformedInputs(t *testing.T) {
	assert.Equal(t, ToolRepeatIdentical, classifyToolRepeat(
		ToolCallRow{ToolName: "Grep", InputJSON: "{broken"},
		ToolCallRow{ToolName: "Grep", InputJSON: "{broken", EventStatus: "errored"},
	))
	assert.Equal(t, ToolRepeatNone, classifyToolRepeat(
		ToolCallRow{ToolName: "Grep", InputJSON: "{broken"},
		ToolCallRow{ToolName: "Grep", InputJSON: "{other"},
	))
	assert.Equal(t, ToolRepeatNone, classifyToolRepeat(
		ToolCallRow{ToolName: "Grep"},
		ToolCallRow{ToolName: "Grep"},
	))
}

func TestExtractToolSequences_MessageBoundaries(t *testing.T) {
	calls := []ToolCallRow{
		{MessageOrdinal: 1, ToolName: "Grep", InputJSON: `{"q":"a"}`, ResultContent: "No matches found"},
		{MessageOrdinal: 1, CallIndex: 1, ToolName: "Grep", InputJSON: `{"q":"a"}`, ResultContent: "No matches found"},
		{MessageOrdinal: 1, CallIndex: 2, ToolName: "Glob", ResultContent: "src/a.go"},
	}
	got := ExtractToolSequences(calls, true)
	require.Len(t, got.Sequences, 1)
	assert.Equal(t, ToolRepeatNone, got.Calls[1].Repeat)
	assert.False(t, got.Calls[2].ToolChanged)
	assert.NotEqual(t, ToolSequenceEndingRecovered, got.Sequences[0].Ending)
	calls = append(calls, ToolCallRow{MessageOrdinal: 2, ToolName: "Read", ResultContent: "file contents"})
	got = ExtractToolSequences(calls, true)
	assert.Equal(t, []ToolSequence{{Start: 0, End: 4, ToolChanged: true, Ending: ToolSequenceEndingRecovered}}, got.Sequences)
}

func TestExtractToolSequences_UnknownTail(t *testing.T) {
	got := ExtractToolSequences([]ToolCallRow{
		{MessageOrdinal: 1, ToolName: "Grep", ResultContent: "No matches found"},
		{MessageOrdinal: 2, ToolName: "Read", EventStatus: "running"},
	}, true)
	assert.Equal(t, []ToolSequence{{Start: 0, End: 2, ToolChanged: true, Ending: ToolSequenceEndingUnknown}}, got.Sequences)
}
