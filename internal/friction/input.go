package friction

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/nanoclaw"
	"go.kenn.io/agentsview/internal/serdejson"
	"go.kenn.io/agentsview/internal/signals"
)

// SourceSubtypeInterrupted marks Claude's interrupted-request system rows.
const SourceSubtypeInterrupted = "interrupted"

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
	// UnwrapNanoClawEnvelope applies nanoclaw.UnwrapEnvelope to user text,
	// as jilog's NanoClaw reader does (nanoclaw.rs:292-313), so correction
	// lengths are measured on what the person wrote.
	UnwrapNanoClawEnvelope bool
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

// BuildSessionInput maps archived rows into the detector stream and pattern input.
// It does not mutate the caller's slices.
func BuildSessionInput(subjectID string, dims Dims, isSubAgent bool, msgs []RawMessage, calls []RawToolCall, opts BuildOptions) SessionInput {
	msgs = slices.Clone(msgs)
	slices.SortStableFunc(msgs, func(a, b RawMessage) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
	calls = slices.Clone(calls)
	slices.SortStableFunc(calls, func(a, b RawToolCall) int {
		return cmp.Or(cmp.Compare(a.MessageOrdinal, b.MessageOrdinal), cmp.Compare(a.CallIndex, b.CallIndex))
	})

	msgTime := make(map[int]time.Time, len(msgs))
	for _, m := range msgs {
		if _, ok := msgTime[m.Ordinal]; !ok {
			msgTime[m.Ordinal] = m.Timestamp
		}
	}
	callTime := func(c RawToolCall) time.Time {
		if !c.Timestamp.IsZero() {
			return c.Timestamp
		}
		return msgTime[c.MessageOrdinal]
	}

	in := SessionInput{SubjectID: subjectID, Dims: dims, IsSubAgent: isSubAgent}
	emitCall := func(c RawToolCall) {
		in.Messages = append(in.Messages, Message{
			Ordinal: c.MessageOrdinal, Role: "tool", Text: ToolEnvelope(c),
			ToolName: c.ToolName, NoiseName: NoiseToolName(c.ToolName, c.Category),
			CallIndex: c.CallIndex, Timestamp: callTime(c),
		})
	}

	j := 0
	for _, m := range msgs {
		for j < len(calls) && calls[j].MessageOrdinal < m.Ordinal {
			emitCall(calls[j])
			j++
		}
		k := j
		for k < len(calls) && calls[k].MessageOrdinal == m.Ordinal {
			k++
		}
		own := calls[j:k]
		switch {
		case m.IsCompactBoundary:
			in.Patterns.CompactBoundaries = append(in.Patterns.CompactBoundaries, m.Ordinal)
			in.Patterns.BoundaryTimes = append(in.Patterns.BoundaryTimes, m.Timestamp)
		case m.IsSystem && m.SourceSubtype == SourceSubtypeInterrupted:
			in.Interruptions = append(in.Interruptions, Message{
				Ordinal: m.Ordinal, Role: m.Role, Text: m.Content, Timestamp: m.Timestamp,
			})
		case m.IsSystem || m.SourceSubtype == "tool_result":
			// System and tool-result rows do not enter the correction stream.
		case m.Role == "user":
			// Every real user row is user activity for iteration_runaway,
			// envelope or not (nanoclaw.rs:457-464).
			in.Patterns.UserOrdinals = append(in.Patterns.UserOrdinals, m.Ordinal)
			text := m.Content
			if opts.UnwrapNanoClawEnvelope {
				text = nanoclaw.UnwrapEnvelope(text)
				if text == "" {
					// nanoclaw.rs:304-307: no text, no user message.
					break
				}
			}
			in.Messages = append(in.Messages, Message{
				Ordinal: m.Ordinal, Role: "user", Text: text, Timestamp: m.Timestamp,
			})
		case m.Role == "assistant":
			in.Messages = append(in.Messages, Message{
				Ordinal: m.Ordinal, Role: m.Role,
				Text:      AssistantText(m.Content, m.ThinkingText, own, opts.RedactedToolRenderings),
				Timestamp: m.Timestamp,
			})
		}
		for _, c := range own {
			emitCall(c)
		}
		j = k
	}
	for ; j < len(calls); j++ {
		emitCall(calls[j])
	}

	ordinals := make([]signals.ToolCallOrdinal, 0, len(calls))
	for _, c := range calls {
		in.Patterns.Calls = append(in.Patterns.Calls, c.row())
		in.Patterns.CallTimes = append(in.Patterns.CallTimes, callTime(c))
		ordinals = append(ordinals, signals.ToolCallOrdinal{MessageOrdinal: c.MessageOrdinal, ToolName: c.ToolName})
	}
	in.Patterns.MidTaskCompactions = signals.CountMidTaskCompactions(in.Patterns.CompactBoundaries, ordinals)
	in.Patterns.PressureMax = opts.PressureMax
	if opts.PressureMax != nil {
		in.Patterns.PressureAt = peakContextTime(msgs)
	}
	return in
}

// peakContextTime returns the first assistant timestamp at the maximum
// recorded context size, including system rows.
func peakContextTime(msgs []RawMessage) time.Time {
	var at time.Time
	best := -1
	for _, m := range msgs {
		if m.Role == "assistant" && m.HasContextTokens && m.ContextTokens > best {
			best, at = m.ContextTokens, m.Timestamp
		}
	}
	return at
}
