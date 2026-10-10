package insight

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/stringutil"
)

const (
	ToolEffectivenessType          = "tool_effectiveness"
	ToolEffectivenessSchemaVersion = "tool_effectiveness.v1"
	// toolEvidenceBudgetBytes bounds call input and result text only.
	toolEvidenceBudgetBytes = 256 << 10
)

// Assessment values a tool-effectiveness conclusion may carry.
const (
	AssessmentHelped     = "helped"
	AssessmentDidNotHelp = "did_not_help"
	AssessmentUnknown    = "unknown"
)

// Omission reasons recorded for evidence the model did not see in full.
const (
	OmissionBudget     = "budget"
	OmissionUnretained = "unretained"
	OmissionNoResult   = "no_result"
	OmissionPreviews   = "previews"
)

const toolEffectivenessInstruction = "You are assessing how the tool calls in one AI agent session served its task. " +
	"Reply with only a JSON object of this shape and no other text: " +
	`{"conclusions":[{"assessment":"helped|did_not_help|unknown","text":"...","ordinals":[N],"calls":[{"ordinal":N,"call_index":I}]}]}. ` +
	"Each conclusion cites the message ordinals that support it in \"ordinals\", and names the exact call in \"calls\" when a message holds several. " +
	"Cite only message ordinals and calls that appear in this prompt. " +
	"An empty result can be useful evidence; a call with status completed and an empty result finished without output, as a silent command does. " +
	"A successful tool call does not prove the task succeeded. " +
	"Answer unknown when the evidence is insufficient, including inputs or results marked cut, not retained, or not recorded. " +
	"Do not assign session cost, tokens, or time to a single tool.\n"

// ToolEvidenceOmission counts one kind of evidence the prompt cut or never
// had. Each call's own cut is marked beside its evidence, so a long session
// adds no more than one omission per kind.
type ToolEvidenceOmission struct {
	Reason string `json:"reason"`
	// Field names what a budget cut shortened: input or result.
	Field string `json:"field,omitempty"`
	Count int    `json:"count"`
	// KeptBytes is the most a budget cut kept of any one field.
	KeptBytes *int `json:"kept_bytes,omitempty"`
	// KeptChars is the length of each message preview.
	KeptChars *int `json:"kept_chars,omitempty"`
}

// ToolEffectivenessEvidence records what a prompt sent, for validating the reply.
type ToolEffectivenessEvidence struct {
	SessionID          string
	CallCount          int
	TranscriptRevision string
	TerminationStatus  string
	Omissions          []ToolEvidenceOmission
	evidenceBytes      int
	sentOrdinals       map[int]bool
	sentCalls          map[[2]int]bool
	callDetails        map[[2]int]ToolEffectivenessCitedCall
	// sequenceLines summarize the observed sequences for the saved Markdown.
	sequenceLines     []string
	allResultsUnknown bool
}

// ToolEffectivenessCallRef names one call by message ordinal and call index.
type ToolEffectivenessCallRef struct {
	Ordinal   int `json:"ordinal"`
	CallIndex int `json:"call_index"`
}

// UnmarshalJSON requires both fields, so a missing call_index cannot default
// to call 0 of a message that holds several calls.
func (c *ToolEffectivenessCallRef) UnmarshalJSON(data []byte) error {
	var raw struct {
		Ordinal   *int `json:"ordinal"`
		CallIndex *int `json:"call_index"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Ordinal == nil || raw.CallIndex == nil {
		return errors.New("call reference needs both ordinal and call_index")
	}
	*c = ToolEffectivenessCallRef{Ordinal: *raw.Ordinal, CallIndex: *raw.CallIndex}
	return nil
}

// ToolEffectivenessConclusion is one model judgment with its citations.
type ToolEffectivenessConclusion struct {
	Assessment string                     `json:"assessment"`
	Text       string                     `json:"text"`
	Ordinals   []int                      `json:"ordinals"`
	Calls      []ToolEffectivenessCallRef `json:"calls"`
}

// ToolEffectivenessReport is the JSON object the model returns.
type ToolEffectivenessReport struct {
	Conclusions []ToolEffectivenessConclusion `json:"conclusions"`
}

// ToolEffectivenessCitedCall describes one cited call so the report view can
// label it without loading the transcript.
type ToolEffectivenessCitedCall struct {
	Ordinal   int `json:"ordinal"`
	CallIndex int `json:"call_index"`
	// ToolUseID lets a jump check that the message still holds this call.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// CallFingerprint lets a jump check the call's tool name and input as
	// well, since some agents derive tool IDs from the message position.
	CallFingerprint string `json:"call_fingerprint"`
	ToolName        string `json:"tool_name"`
	InputPreview    string `json:"input_preview"`
	Outcome         string `json:"outcome"`
	ResultBytes     *int   `json:"result_bytes,omitempty"`
	// ResultKeptBytes is how much of the result the model saw when the
	// budget cut it.
	ResultKeptBytes *int `json:"result_kept_bytes,omitempty"`
	// MessageCalls tells the view whether an ordinal-only citation names
	// this call or a message with several calls.
	MessageCalls int `json:"message_calls"`
}

// ToolEffectivenessStructured is the saved structured_json payload.
type ToolEffectivenessStructured struct {
	SessionID   string                        `json:"session_id"`
	CallCount   int                           `json:"call_count"`
	Conclusions []ToolEffectivenessConclusion `json:"conclusions"`
	Omissions   []ToolEvidenceOmission        `json:"omissions"`
	// TranscriptRevision lets the report view notice later transcript writes.
	TranscriptRevision string `json:"transcript_revision,omitempty"`
	// TerminationStatus decides whether a trailing sequence is open or
	// abandoned, so the view also flags reports whose status changed.
	TerminationStatus string `json:"termination_status"`
	// CitedCalls covers every call a conclusion names, including the only
	// call of a message cited by ordinal alone.
	CitedCalls []ToolEffectivenessCitedCall `json:"cited_calls,omitempty"`
}

// toolCitationPreviewBytes matches the tool-sequences input preview cap.
const toolCitationPreviewBytes = 512

// ErrNoCitableMessages means the session has no user or assistant message
// for a conclusion to cite.
var ErrNoCitableMessages = errors.New("this session has no messages to analyze")

// loadCheckedSessionPromptInput reads the session through the same revision
// check as the tool sequences panel, so the report never mixes two versions.
func loadCheckedSessionPromptInput(
	ctx context.Context,
	database db.Store,
	sessionID string,
) (sessionPromptInput, error) {
	var in sessionPromptInput
	sess, err := db.ReadSessionChecked(ctx, database, sessionID, func(sess *db.Session) error {
		var err error
		in, err = loadSessionEvidence(ctx, database, sess)
		return err
	})
	if err != nil {
		return sessionPromptInput{}, err
	}
	if sess == nil {
		return sessionPromptInput{}, fmt.Errorf("session not found: %s", sessionID)
	}
	return in, nil
}

func revisionOf(sess *db.Session) string {
	if sess.TranscriptRevision == nil {
		return ""
	}
	return *sess.TranscriptRevision
}

func terminationOf(sess *db.Session) string {
	if sess.TerminationStatus == nil {
		return ""
	}
	return *sess.TerminationStatus
}

// ErrPromptTooLarge means the prompt can't fit the agent's argument limit,
// even with no tool evidence.
var ErrPromptTooLarge = errors.New("this prompt is too large for the selected agent; choose another agent")

// BuildToolEffectivenessPrompt writes the session prompt plus the tool
// evidence and omissions sections, and returns what it sent. When
// req.MaxPromptBytes is set, the evidence budget shrinks to fit it with room
// left for one correction.
func BuildToolEffectivenessPrompt(
	ctx context.Context,
	database db.Store,
	req GenerateRequest,
) (string, ToolEffectivenessEvidence, error) {
	in, err := loadCheckedSessionPromptInput(ctx, database, req.SessionID)
	if err != nil {
		return "", ToolEffectivenessEvidence{SessionID: req.SessionID}, err
	}
	budget := toolEvidenceBudgetBytes
	step, prevSize := 0, -1
	limit := req.MaxPromptBytes - toolEffectivenessCorrectionReserve
	for {
		prompt, ev, err := buildToolEffectivenessPrompt(req, in, budget)
		if err != nil || req.MaxPromptBytes <= 0 {
			return prompt, ev, err
		}
		size := promptArgSize(prompt)
		over := size - limit
		if over <= 0 {
			return prompt, ev, nil
		}
		if budget == 0 {
			break
		}
		// Per-field rounding and argument escaping can leave the size unchanged, so the cut grows until it bites.
		if prevSize >= 0 && size >= prevSize {
			step *= 2
		} else {
			step = over
		}
		prevSize = size
		budget = max(0, min(budget, ev.evidenceBytes)-step)
	}
	return "", ToolEffectivenessEvidence{SessionID: req.SessionID}, ErrPromptTooLarge
}

func buildToolEffectivenessPrompt(
	req GenerateRequest,
	in sessionPromptInput,
	budget int,
) (string, ToolEffectivenessEvidence, error) {
	ev := ToolEffectivenessEvidence{SessionID: req.SessionID}
	ev.TranscriptRevision = revisionOf(in.sess)
	ev.TerminationStatus = terminationOf(in.sess)
	var b strings.Builder
	writeSystemInstruction(&b, ToolEffectivenessType)
	previews := writeSessionBody(&b, req.SessionID, in)

	ev.sentOrdinals = make(map[int]bool, len(in.msgs))
	for _, m := range in.msgs {
		if !m.IsSystem {
			ev.sentOrdinals[m.Ordinal] = true
		}
	}
	// Every conclusion must cite a message, so a session without one can't pass validation.
	if len(ev.sentOrdinals) == 0 {
		return "", ev, ErrNoCitableMessages
	}

	rows := ingest.ExtractToolCallRows(in.msgs)
	observed := signals.ExtractToolSequences(rows, parser.TerminationComplete(in.sess.TerminationStatus))
	states := recordToolCalls(&ev, rows, observed)
	kept, omissions := fitToolEvidence(&ev, rows, states, budget)
	writeToolEvidence(&b, &ev, rows, observed, states, kept)

	withheld, missing := 0, 0
	for _, state := range states {
		switch state {
		case resultWithheld:
			withheld++
		case resultMissing:
			missing++
		case resultSent:
		}
	}
	if withheld > 0 {
		omissions = append(omissions, ToolEvidenceOmission{Reason: OmissionUnretained, Field: "result", Count: withheld})
	}
	if missing > 0 {
		omissions = append(omissions, ToolEvidenceOmission{Reason: OmissionNoResult, Field: "result", Count: missing})
	}
	if previews > 0 {
		omissions = append(omissions, ToolEvidenceOmission{
			Reason: OmissionPreviews, Count: previews, KeptChars: new(sessionPreviewRunes),
		})
	}
	ev.Omissions = omissions

	b.WriteString("## Omissions\n\n")
	if len(omissions) == 0 {
		b.WriteString("None.\n")
	}
	for _, o := range omissions {
		b.WriteString("- ")
		b.WriteString(describeOmission(o))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	writeUserQuery(&b, req.Prompt)
	return b.String(), ev, nil
}

// resultState says whether a call's result text can go in the prompt.
type resultState int

const (
	resultSent resultState = iota
	// resultWithheld keeps the result's length or error status but not its text.
	resultWithheld
	// resultMissing means the call never recorded a finished result.
	resultMissing
)

// classifyResult treats a finished call with no output and no length as
// having returned nothing, which is evidence too.
func classifyResult(row signals.ToolCallRow, outcome signals.ToolOutcome) resultState {
	if row.ResultContentUnknown || (row.ResultContent == "" && row.ResultContentLength > 0) {
		return resultWithheld
	}
	if row.ResultContent == "" && outcome == signals.ToolOutcomeUnknown && !signals.IsCompletedToolStatus(row.EventStatus) {
		return resultMissing
	}
	return resultSent
}

// recordToolCalls notes every call the prompt sends and the details a
// citation of it saves, and returns each call's result state.
func recordToolCalls(
	ev *ToolEffectivenessEvidence,
	rows []signals.ToolCallRow,
	observed signals.ToolSequences,
) []resultState {
	ev.CallCount = len(rows)
	ev.sentCalls = make(map[[2]int]bool, len(rows))
	ev.callDetails = make(map[[2]int]ToolEffectivenessCitedCall, len(rows))
	ev.allResultsUnknown = len(rows) > 0
	states := make([]resultState, len(rows))
	for i, row := range rows {
		outcome := observed.Calls[i].Outcome
		states[i] = classifyResult(row, outcome)
		// A finished call whose result is retained, even a silent command's empty one, is known evidence though its outcome class is unknown.
		if outcome != signals.ToolOutcomeUnknown || (signals.IsCompletedToolStatus(row.EventStatus) && states[i] == resultSent) {
			ev.allResultsUnknown = false
		}
		detail := ToolEffectivenessCitedCall{
			Ordinal: row.MessageOrdinal, CallIndex: row.CallIndex, ToolUseID: row.ToolUseID, ToolName: row.ToolName,
			InputPreview:    strings.Clone(stringutil.SafeTruncate(row.InputJSON, toolCitationPreviewBytes)),
			Outcome:         string(outcome),
			CallFingerprint: toolCallFingerprint(row.ToolName, row.InputJSON),
		}
		if states[i] == resultSent || row.ResultContentLength > 0 {
			detail.ResultBytes = new(max(row.ResultContentLength, len(row.ResultContent)))
		}
		key := [2]int{row.MessageOrdinal, row.CallIndex}
		ev.sentCalls[key] = true
		ev.callDetails[key] = detail
	}
	return states
}

// toolCallFingerprint hashes a call's tool name and input with 32-bit FNV-1a
// over their UTF-8 bytes. The frontend's toolCallFingerprint must match it.
func toolCallFingerprint(toolName, input string) string {
	h := fnv.New32a()
	h.Write([]byte(toolName))
	h.Write([]byte{0})
	h.Write([]byte(input))
	return fmt.Sprintf("%08x", h.Sum32())
}

// keptText is the part of one input or result the prompt sends.
type keptText struct {
	text string
	// original is the length before a budget cut, or zero when uncut.
	original int
}

type keptCall struct {
	input, result keptText
}

func keepText(text string, limit int) keptText {
	if limit < 0 || len(text) <= limit {
		return keptText{text: text}
	}
	return keptText{text: stringutil.SafeTruncate(text, limit), original: len(text)}
}

// fitToolEvidence cuts call inputs and results to fit budget, records how
// much of each cut result a citation's reader should know the model saw, and
// returns one omission per field kind that was cut.
func fitToolEvidence(
	ev *ToolEffectivenessEvidence,
	rows []signals.ToolCallRow,
	states []resultState,
	budget int,
) ([]keptCall, []ToolEvidenceOmission) {
	lengths := make([]int, 0, 2*len(rows))
	for i, row := range rows {
		lengths = append(lengths, len(row.InputJSON))
		if states[i] == resultSent {
			lengths = append(lengths, len(row.ResultContent))
		}
	}
	total := 0
	for _, n := range lengths {
		total += n
	}
	ev.evidenceBytes = min(total, budget)
	limit := toolEvidenceCap(lengths, budget)

	kept := make([]keptCall, len(rows))
	inputCuts, resultCuts := 0, 0
	for i, row := range rows {
		kept[i].input = keepText(row.InputJSON, limit)
		if kept[i].input.original > 0 {
			inputCuts++
		}
		if states[i] != resultSent {
			continue
		}
		kept[i].result = keepText(row.ResultContent, limit)
		if kept[i].result.original > 0 {
			resultCuts++
			key := [2]int{row.MessageOrdinal, row.CallIndex}
			detail := ev.callDetails[key]
			detail.ResultKeptBytes = new(len(kept[i].result.text))
			ev.callDetails[key] = detail
		}
	}
	var omissions []ToolEvidenceOmission
	if inputCuts > 0 {
		omissions = append(omissions, ToolEvidenceOmission{
			Reason: OmissionBudget, Field: "input", Count: inputCuts, KeptBytes: new(limit),
		})
	}
	if resultCuts > 0 {
		omissions = append(omissions, ToolEvidenceOmission{
			Reason: OmissionBudget, Field: "result", Count: resultCuts, KeptBytes: new(limit),
		})
	}
	return kept, omissions
}

func writeToolEvidence(
	b *strings.Builder,
	ev *ToolEffectivenessEvidence,
	rows []signals.ToolCallRow,
	observed signals.ToolSequences,
	states []resultState,
	kept []keptCall,
) {
	sequenceOf := make([]int, len(rows))
	for i, seq := range observed.Sequences {
		for j := seq.Start; j < seq.End && j < len(rows); j++ {
			sequenceOf[j] = i + 1
		}
	}
	b.WriteString("\n## Tool evidence\n\n")
	if len(rows) == 0 {
		b.WriteString("No tool calls found for this session.\n\n")
	}
	for i, row := range rows {
		fmt.Fprintf(b, "### Call msg %d #%d %s\n", row.MessageOrdinal, row.CallIndex, row.ToolName)
		fmt.Fprintf(b, "- Outcome: %s\n", observed.Calls[i].Outcome)
		if row.EventStatus != "" {
			fmt.Fprintf(b, "- Status: %s\n", row.EventStatus)
		}
		if sequenceOf[i] > 0 {
			fmt.Fprintf(b, "- Sequence: %d\n", sequenceOf[i])
		} else {
			b.WriteString("- Sequence: none\n")
		}
		b.WriteString("\n")
		writeEvidenceText(b, "Input", kept[i].input)
		switch states[i] {
		case resultWithheld:
			b.WriteString("Result:\n(result not retained)\n\n")
		case resultMissing:
			b.WriteString("Result:\n(no result recorded)\n\n")
		case resultSent:
			writeEvidenceText(b, "Result", kept[i].result)
		}
	}
	writeToolSequences(b, ev, rows, observed)
}

func writeEvidenceText(b *strings.Builder, label string, kept keptText) {
	if kept.original > 0 {
		fmt.Fprintf(b, "%s (cut to %d of %d bytes):\n", label, len(kept.text), kept.original)
	} else {
		b.WriteString(label + ":\n")
	}
	writeFenced(b, kept.text)
}

func writeToolSequences(
	b *strings.Builder,
	ev *ToolEffectivenessEvidence,
	rows []signals.ToolCallRow,
	observed signals.ToolSequences,
) {
	if len(observed.Sequences) == 0 {
		return
	}
	b.WriteString("### Sequences\n\n")
	for i, seq := range observed.Sequences {
		var ordinals []string
		var tools []string
		for j := seq.Start; j < seq.End && j < len(rows); j++ {
			ordinals = append(ordinals, strconv.Itoa(rows[j].MessageOrdinal))
			if !slices.Contains(tools, rows[j].ToolName) {
				tools = append(tools, rows[j].ToolName)
			}
		}
		line := fmt.Sprintf("messages %s; tools %s; ending %s; identical repeat %s; near-identical repeat %s; tool switch %s",
			strings.Join(ordinals, ", "), strings.Join(tools, ", "), seq.Ending,
			yesNo(seq.Identical), yesNo(seq.NearIdentical), yesNo(seq.ToolChanged))
		ev.sequenceLines = append(ev.sequenceLines, line)
		fmt.Fprintf(b, "- Sequence %d: %s\n", i+1, line)
	}
	b.WriteString("\n")
}

// toolEvidenceCap returns the largest per-field byte cap that fits every
// field within budget, or -1 when nothing needs cutting. It sorts lengths.
func toolEvidenceCap(lengths []int, budget int) int {
	total := 0
	for _, n := range lengths {
		total += n
	}
	if total <= budget {
		return -1
	}
	slices.Sort(lengths)
	remaining := budget
	for i, n := range lengths {
		left := len(lengths) - i
		if n*left > remaining {
			return remaining / left
		}
		remaining -= n
	}
	return -1
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// writeFenced fences text with a backtick run longer than any inside it.
func writeFenced(b *strings.Builder, text string) {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	b.WriteString(fence)
	b.WriteString("\n")
	b.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(fence)
	b.WriteString("\n\n")
}

func describeOmission(o ToolEvidenceOmission) string {
	switch o.Reason {
	case OmissionBudget:
		return fmt.Sprintf("%d call %ss cut to at most %d bytes each", o.Count, o.Field, derefInt(o.KeptBytes))
	case OmissionUnretained:
		return fmt.Sprintf("%d call results not retained", o.Count)
	case OmissionNoResult:
		return fmt.Sprintf("%d calls with no recorded result", o.Count)
	case OmissionPreviews:
		return fmt.Sprintf("%d messages shown as %d-character previews", o.Count, derefInt(o.KeptChars))
	}
	return o.Reason
}

// toolEffectivenessCorrectionReserve keeps room under an argument limit for
// the correction a rejected reply gets, escaping included.
const toolEffectivenessCorrectionReserve = 2 << 10

// ToolEffectivenessCorrectionPrompt asks the model once more after its reply
// failed validation, naming what was wrong.
func ToolEffectivenessCorrectionPrompt(prompt string, rejected error) string {
	return prompt + "\n## Correction\n\nYour previous reply was rejected: " +
		stringutil.SafeTruncate(rejected.Error(), 512) +
		"\nReply again with the complete JSON object, corrected.\n"
}

// ParseToolEffectivenessReport decodes the model reply after removing one
// surrounding code fence. A reply that wraps the object in prose, before or
// after it, falls back to the text between its first and last brace. Members
// the schema doesn't name are ignored, since validation checks every citation.
func ParseToolEffectivenessReport(content string) (ToolEffectivenessReport, error) {
	clean := strings.TrimSpace(content)
	if rest, fenced := strings.CutPrefix(clean, "```"); fenced {
		clean = rest
		if _, body, ok := strings.Cut(rest, "\n"); ok {
			clean = body
		}
		clean = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(clean), "```"))
	}
	var out ToolEffectivenessReport
	err := json.Unmarshal([]byte(clean), &out)
	if err != nil {
		start, end := strings.IndexByte(clean, '{'), strings.LastIndexByte(clean, '}')
		if start >= 0 && end > start {
			out = ToolEffectivenessReport{}
			if json.Unmarshal([]byte(clean[start:end+1]), &out) == nil {
				err = nil
			}
		}
	}
	if err != nil {
		return ToolEffectivenessReport{}, fmt.Errorf("parsing tool effectiveness JSON: %w", err)
	}
	// A citation repeated within one conclusion adds nothing, so it's dropped rather than failing a paid run.
	for i := range out.Conclusions {
		c := &out.Conclusions[i]
		slices.Sort(c.Ordinals)
		c.Ordinals = slices.Compact(c.Ordinals)
		seen := make(map[ToolEffectivenessCallRef]bool, len(c.Calls))
		c.Calls = slices.DeleteFunc(c.Calls, func(call ToolEffectivenessCallRef) bool {
			repeated := seen[call]
			seen[call] = true
			return repeated
		})
	}
	return out, nil
}

// ValidateToolEffectivenessReport rejects any conclusion citing evidence the
// prompt did not send, citing a multi-call message without naming the call,
// or judging a session with no known tool result.
func ValidateToolEffectivenessReport(r ToolEffectivenessReport, ev ToolEffectivenessEvidence) error {
	if len(r.Conclusions) == 0 {
		return errors.New("at least one conclusion is required")
	}
	unknownOnly := ev.CallCount == 0 || ev.allResultsUnknown
	callsPerMessage := make(map[int]int, len(ev.sentCalls))
	for key := range ev.sentCalls {
		callsPerMessage[key[0]]++
	}
	for i, c := range r.Conclusions {
		switch c.Assessment {
		case AssessmentHelped, AssessmentDidNotHelp, AssessmentUnknown:
		default:
			return fmt.Errorf("conclusion %d: invalid assessment %q", i, c.Assessment)
		}
		if unknownOnly && c.Assessment != AssessmentUnknown {
			return fmt.Errorf("conclusion %d: assessment must be unknown when no tool result is known", i)
		}
		if strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("conclusion %d: text is required", i)
		}
		if len(c.Ordinals) == 0 {
			return fmt.Errorf("conclusion %d: at least one ordinal is required", i)
		}
		for _, n := range c.Ordinals {
			if !ev.sentOrdinals[n] {
				return fmt.Errorf("conclusion %d: ordinal %d was not in the prompt", i, n)
			}
		}
		for _, call := range c.Calls {
			key := [2]int{call.Ordinal, call.CallIndex}
			if !ev.sentCalls[key] {
				return fmt.Errorf("conclusion %d: call msg %d #%d was not in the prompt", i, call.Ordinal, call.CallIndex)
			}
		}
		for _, n := range c.Ordinals {
			if callsPerMessage[n] > 1 && !slices.ContainsFunc(c.Calls, func(call ToolEffectivenessCallRef) bool { return call.Ordinal == n }) {
				return fmt.Errorf("conclusion %d: msg %d holds %d calls, so the conclusion must name one", i, n, callsPerMessage[n])
			}
		}
	}
	return nil
}

// ToolEffectivenessStructuredJSON returns the saved structured payload.
func ToolEffectivenessStructuredJSON(r ToolEffectivenessReport, ev ToolEffectivenessEvidence) ([]byte, error) {
	return json.Marshal(ToolEffectivenessStructured{
		SessionID: ev.SessionID, CallCount: ev.CallCount,
		Conclusions: r.Conclusions, Omissions: ev.Omissions,
		TranscriptRevision: ev.TranscriptRevision,
		TerminationStatus:  ev.TerminationStatus,
		CitedCalls:         citedCalls(r, ev),
	})
}

// citedCalls returns the details of every call the conclusions cite, sorted
// by message and call index.
func citedCalls(r ToolEffectivenessReport, ev ToolEffectivenessEvidence) []ToolEffectivenessCitedCall {
	onlyCall := make(map[int][2]int, len(ev.callDetails))
	callsPerMessage := make(map[int]int, len(ev.callDetails))
	for key := range ev.callDetails {
		callsPerMessage[key[0]]++
		onlyCall[key[0]] = key
	}
	cited := make(map[[2]int]bool)
	for _, c := range r.Conclusions {
		named := make(map[int]bool, len(c.Calls))
		for _, call := range c.Calls {
			cited[[2]int{call.Ordinal, call.CallIndex}] = true
			named[call.Ordinal] = true
		}
		for _, n := range c.Ordinals {
			if !named[n] && callsPerMessage[n] == 1 {
				cited[onlyCall[n]] = true
			}
		}
	}
	out := make([]ToolEffectivenessCitedCall, 0, len(cited))
	for key := range cited {
		if detail, ok := ev.callDetails[key]; ok {
			detail.MessageCalls = callsPerMessage[key[0]]
			out = append(out, detail)
		}
	}
	slices.SortFunc(out, func(a, b ToolEffectivenessCitedCall) int {
		if c := a.Ordinal - b.Ordinal; c != 0 {
			return c
		}
		return a.CallIndex - b.CallIndex
	})
	return out
}

// RenderToolEffectivenessMarkdown writes the report for CLI and export.
func RenderToolEffectivenessMarkdown(r ToolEffectivenessReport, ev ToolEffectivenessEvidence) string {
	var b strings.Builder
	b.WriteString("## Model assessment\n\n")
	for _, c := range r.Conclusions {
		cites := make([]string, 0, len(c.Ordinals)+len(c.Calls))
		for _, n := range c.Ordinals {
			cites = append(cites, fmt.Sprintf("msg %d", n))
		}
		for _, call := range c.Calls {
			cites = append(cites, fmt.Sprintf("msg %d #%d", call.Ordinal, call.CallIndex))
		}
		// A line break in the model's text would end the list item early.
		text := strings.Join(strings.Fields(c.Text), " ")
		fmt.Fprintf(&b, "- **%s**: %s (%s)\n", assessmentLabel(c.Assessment), text, strings.Join(cites, ", "))
	}
	b.WriteString("\n## Observed tool sequences\n\n")
	if len(ev.sequenceLines) == 0 {
		b.WriteString("None.\n")
	}
	for i, line := range ev.sequenceLines {
		fmt.Fprintf(&b, "- Sequence %d: %s\n", i+1, line)
	}
	b.WriteString("\n## Omissions\n\n")
	if len(ev.Omissions) == 0 {
		b.WriteString("None.\n")
	}
	for _, o := range ev.Omissions {
		b.WriteString("- ")
		b.WriteString(describeOmission(o))
		b.WriteString("\n")
	}
	return b.String()
}

func assessmentLabel(assessment string) string {
	switch assessment {
	case AssessmentHelped:
		return "Helped"
	case AssessmentDidNotHelp:
		return "Did not help"
	}
	return "Unclear"
}
