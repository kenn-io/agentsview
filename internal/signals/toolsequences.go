package signals

import (
	"encoding/json/jsontext"
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
	activeStart := -1
	identical := false
	nearIdentical := false
	toolChanged := false

	for i, call := range calls {
		outcome := classifyToolOutcome(call)
		observed := ToolCallOutcome{
			ToolUseID:      call.ToolUseID,
			MessageOrdinal: call.MessageOrdinal,
			CallIndex:      call.CallIndex,
			ToolName:       call.ToolName,
			Outcome:        outcome,
			Repeat:         ToolRepeatNone,
		}

		followup := activeStart >= 0 && call.MessageOrdinal > calls[i-1].MessageOrdinal
		if followup {
			observed.ToolChanged = call.ToolName != calls[i-1].ToolName
			observed.Repeat = classifyToolRepeat(
				calls[i-1], call,
			)
			identical = identical || observed.Repeat == ToolRepeatIdentical
			nearIdentical = nearIdentical ||
				observed.Repeat == ToolRepeatNearIdentical
			toolChanged = toolChanged || observed.ToolChanged
		}
		result.Calls = append(result.Calls, observed)

		if activeStart < 0 {
			if startsToolSequence(outcome) {
				activeStart = i
				identical = false
				nearIdentical = false
				toolChanged = false
			}
			continue
		}

		if followup && outcome == ToolOutcomeContent {
			result.Sequences = append(result.Sequences, ToolSequence{
				Start:         activeStart,
				End:           i + 1,
				Identical:     identical,
				NearIdentical: nearIdentical,
				ToolChanged:   toolChanged,
				Ending:        ToolSequenceEndingRecovered,
			})
			activeStart = -1
			continue
		}
	}

	if activeStart >= 0 {
		ending := ToolSequenceEndingOpen
		if complete {
			ending = ToolSequenceEndingUnknown
			if startsToolSequence(result.Calls[len(result.Calls)-1].Outcome) {
				ending = ToolSequenceEndingAbandoned
			}
		}
		result.Sequences = append(result.Sequences, ToolSequence{
			Start:         activeStart,
			End:           len(calls),
			Identical:     identical,
			NearIdentical: nearIdentical,
			ToolChanged:   toolChanged,
			Ending:        ending,
		})
	}
	return result
}

func startsToolSequence(outcome ToolOutcome) bool {
	return outcome == ToolOutcomeErrored || outcome == ToolOutcomeEmpty
}

func classifyToolOutcome(call ToolCallRow) ToolOutcome {
	if IsFailure(call) {
		return ToolOutcomeErrored
	}
	if call.EventStatus != "" && !IsCompletedToolStatus(call.EventStatus) {
		return ToolOutcomeUnknown
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

// IsCompletedToolStatus reports whether a provider status means the call finished.
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

func classifyToolRepeat(previous, current ToolCallRow) ToolRepeat {
	if previous.ToolName != current.ToolName || current.InputJSON == "" {
		return ToolRepeatNone
	}
	if previous.InputJSON == current.InputJSON && previous.InputJSON != "" {
		return ToolRepeatIdentical
	}
	if previous.InputJSON == "" {
		return ToolRepeatNone
	}
	previousNormalized, previousOK := normalizeToolInput(previous.InputJSON)
	currentNormalized, currentOK := normalizeToolInput(current.InputJSON)
	if previousOK && currentOK && previousNormalized == currentNormalized &&
		previous.InputJSON != current.InputJSON {
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
