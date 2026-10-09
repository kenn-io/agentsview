package signals

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"fmt"
	"strings"
)

// ToolOutcome describes the retained evidence for a call, not task success.
type ToolOutcome string

// ToolRepeat compares a call's input with the immediately preceding call.
type ToolRepeat string

// ToolSequenceEnding describes the evidence at the end of a sequence.
type ToolSequenceEnding string

const (
	ToolOutcomeErrored ToolOutcome = "errored"
	ToolOutcomeEmpty   ToolOutcome = "empty"
	ToolOutcomeContent ToolOutcome = "content"
	ToolOutcomeUnknown ToolOutcome = "unknown"

	ToolRepeatNone          ToolRepeat = "none"
	ToolRepeatIdentical     ToolRepeat = "identical"
	ToolRepeatNearIdentical ToolRepeat = "near_identical"

	ToolSequenceEndingRecovered ToolSequenceEnding = "recovered"
	ToolSequenceEndingAbandoned ToolSequenceEnding = "abandoned"
	ToolSequenceEndingOpen      ToolSequenceEnding = "open"
	ToolSequenceEndingUnknown   ToolSequenceEnding = "unknown"
)

// ToolCallOutcome retains a call identity and its observed outcome and follow-up facts.
type ToolCallOutcome struct {
	ToolUseID      string
	MessageOrdinal int
	CallIndex      int
	ToolName       string
	Outcome        ToolOutcome
	Repeat         ToolRepeat
	ToolChanged    bool
}

// ToolSequence covers Calls[Start:End]; End is exclusive.
// Recovery means a later message returned content, not that the task succeeded.
type ToolSequence struct {
	Start         int
	End           int
	Identical     bool
	NearIdentical bool
	ToolChanged   bool
	Ending        ToolSequenceEnding
}

// ToolSequences preserves input order and groups calls from an error or empty result.
type ToolSequences struct {
	Calls     []ToolCallOutcome
	Sequences []ToolSequence
}

// ExtractToolSequences accepts calls ordered by message ordinal and call index.
// complete means the caller knows the session has ended, not merely that all
// currently available calls were loaded. Incomplete tails stay open. Completed
// tails are abandoned only when the last outcome is an error or empty result;
// insufficient evidence produces an unknown ending.
//
// Repeats and tool switches compare adjacent calls only, and require a later
// message ordinal. Calls in the same message cannot recover from each other.
// Grep A, Glob, Grep A therefore does not count as a repeat. Both nil and empty
// input return empty Calls and Sequences slices.
func ExtractToolSequences(calls []ToolCallRow, complete bool) ToolSequences {
	result := ToolSequences{
		Calls:     make([]ToolCallOutcome, 0, len(calls)),
		Sequences: make([]ToolSequence, 0),
	}
	var state ToolSequenceContinuation
	activeStart := -1
	for i, call := range calls {
		wasActive := state.Active != nil
		observed, recovered := state.Step(SequenceFactFor(call))
		observed.ToolUseID = call.ToolUseID
		result.Calls = append(result.Calls, observed)
		if !wasActive && state.Active != nil {
			activeStart = i
		}
		if recovered != nil {
			result.Sequences = append(result.Sequences, ToolSequence{Start: activeStart, End: i + 1, Identical: recovered.Identical, NearIdentical: recovered.NearIdentical, ToolChanged: recovered.ToolChanged, Ending: ToolSequenceEndingRecovered})
			activeStart = -1
		}
	}
	if state.Active != nil {
		result.Sequences = append(result.Sequences, ToolSequence{Start: activeStart, End: len(calls), Identical: state.Active.Identical, NearIdentical: state.Active.NearIdentical, ToolChanged: state.Active.ToolChanged, Ending: state.Ending(complete)})
	}

	return result
}

// ToolSequenceFact retains bounded comparison facts rather than result content.
type ToolSequenceFact struct {
	CallPos
	ToolName       string      `json:"tool_name"`
	Outcome        ToolOutcome `json:"outcome"`
	InputHash      string      `json:"input_hash,omitempty"`
	NormalizedHash string      `json:"normalized_hash,omitempty"`
}

type ToolSequenceProgress struct {
	Start         CallPos `json:"start"`
	Identical     bool    `json:"identical,omitempty"`
	NearIdentical bool    `json:"near_identical,omitempty"`
	ToolChanged   bool    `json:"tool_changed,omitempty"`
}

// ToolSequenceContinuation resumes immediately before the retained call window.
type ToolSequenceContinuation struct {
	Active   *ToolSequenceProgress `json:"active,omitempty"`
	Previous ToolSequenceFact      `json:"previous"`
}

func SequenceFactFor(call ToolCallRow) ToolSequenceFact {
	fact := ToolSequenceFact{CallPos: CallPos{call.MessageOrdinal, call.CallIndex}, ToolName: call.ToolName, Outcome: ClassifyToolOutcome(call)}
	if call.InputJSON != "" {
		fact.InputHash = fmt.Sprintf("%x", sha256.Sum256([]byte(call.InputJSON)))
		if normalized, ok := normalizeToolInput(call.InputJSON); ok {
			fact.NormalizedHash = fmt.Sprintf("%x", sha256.Sum256([]byte(normalized)))
		}
	}
	return fact
}

// Step applies the same adjacent-call and same-message rules for full and tail replay.
func (s *ToolSequenceContinuation) Step(call ToolSequenceFact) (ToolCallOutcome, *ToolSequenceProgress) {
	observed := ToolCallOutcome{MessageOrdinal: call.MessageOrdinal, CallIndex: call.CallIndex, ToolName: call.ToolName, Outcome: call.Outcome, Repeat: ToolRepeatNone}
	if call.MessageOrdinal > s.Previous.MessageOrdinal {
		observed.Repeat = compareSequenceInputs(s.Previous, call)
	}
	followup := s.Active != nil && call.MessageOrdinal > s.Previous.MessageOrdinal
	var recovered *ToolSequenceProgress
	if s.Active != nil {
		active := *s.Active
		s.Active = &active
	}
	if followup {
		observed.ToolChanged = call.ToolName != s.Previous.ToolName
		s.Active.Identical = s.Active.Identical || observed.Repeat == ToolRepeatIdentical
		s.Active.NearIdentical = s.Active.NearIdentical || observed.Repeat == ToolRepeatNearIdentical
		s.Active.ToolChanged = s.Active.ToolChanged || observed.ToolChanged
	}
	if s.Active == nil {
		if startsToolSequence(call.Outcome) {
			s.Active = &ToolSequenceProgress{Start: call.CallPos}
		}
	} else if followup && call.Outcome == ToolOutcomeContent {
		recovered = s.Active
		s.Active = nil
	}
	s.Previous = call
	return observed, recovered
}

func (s *ToolSequenceContinuation) Ending(complete bool) ToolSequenceEnding {
	if !complete {
		return ToolSequenceEndingOpen
	}
	if startsToolSequence(s.Previous.Outcome) {
		return ToolSequenceEndingAbandoned
	}
	return ToolSequenceEndingUnknown
}

func startsToolSequence(outcome ToolOutcome) bool {
	return outcome == ToolOutcomeErrored || outcome == ToolOutcomeEmpty
}

// ClassifyToolOutcome classifies one retained tool result.
func ClassifyToolOutcome(call ToolCallRow) ToolOutcome {
	if IsFailure(call) {
		return ToolOutcomeErrored
	}
	if call.EventStatus != "" && !IsCompletedToolStatus(call.EventStatus) {
		return ToolOutcomeUnknown
	}

	if call.ContentOutcome != "" {
		return call.ContentOutcome
	}
	if call.ResultContentUnknown {
		return ToolOutcomeUnknown
	}
	if IsCompletedToolStatus(call.EventStatus) && call.ResultContentLength == 0 &&
		call.ResultContent == "" && isSupportedEmptyTool(call) {
		return ToolOutcomeEmpty
	}
	if isMeasuredEmptyToolResult(call.ToolName, call.ResultContent) {
		return ToolOutcomeEmpty
	}
	if call.ResultContent == "" {
		return ToolOutcomeUnknown
	}
	return ToolOutcomeContent
}

// IsCompletedToolStatus reports a provider status that means the call finished normally.
func IsCompletedToolStatus(status string) bool {
	return status == "completed" || status == "success"
}

func isSupportedEmptyTool(call ToolCallRow) bool {
	switch call.Category {
	case "Read", "Grep", "Glob":
		return true
	case "Tool":
		switch call.ToolName {
		case "search", "WebSearch", "search_web", "web_search":
			return true
		}
	}
	return false
}

func isMeasuredEmptyToolResult(toolName, content string) bool {
	content = strings.TrimSpace(content)
	switch toolName {
	case "Grep":
		return content == "No matches found" || content == "No files found"
	case "Glob", "grep", "glob":
		return content == "No files found"
	default:
		return false
	}
}

func compareSequenceInputs(previous, current ToolSequenceFact) ToolRepeat {
	if previous.ToolName != current.ToolName || current.InputHash == "" {
		return ToolRepeatNone
	}
	if current.InputHash == previous.InputHash {
		return ToolRepeatIdentical
	}
	if current.NormalizedHash != "" && current.NormalizedHash == previous.NormalizedHash {
		return ToolRepeatNearIdentical
	}
	return ToolRepeatNone
}

func normalizeToolInput(input string) (string, bool) {
	value := jsontext.Value([]byte(input))
	if err := value.Format(
		jsontext.CanonicalizeRawInts(false),
		jsontext.CanonicalizeRawFloats(false),
		jsontext.ReorderRawObjects(true),
	); err != nil {
		return "", false
	}
	return string(value), true
}
