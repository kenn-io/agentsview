package signals

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyToolOutcome(t *testing.T) {
	image := `[ {"type":"input_image","image_url":"data:image/png;base64,AAEC"} ]`
	projectedImage := `[{"byte_size":3,"media_type":"image/png","sha256":"","text":"[Image: image/png, 3 bytes]","type":"agentsview_image","version":1}]`
	offloadedImage := `[{"byte_size":3,"image_ref":"asset://abc.png","media_type":"image/png","sha256":"abc","text":"![Image: image/png, 3 bytes](asset://abc.png)","type":"agentsview_image","version":1}]`
	tests := []struct {
		name string
		call ToolCallRow
		want ToolOutcome
	}{
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
				ToolName: "Grep", EventStatus: "completed",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "measured grep no matches",
			call: ToolCallRow{
				ToolName: "Grep", ResultContent: "No matches found",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "measured grep no files",
			call: ToolCallRow{
				ToolName: "Grep", ResultContent: "No files found",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "measured glob no files",
			call: ToolCallRow{
				ToolName: "Glob", ResultContent: "No files found",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "search style empty",
			call: ToolCallRow{
				ToolName: "search_web", EventStatus: "completed",
			},
			want: ToolOutcomeEmpty,
		},
		{
			name: "missing completion evidence",
			call: ToolCallRow{ToolName: "Grep", ResultContentLength: 17},
			want: ToolOutcomeUnknown,
		},
		{
			name: "empty content with retained length 17",
			call: ToolCallRow{
				ToolName: "Grep", EventStatus: "completed",
				ResultContentLength: 17,
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "unsupported completed empty",
			call: ToolCallRow{ToolName: "Bash", EventStatus: "completed"},
			want: ToolOutcomeUnknown,
		},
		{
			name: "running status",
			call: ToolCallRow{
				ToolName: "Read", EventStatus: "running",
				ResultContent: "text",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "future status",
			call: ToolCallRow{
				ToolName: "Read", EventStatus: "future",
				ResultContent: "text",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "direct staged marker",
			call: ToolCallRow{
				ToolName: "Bash", EventStatus: "completed",
				ResultContent: "staged:7",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "labeled staged marker",
			call: ToolCallRow{
				ToolName: "Bash", ResultContent: "agent-a:\nstaged:7\n\nagent-b:\nstaged:8",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "anonymous joined staged markers",
			call: ToolCallRow{
				ToolName: "Bash", ResultContent: "staged:7\n\nstaged:8",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "ordinary staged text",
			call: ToolCallRow{
				ToolName: "Bash", ResultContent: "saved staged:7 output",
			},
			want: ToolOutcomeContent,
		},
		{
			name: "direct inline image",
			call: ToolCallRow{ToolName: "Read", ResultContent: image},
			want: ToolOutcomeUnknown,
		},
		{
			name: "direct inline image with blank lines",
			call: ToolCallRow{ToolName: "Read", ResultContent: "[\n\n{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,AAEC\"}\n]"},
			want: ToolOutcomeUnknown,
		},
		{
			name: "direct projected image",
			call: ToolCallRow{ToolName: "Read", ResultContent: projectedImage},
			want: ToolOutcomeUnknown,
		},
		{
			name: "direct offloaded image",
			call: ToolCallRow{ToolName: "Read", ResultContent: offloadedImage},
			want: ToolOutcomeUnknown,
		},
		{
			name: "labeled image",
			call: ToolCallRow{
				ToolName: "Read",
				ResultContent: "agent-a:\n" + projectedImage +
					"\n\nagent-b:\n" + offloadedImage,
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "labeled offload reference",
			call: ToolCallRow{
				ToolName:      "Read",
				ResultContent: "agent-a:\n![Image: image/png, 3 bytes](asset://abc.png)",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "labeled inline image with blank lines",
			call: ToolCallRow{
				ToolName:      "Read",
				ResultContent: "agent-a:\n[\n\n{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,AAEC\"}\n]",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "joined labeled images with blank lines",
			call: ToolCallRow{
				ToolName: "Read",
				ResultContent: "agent-a:\n[\n\n{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,AAEC\"}\n]" +
					"\n\nagent-b:\n[\n\n{\"type\":\"agentsview_image\",\"text\":\"![Image: image/png, 3 bytes](asset://abc.png)\"}\n]",
			},
			want: ToolOutcomeUnknown,
		},
		{
			name: "offload reference followed by text",
			call: ToolCallRow{
				ToolName:      "Read",
				ResultContent: "![Image: image/png, 3 bytes](asset://abc.png)\nFound target in src/main.go (line 12)",
			},
			want: ToolOutcomeContent,
		},
		{
			name: "same-line image references with text",
			call: ToolCallRow{
				ToolName:      "Read",
				ResultContent: "![Image: image/png, 3 bytes](asset://a.png) Found target ![Image: image/png, 3 bytes](asset://b.png)",
			},
			want: ToolOutcomeContent,
		},
		{
			name: "mixed image and text",
			call: ToolCallRow{
				ToolName:      "Read",
				ResultContent: `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"text","text":"kept text"}]`,
			},
			want: ToolOutcomeContent,
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
			ToolName: "Grep", InputJSON: `{"path":"/tmp","query":"needle"}`,
			EventStatus: "completed",
		},
		{
			ToolUseID: "empty-2", MessageOrdinal: 5, CallIndex: 0,
			ToolName: "Grep", InputJSON: `{"path":"/tmp","query":"needle"}`,
			EventStatus: "completed",
		},
		{
			ToolUseID: "switch-1", MessageOrdinal: 6, CallIndex: 0,
			ToolName: "Glob", ResultContent: "No files found",
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

func TestExtractToolSequences_Repeats(t *testing.T) {
	calls := []ToolCallRow{
		{
			ToolName: "Grep", InputJSON: `{"path":"/tmp","line":1}`,
			EventStatus: "errored",
		},
		{
			ToolName: "Grep", InputJSON: `{"path":"/tmp","line":1}`,
			EventStatus: "errored",
		},
		{
			ToolName: "Grep", InputJSON: "{\n  \"line\": 1,\n  \"path\": \"/tmp\"\n}",
			EventStatus: "errored",
		},
		{
			ToolName: "Read", InputJSON: `{"path":"/tmp"}`,
			EventStatus: "errored",
		},
		{
			ToolName: "Read", InputJSON: `{"path":"/tmp"}`,
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
	empty := ToolCallRow{ToolName: "Grep", EventStatus: "completed"}
	assert.Equal(t, []ToolSequence{{
		Start: 0, End: 1, Ending: ToolSequenceEndingAbandoned,
	}}, ExtractToolSequences([]ToolCallRow{empty}, true).Sequences)
	assert.Equal(t, []ToolSequence{{
		Start: 0, End: 1, Ending: ToolSequenceEndingOpen,
	}}, ExtractToolSequences([]ToolCallRow{empty}, false).Sequences)

	recovered := ExtractToolSequences([]ToolCallRow{
		empty, {ToolName: "Bash", ResultContent: "done"},
	}, true)
	assert.Equal(t, []ToolSequence{{
		Start: 0, End: 2, ToolChanged: true, Ending: ToolSequenceEndingRecovered,
	}}, recovered.Sequences)

	unknown := ToolCallRow{ToolName: "Read", EventStatus: "running"}
	assert.Empty(t, ExtractToolSequences([]ToolCallRow{unknown}, true).Sequences)
	assert.Empty(t, ExtractToolSequences([]ToolCallRow{
		unknown, {ToolName: "Bash", ResultContent: "done"},
	}, true).Sequences)

	activeUnknown := ExtractToolSequences([]ToolCallRow{
		empty,
		{ToolName: "Read", ResultContent: "staged:7\n\nstaged:8"},
	}, false)
	assert.Equal(t, []ToolSequence{{
		Start: 0, End: 2, ToolChanged: true,
		Ending: ToolSequenceEndingOpen,
	}}, activeUnknown.Sequences)

	activeUnknownRecovered := ExtractToolSequences([]ToolCallRow{
		empty,
		{ToolName: "Read", EventStatus: "running", ResultContent: "partial"},
		{ToolName: "Bash", ResultContent: "done"},
	}, false)
	assert.Equal(t, []ToolSequence{{
		Start: 0, End: 3, ToolChanged: true,
		Ending: ToolSequenceEndingRecovered,
	}}, activeUnknownRecovered.Sequences)
}

func TestExtractToolSequences_Invariants(t *testing.T) {
	calls := []ToolCallRow{
		{
			ToolUseID: "duplicate", MessageOrdinal: 3, CallIndex: 2,
			ToolName: "Grep", InputJSON: `{"q":1}`, EventStatus: "completed",
		},
		{
			ToolUseID: "duplicate", MessageOrdinal: 4, CallIndex: 0,
			ToolName: "Grep", InputJSON: `{"q":1}`, EventStatus: "completed",
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

	assert.Equal(t, ToolSequences{}, ExtractToolSequences(nil, false))
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
