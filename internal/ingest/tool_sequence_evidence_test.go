package ingest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/signals"
)

func TestToolSequenceStructuredEvidence(t *testing.T) {
	for _, tt := range []struct {
		name   string
		events []db.ToolResultEvent
		want   signals.ToolOutcome
	}{
		{"images and staged", []db.ToolResultEvent{{AgentID: "a", Content: "[image]"}, {AgentID: "b", Content: "staged:7"}}, signals.ToolOutcomeUnknown},
		{"latest per agent", []db.ToolResultEvent{{AgentID: "a", Content: "old text"}, {AgentID: "a", Content: "[image]"}, {AgentID: "b", Content: "[binary content]"}}, signals.ToolOutcomeUnknown},
		{"mixed text", []db.ToolResultEvent{{AgentID: "a", Content: "[image]"}, {AgentID: "b", Content: "file contents"}}, signals.ToolOutcomeContent},
		{"anonymous latest", []db.ToolResultEvent{{Content: "old text"}, {Content: `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`}}, signals.ToolOutcomeUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			messages := []db.Message{{Ordinal: 1, ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "Read", ResultEvents: tt.events}}}}
			require.NoError(t, ingest.PairToolResultEventSummariesContext(t.Context(), messages, nil))
			got := signals.ExtractToolSequences(ingest.ExtractToolCallRows(messages), true)
			require.Len(t, got.Calls, 1)
			assert.Equal(t, tt.want, got.Calls[0].Outcome)
		})
	}
}

func TestToolSequenceUnstructuredEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		want          signals.ToolOutcome
	}{
		{"image placeholder", "[image]", signals.ToolOutcomeUnknown},
		{"repeated image placeholder", "[image][image]", signals.ToolOutcomeUnknown},
		{"binary placeholder", "[binary content]", signals.ToolOutcomeUnknown},
		{"staged", "staged:7", signals.ToolOutcomeUnknown},
		{"ordinary staged text", "saved staged:7 output", signals.ToolOutcomeContent},
		{"inline image", `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`, signals.ToolOutcomeUnknown},
		{"Amp image", `[{"type":"image","data":"AAEC"}]`, signals.ToolOutcomeUnknown},
		{"projected image", `[{"type":"agentsview_image","text":"[Image: image/png, 3 bytes]"}]`, signals.ToolOutcomeUnknown},
		{"offloaded image", `[{"type":"agentsview_image","image_ref":"asset://abc.png","text":"![Image: image/png, 3 bytes](asset://abc.png)"}]`, signals.ToolOutcomeUnknown},
		{"offloaded reference", "![Image: image/png, 3 bytes](asset://abc.png)", signals.ToolOutcomeUnknown},
		{"reference and text", "![Image: image/png, 3 bytes](asset://abc.png)\nFound target", signals.ToolOutcomeContent},
		{"image and empty text", `[{"type":"input_text","text":""},{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`, signals.ToolOutcomeUnknown},
		{"image and text", `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"},{"type":"output_text","text":"kept text"}]`, signals.ToolOutcomeContent},
		{"unknown JSON block", `[{"type":"future","text":"kept text"}]`, signals.ToolOutcomeContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := ingest.ExtractToolCallRows([]db.Message{{ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: tt.content}}}})
			got := signals.ExtractToolSequences(rows, true)
			require.Len(t, got.Calls, 1)
			assert.Equal(t, tt.want, got.Calls[0].Outcome)
		})
	}
}
