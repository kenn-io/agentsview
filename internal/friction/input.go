package friction

import (
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/serdejson"
	"go.kenn.io/agentsview/internal/signals"
)

// RawMessage is one archived message row, as the caller read it from
// the store. The adapter never reads the store itself.
type RawMessage struct {
	Ordinal           int
	Role              string
	Content           string
	ThinkingText      string
	IsSystem          bool
	IsCompactBoundary bool
	SourceSubtype     string
	Timestamp         time.Time
	ContextTokens     int
	HasContextTokens  bool
}

// RawToolCall is one archived tool call. LastEventContent and
// EventStatus come from the call's last tool_result_events row.
// Timestamp is the call's time when known; zero falls back to the
// owning message's timestamp.
type RawToolCall struct {
	MessageOrdinal      int
	CallIndex           int
	ToolName            string
	Category            string
	InputJSON           string
	ResultContent       string
	LastEventContent    string
	EventStatus         string
	ContentFailure      bool
	ContentFailureKnown bool
	Timestamp           time.Time
}

// BuildOptions tunes BuildSessionInput.
type BuildOptions struct {
	// RedactedToolRenderings prefers the redacted rendering when
	// stripping tool calls from assistant text (storage policies that
	// drop tool inputs store the redacted form).
	RedactedToolRenderings bool
	// PressureMax is the session's peak context pressure as the
	// quality signal pass computed it (sessions.context_pressure_max).
	PressureMax *float64
}

// SessionInput is one subject mapped into the stream the detectors
// expect.
type SessionInput struct {
	SubjectID  string
	Dims       Dims
	IsSubAgent bool
	Excluded   bool
	Messages   []Message
	// Interruptions holds the rows with is_system=1 and
	// source_subtype "interrupted" that Messages drops as system rows.
	// It feeds DetectInterruptions (spec §6.8).
	Interruptions []Message
	Patterns      PatternInput
}

func (c RawToolCall) row() signals.ToolCallRow {
	return signals.ToolCallRow{
		ToolName:            c.ToolName,
		Category:            c.Category,
		InputJSON:           c.InputJSON,
		ResultContent:       c.ResultContent,
		MessageOrdinal:      c.MessageOrdinal,
		CallIndex:           c.CallIndex,
		EventStatus:         c.EventStatus,
		ContentFailure:      c.ContentFailure,
		ContentFailureKnown: c.ContentFailureKnown,
	}
}

// ToolEnvelope synthesizes the {"error","success"} tool result envelope
// jilog's error detector reads (spec §6.3, D12). success is the
// negation of signals.IsFailure. A result event is present iff
// EventStatus != "" (the caller fills EventStatus and LastEventContent
// from the last tool_result_events row). error is that event's content
// when an event is present and its content is non-empty, else the
// call's result content, else null.
func ToolEnvelope(c RawToolCall) string {
	var errVal any
	switch {
	case c.EventStatus != "" && c.LastEventContent != "":
		errVal = c.LastEventContent
	case c.ResultContent != "":
		errVal = c.ResultContent
	}
	return serdejson.CompactString(map[string]any{
		"error":   errVal,
		"success": !signals.IsFailure(c.row()),
	})
}

// NoiseToolName is the name the expected-noise allowlist matches
// (spec §6.3): "bash" for any Bash-category call, otherwise the
// lowercased tool name.
func NoiseToolName(toolName, category string) string {
	if category == "Bash" {
		return "bash"
	}
	return strings.ToLower(toolName)
}
