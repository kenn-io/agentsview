package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/stringutil"
)

// Wire structs for chat-messages.json.
//
// These are the memory contract: the whole point of the streaming decoder is
// that the transcript's per-message metadata.runState embeds the full project
// context (file tree, per-file token scores, knowledge files, agent templates
// with their handleSteps source, custom tool definitions, message history),
// which makes a transcript quadratic in conversation length. Only fields the
// parser consumes are declared; the v2 decoder skips every undeclared
// member's bytes instead of materializing them. Adding a field here for
// convenience can reintroduce the entire problem -- read the structs before
// the decode loop (plan 021).
//
// The `html` block type is deliberately absent: upstream marks it "NOT
// serializable - don't use for persistent data" (cli/src/types/chat.ts), so
// it cannot appear in a persisted transcript.
type codebuffWireMessage struct {
	ID      string `json:"id"`
	Variant string `json:"variant"`
	Content string `json:"content"`
	// Timestamp is always present in CLI-written transcripts: upstream's
	// ChatMessage type declares `timestamp: string` as required
	// (cli/src/types/chat.ts) and every message constructor fills it with
	// formatTimestamp(). Do not add handling for, or accept review findings
	// about, a message that lacks one. The value is a locale-formatted
	// hour and minute (Intl.DateTimeFormat with 2-digit hour and minute),
	// not ISO, so the date comes from the session directory; see
	// parseCodebuffTimestamp for the formats the parser accepts.
	Timestamp string `json:"timestamp"`
	ParentID  string `json:"parentId"`
	// Credits stays a raw value, not a float: presence (including an
	// explicit null or a non-numeric string) is what suppresses the legacy
	// run-state fallback, and the raw text feeds plan 020's decimal-safe
	// cost parsing exactly as gjson's .Raw did.
	Credits  jsontext.Value      `json:"credits"`
	Metadata *codebuffWireMeta   `json:"metadata"`
	Blocks   []codebuffWireBlock `json:"blocks"`

	// Fields plan 022 consumes: the message envelope beyond content and
	// blocks. userError is a runtime notice the app displayed via
	// setError(); an empty string means absent, matching upstream's
	// clearUserError() reset.
	UserError        string                        `json:"userError"`
	ValidationErrors []codebuffWireValidationError `json:"validationErrors"`
	Attachments      []codebuffWireImageAttachment `json:"attachments"`
	TextAttachments  []codebuffWireTextAttachment  `json:"textAttachments"`
	FileAttachments  []codebuffWireFileAttachment  `json:"fileAttachments"`
	IsComplete       *bool                         `json:"isComplete"`
	CompletionTime   string                        `json:"completionTime"`
	IsCompletion     *bool                         `json:"isCompletion"`
}

// codebuffWireValidationError is one upstream validation entry; only the
// message text is rendered, never the id.
type codebuffWireValidationError struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// codebuffWireImageAttachment is upstream ImageAttachment. The path member
// is deliberately undeclared: local filesystem paths are never stored.
type codebuffWireImageAttachment struct {
	Filename string `json:"filename"`
}

// codebuffWireTextAttachment is upstream TextAttachment. The full content
// member is deliberately undeclared -- storing a whole pasted document in
// the transcript is both a memory hazard and out of scope; only the
// preview (byte-limited at render time) and its charCount are kept.
type codebuffWireTextAttachment struct {
	Preview   string `json:"preview"`
	CharCount int64  `json:"charCount"`
}

// codebuffWireFileAttachment is upstream FileAttachment. The path member is
// deliberately undeclared: local filesystem paths are never stored.
type codebuffWireFileAttachment struct {
	Filename    string `json:"filename"`
	IsDirectory bool   `json:"isDirectory"`
	Note        string `json:"note"`
}

type codebuffWireMeta struct {
	RunState *codebuffWireRunState `json:"runState"`
}

// codebuffWireRunState retains only what plan 020's model resolution
// consumes. Everything else in the upstream RunState -- fileTree,
// fileTokenScores, tokenCallers, knowledgeFiles, userKnowledgeFiles,
// messageHistory, systemPrompt, agentTemplates' handleSteps source,
// customToolDefinitions, shellConfigFiles, systemInfo -- must stay absent
// from this struct so the decoder skips those bytes rather than holding the
// whole project context in memory.
type codebuffWireRunState struct {
	AgentType    string                    `json:"agentType"`
	Inference    *codebuffWireInference    `json:"inference"`
	SessionState *codebuffWireSessionState `json:"sessionState"`
	FileContext  *codebuffWireFileContext  `json:"fileContext"`
}

type codebuffWireInference struct {
	Source string `json:"source"`
	Model  string `json:"model"`
}

type codebuffWireSessionState struct {
	MainAgentState *codebuffWireMainAgentState `json:"mainAgentState"`
	FileContext    *codebuffWireFileContext    `json:"fileContext"`
}

// codebuffWireMainAgentState retains only the agent type and inference
// source; the upstream AgentState's messageHistory and systemPrompt stay
// undeclared so their bytes are skipped.
type codebuffWireMainAgentState struct {
	AgentType string                 `json:"agentType"`
	Inference *codebuffWireInference `json:"inference"`
}

type codebuffWireFileContext struct {
	AgentTemplates map[string]codebuffWireTemplate `json:"agentTemplates"`
}

type codebuffWireTemplate struct {
	Model string `json:"model"`
}

// codebuffWireBlock is a tagged union over `type`: one struct whose members
// cover the per-type shapes the parser reads. Members not present on a given
// block type decode as zero values, matching the gjson Get("") behavior this
// replaces. Params/Input/Output stay jsontext.Value so their raw bytes pass
// through byte-identical to the gjson .Raw values the parser stores.
type codebuffWireBlock struct {
	Type string `json:"type"`

	// text / image / plan
	TextType string `json:"textType"`
	Content  string `json:"content"`
	Filename string `json:"filename"`

	// tool
	ToolName   string         `json:"toolName"`
	ToolCallID string         `json:"toolCallId"`
	Input      jsontext.Value `json:"input"`
	Output     jsontext.Value `json:"output"`

	// agent
	AgentType     string              `json:"agentType"`
	AgentName     string              `json:"agentName"`
	AgentID       string              `json:"agentId"`
	Status        string              `json:"status"`
	Params        jsontext.Value      `json:"params"`
	InitialPrompt string              `json:"initialPrompt"`
	Blocks        []codebuffWireBlock `json:"blocks"`

	// mode-divider
	Mode string `json:"mode"`

	// ask-user
	Questions []codebuffWireQuestion `json:"questions"`
	Answers   []codebuffWireAnswer   `json:"answers"`
	Skipped   bool                   `json:"skipped"`

	// sponsored-proposal: target and consent.headline only. proposal,
	// consent.body, consent.folder, consent.branch, and consent.runId are
	// deliberately undeclared -- advertiser payloads and local paths or
	// branch names are never stored.
	Target  string               `json:"target"`
	Consent *codebuffWireConsent `json:"consent"`

	// agent-list: display names and ids only; agentsDir is deliberately
	// undeclared (local filesystem path).
	Agents []codebuffWireAgentEntry `json:"agents"`
}

// codebuffWireConsent retains only the headline an operator would see in a
// consent banner; body, folder, branch, and runId stay undeclared.
type codebuffWireConsent struct {
	AdvertiserName string `json:"advertiserName"`
	Headline       string `json:"headline"`
}

type codebuffWireAgentEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type codebuffWireQuestion struct {
	Question string `json:"question"`
	Header   string `json:"header"`
}

// codebuffWireAnswer is one user answer to an ask-user block, resolved by
// questionIndex against the questions array.
type codebuffWireAnswer struct {
	QuestionIndex   int      `json:"questionIndex"`
	SelectedOption  string   `json:"selectedOption"`
	SelectedOptions []string `json:"selectedOptions"`
	OtherText       string   `json:"otherText"`
}

// codebuffTranscript carries everything parseCodebuffSession needs from the
// transcript: parsed messages, per-turn billing facts (plan 020), the
// timestamp envelope, whether the file ended mid-write, and the nested
// subagents lifted into their own sessions.
type codebuffTranscript struct {
	Messages  []ParsedMessage
	TurnFacts []codebuffTurnFact
	Subagents []codebuffSubagent
	StartedAt time.Time
	EndedAt   time.Time
	Truncated bool
}

// codebuffSubagent is one nested `agent` block stored as its own session.
// Upstream records a subagent's whole exchange (reasoning, tool calls and
// their outputs, further subagents) in the block's recursive `blocks` array;
// each block becomes a child session linked to the session that spawned it,
// so its tool calls are stored and result-blocked per call like any other.
type codebuffSubagent struct {
	// ID is the child's full session ID; ParentID is the full session ID of
	// the session whose transcript holds the agent block (the root session
	// or another subagent).
	ID       string
	ParentID string
	// AgentID is the raw upstream agentId, empty when the block had none.
	AgentID   string
	AgentType string
	AgentName string
	// Timestamp is the containing message's timestamp: nested blocks carry
	// none of their own.
	Timestamp time.Time
	Messages  []ParsedMessage
}

// codebuffSubagentIDSep joins the owning session's full ID and a subagent
// key into the child session ID. It contains no ':' so raw-ID lookup can
// still split "<project>:<timestamp>", and no '~' (the host-prefix
// separator).
const codebuffSubagentIDSep = "__subagent__"

// codebuffSubagentSink allocates child session IDs and collects child
// transcripts in document order (a parent before its descendants).
type codebuffSubagentSink struct {
	rootID string
	used   map[string]bool
	// agentBlocks counts agent blocks seen so far, at every depth, so an
	// agent block without an agentId gets a positional key that stays
	// stable while the transcript only grows at the end.
	agentBlocks int
	out         []codebuffSubagent
}

func newCodebuffSubagentSink(rootID string) *codebuffSubagentSink {
	return &codebuffSubagentSink{rootID: rootID, used: map[string]bool{}}
}

// allocate returns the child session ID for one agent block. IDs derive from
// the owning session's full ID plus the block's agentId, so they are stable
// across reparses. An agentId outside [A-Za-z0-9._-] is replaced by a short
// digest so the ID stays a safe single path component; an empty agentId uses
// the block's position among all agent blocks; a repeated key gets a -2, -3,
// ... suffix in document order.
func (s *codebuffSubagentSink) allocate(agentID string) string {
	s.agentBlocks++
	key := codebuffSubagentKey(agentID)
	if key == "" {
		key = "idx" + strconv.Itoa(s.agentBlocks)
	}
	candidate := key
	for n := 2; s.used[candidate]; n++ {
		candidate = key + "-" + strconv.Itoa(n)
	}
	s.used[candidate] = true
	return s.rootID + codebuffSubagentIDSep + candidate
}

// codebuffSubagentKey maps an upstream agentId to the key used in the child
// session ID; see codebuffSubagentSink.allocate.
func codebuffSubagentKey(agentID string) string {
	if agentID == "" {
		return ""
	}
	for i := range len(agentID) {
		c := agentID[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			continue
		}
		sum := sha256.Sum256([]byte(agentID))
		return "h" + hex.EncodeToString(sum[:8])
	}
	return agentID
}

// collect lifts one agent block into a child session and returns its ID, or
// "" when the block carries nothing to show (no prompt and no child blocks
// that render), in which case no child session exists to link to. The
// child's messages are a user message from initialPrompt, then the child
// blocks through the same walker the main agent's AI messages use, so nested
// agents become further children linked to this one.
func (s *codebuffSubagentSink) collect(
	b *codebuffWireBlock, ts time.Time, parentID string,
) string {
	id := s.allocate(b.AgentID)
	idx := len(s.out)
	s.out = append(s.out, codebuffSubagent{
		ID:        id,
		ParentID:  parentID,
		AgentID:   b.AgentID,
		AgentType: b.AgentType,
		AgentName: b.AgentName,
		Timestamp: ts,
	})
	var msgs []ParsedMessage
	if prompt := strings.TrimSpace(b.InitialPrompt); prompt != "" {
		msgs = append(msgs, ParsedMessage{
			Role:          RoleUser,
			Content:       prompt,
			Timestamp:     ts,
			ContentLength: len(prompt),
		})
	}
	msgs = append(msgs, codebuffBlockMessages(b.Blocks, ts, id, s)...)
	if len(msgs) == 0 {
		// Nothing rendered, so no nested agent block was seen either and
		// nothing was appended after idx.
		s.out = s.out[:idx]
		return ""
	}
	for i := range msgs {
		msgs[i].Ordinal = i
	}
	s.out[idx].Messages = msgs
	return id
}

// decodeCodebuffMessages streams chat-messages.json element by element with
// the encoding/json/v2 jsontext decoder, so the megabytes of ignored
// metadata.runState in each AI message are skipped instead of buffered. The
// parsed output is byte-identical to the previous gjson implementation
// (guarded by the reference-decoder equivalence test); any difference in
// ParsedMessage content would be a stored-data change requiring a
// dataVersion bump, which plan 021 must not do.
//
// sessionID is the full ID of the session being decoded; nested agent blocks
// become child sessions whose IDs derive from it (codebuffSubagentSink).
//
// Truncation: an unexpected-EOF or syntax error after at least one element
// decoded yields the messages so far with Truncated=true -- a transcript
// interrupted mid-write shows what survived instead of disappearing from the
// archive. Zero elements decoded keeps today's hard error: there is nothing
// to show.
//
// A reader never observes a file mid-write. The CLI saves chat-messages.json
// with writeFileAtomic / writeFileAtomicAsync (cli/src/utils/
// write-file-atomic.ts): it writes a unique temp sibling, then renames it
// over the target, so every read sees either the previous complete file or
// the next one. A damaged file on disk is static damage left by a crash
// under an older non-atomic writer, never a live write in progress, so no
// later snapshot can turn a complete stored transcript into a prefix. Keeping
// the surviving prefix for such a file, syntax errors included, is
// deliberate. Do not add retry, snapshot-completeness, or keep-the-old-copy
// handling for this case, and do not accept review findings that assume
// torn reads.
//
// Cancellation is not truncation, and the distinction is load-bearing: the
// Codebuff source set sets ForceReplace=true (codebuff_provider.go), so
// anything this function returns replaces the stored transcript. A cancelled
// decode that returned its partial buffer would commit half a conversation as
// if it were the whole one, marked only by a truncation flag a reader may
// read as "the agent stopped", with no error to notice because the parse
// reports success. So a ctx error returns an empty transcript and the error
// -- never the partial buffer, never Truncated=true. Note honestly: the
// production caller passes context.Background() (WithFileParse has no ctx
// and widening it is out of scope for plan 021), so the check is currently
// reachable only from tests. That is a known gap, not a guarantee; the rule
// is what keeps the gap safe when someone threads a real context later.
func decodeCodebuffMessages(
	ctx context.Context, r io.Reader, sessionDate time.Time, sessionID string,
) (codebuffTranscript, error) {
	dec := jsontext.NewDecoder(r)
	subs := newCodebuffSubagentSink(sessionID)

	tok, err := dec.ReadToken()
	if err != nil {
		return codebuffTranscript{}, fmt.Errorf("decode chat-messages: %w", err)
	}
	if tok.Kind() != jsontext.KindBeginArray {
		return codebuffTranscript{}, errors.New(
			"decode chat-messages: root is not an array",
		)
	}

	var (
		t           codebuffTranscript
		startedAt   time.Time
		endedAt     time.Time
		ordinal     int
		currentDate = sessionDate
		prevHour    = -1
		// decodedElements counts message objects that survived the wire
		// decode, whether or not they rendered a ParsedMessage. Truncation
		// detection must not key on len(t.Messages): a valid AI element
		// with credits but no displayable blocks renders nothing, so a
		// transcript of such elements would look empty and a later cut
		// would become a hard parse error that discards the session.
		decodedElements int
	)
	// Seed the rollover state from the session creation time-of-day so the
	// first time-only message can roll past midnight (see
	// parseCodebuffMessages for the worked example).
	if !sessionDate.IsZero() {
		prevHour = sessionDate.Hour()
	}

	for dec.PeekKind() != jsontext.KindEndArray {
		if err := ctx.Err(); err != nil {
			// Cancellation is not truncation: return no results, per the
			// ForceReplace note above.
			return codebuffTranscript{}, err
		}
		var raw jsontext.Value
		if err := json.UnmarshalDecode(dec, &raw); err != nil {
			if decodedElements > 0 && decodeIsTruncation(err) {
				t.Truncated = true
				t.StartedAt, t.EndedAt = startedAt, endedAt
				t.Subagents = subs.out
				return t, nil
			}
			return codebuffTranscript{}, fmt.Errorf("decode chat-messages: %w", err)
		}
		// Retention accounting: the element's raw bytes are transiently
		// live while it is folded into the accumulator -- that is the
		// decoder's true peak, one element at a time, independent of how
		// many elements the transcript carries. The decoded subset is far
		// smaller because the struct skips the undeclared run-state
		// members; charging only that would under-report the transient.
		charge := conservativeDecodedRetainedBytes(int64(len(raw)))
		observeStreamingRetainedBytes(ctx, charge)
		released := false
		release := func() {
			if !released {
				observeStreamingRetainedBytes(ctx, -charge)
				released = true
			}
		}

		var m codebuffWireMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			// A valid-JSON element that is not a message object (a bare
			// number or string) -- the gjson implementation silently skipped
			// such elements: Get("variant") returned empty and no case
			// matched. Preserve that tolerance so stored output cannot
			// change.
			release()
			continue
		}
		decodedElements++

		var ts time.Time
		ts, currentDate, prevHour = codebuffFoldTimestamp(
			m.Timestamp, currentDate, prevHour,
		)
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}
		appendCodebuffWireMessage(&t, &m, ts, &ordinal, sessionID, subs)
		release()
	}
	if _, err := dec.ReadToken(); err != nil {
		// Consumes the closing bracket. An EOF or syntax error here means
		// the file was cut off after at least one complete element.
		if decodedElements > 0 && decodeIsTruncation(err) {
			t.Truncated = true
			t.StartedAt, t.EndedAt = startedAt, endedAt
			t.Subagents = subs.out
			return t, nil
		}
		return codebuffTranscript{}, fmt.Errorf("decode chat-messages: %w", err)
	}
	// The whole-file validation this replaces (gjson.ValidBytes) rejected
	// anything after the closing bracket; keep that hard error.
	if _, err := dec.ReadToken(); err != nil {
		if !errors.Is(err, io.EOF) {
			return codebuffTranscript{}, fmt.Errorf(
				"decode chat-messages: trailing data after array: %w", err)
		}
	} else {
		return codebuffTranscript{}, errors.New(
			"decode chat-messages: trailing data after array")
	}

	t.StartedAt, t.EndedAt = startedAt, endedAt
	t.Subagents = subs.out
	return t, nil
}

// decodeIsTruncation reports whether a decode error means the file was cut
// off mid-write: an unexpected EOF from a partial element, a clean EOF from
// a missing closing bracket, or a syntax error from malformed bytes.
func decodeIsTruncation(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) ||
		isSyntaxDecodeErr(err)
}

// isSyntaxDecodeErr reports whether err is a JSON syntax error from the
// jsontext decoder.
func isSyntaxDecodeErr(err error) bool {
	_, ok := errors.AsType[*jsontext.SyntacticError](err)
	return ok
}

// codebuffFoldTimestamp applies the midnight-rollover state machine to one
// raw timestamp string, mirroring parseCodebuffMessages' loop body exactly:
// time-only timestamps roll the date forward when the hour wraps past
// midnight; absolute timestamps anchor the current date to their own local
// calendar date and reset the rollover tracker.
func codebuffFoldTimestamp(
	raw string, currentDate time.Time, prevHour int,
) (ts time.Time, nextDate time.Time, nextHour int) {
	cur := currentDate
	nextHour = prevHour
	ts = parseCodebuffTimestamp(raw, cur)

	rawTS := strings.TrimSpace(raw)
	isTimeOnly := !strings.Contains(rawTS, "T") &&
		!strings.Contains(rawTS, "-") &&
		strings.Contains(rawTS, ":")
	if !ts.IsZero() && prevHour >= 0 && isTimeOnly {
		if ts.Hour() < prevHour {
			cur = cur.AddDate(0, 0, 1)
			// Re-parse with the advanced date.
			ts = parseCodebuffTimestamp(raw, cur)
		}
	}
	if !ts.IsZero() {
		if isTimeOnly {
			nextHour = ts.Hour()
		} else {
			nextHour = -1
			tsLocal := ts.In(cur.Location())
			tsDate := time.Date(
				tsLocal.Year(), tsLocal.Month(), tsLocal.Day(),
				0, 0, 0, 0, cur.Location(),
			)
			if !tsDate.Equal(cur) {
				cur = tsDate
			}
		}
	}
	return ts, cur, nextHour
} // codebuffTextPreviewMaxBytes caps a pasted-text attachment's stored

// preview, including the ellipsis marker appended at the call site when
// truncation happens (its bytes are reserved from the limit).
const codebuffTextPreviewMaxBytes = 200

// codebuffAskUserLabelMaxBytes caps an ask-user answer's question label when
// the question has no header.
const codebuffAskUserLabelMaxBytes = 80

// codebuffEnvelopeAttachmentLines renders the attachment envelope (image,
// pasted-text, and file attachments) as the marker lines the parser stores,
// in plan-022 order: images, then text, then files. Local paths and full
// pasted content are never rendered -- the wire struct does not declare
// them, so they cannot leak here.
func codebuffEnvelopeAttachmentLines(m *codebuffWireMessage) []string {
	var lines []string
	for _, a := range m.Attachments {
		if a.Filename != "" {
			lines = append(lines, "[Image: "+a.Filename+"]")
		} else {
			lines = append(lines, "[Image attached]")
		}
	}
	for _, a := range m.TextAttachments {
		lines = append(lines, fmt.Sprintf(
			"[Text attachment: %d chars]", a.CharCount))
		if a.Preview != "" {
			preview := a.Preview
			if len(preview) > codebuffTextPreviewMaxBytes {
				// The ellipsis marker's bytes are reserved from the
				// limit, per the repo truncation convention, so the
				// stored line stays within the cap.
				preview = stringutil.SafeTruncate(
					preview, codebuffTextPreviewMaxBytes-3) + "…"
			}
			lines = append(lines, preview)
		}
	}
	for _, a := range m.FileAttachments {
		line := "[File: " + a.Filename + "]"
		if a.IsDirectory {
			line += " (directory)"
		}
		if a.Note != "" {
			line += " " + a.Note
		}
		lines = append(lines, line)
	}
	return lines
}

// codebuffEmitSystem appends one system message, the shape the parser uses
// for non-conversation events ([Mode: ...], variant error, runtime errors).
func codebuffEmitSystem(
	t *codebuffTranscript, content string, ts time.Time, ordinal *int,
) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	t.Messages = append(t.Messages, ParsedMessage{
		Ordinal:       *ordinal,
		Role:          RoleSystem,
		Content:       content,
		Timestamp:     ts,
		ContentLength: len(content),
		IsSystem:      true,
	})
	*ordinal++
}

// appendCodebuffWireMessage converts one decoded wire message into zero or
// more ParsedMessages plus its plan-020 turn fact. Variant handling: user
// renders as a user message, ai through the block walker, error as a system
// message, and every other variant -- including agent and anything upstream
// adds later -- as an assistant message, matching upstream's roleHeading
// default, so a variant whose only payload is content is never dropped.
// Envelope handling (plan 022): attachment markers append to the
// content-bearing paths' Content; userError and validationErrors are runtime
// notices, not conversation content, so they emit their own system messages
// for any variant. An ai message skips the attachment markers: its transcript
// payload is the block stream, and upstream stamps attachments on prompts.
func appendCodebuffWireMessage(
	t *codebuffTranscript, m *codebuffWireMessage, ts time.Time, ordinal *int,
	sessionID string, subs *codebuffSubagentSink,
) {
	switch m.Variant {
	case "user":
		content := strings.TrimSpace(m.Content)
		// User messages can also carry blocks (e.g. images). Collect
		// image references from blocks to append to content, one per
		// line, matching the gjson implementation's output byte for byte.
		var imageRefs []string
		for _, b := range m.Blocks {
			if b.Type == "image" {
				if b.Filename != "" {
					imageRefs = append(imageRefs, "[Image: "+b.Filename+"]")
				} else {
					imageRefs = append(imageRefs, "[Image attached]")
				}
			}
		}
		// Attachment envelope markers come after the image-block markers,
		// in plan-022 order: images, pasted text, files. An envelope-only
		// user message (attachments but no text) still counts as a user
		// message: upstream counts the message, not the text, and the
		// markers make the content non-empty here.
		envelope := codebuffEnvelopeAttachmentLines(m)
		if len(envelope) > 0 {
			all := append(append([]string{}, imageRefs...), envelope...)
			content = strings.TrimSpace(
				content + "\n" + strings.Join(all, "\n"),
			)
		} else if len(imageRefs) > 0 {
			content = strings.TrimSpace(
				content + "\n" + strings.Join(imageRefs, "\n"),
			)
		}
		if content != "" {
			t.Messages = append(t.Messages, ParsedMessage{
				Ordinal:       *ordinal,
				Role:          RoleUser,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			*ordinal++
		}

	case "ai":
		firstOrdinal := *ordinal
		parsed := codebuffBlockMessages(m.Blocks, ts, sessionID, subs)
		if len(parsed) > 0 {
			for i := range parsed {
				parsed[i].Ordinal = *ordinal
				*ordinal++
			}
			t.Messages = append(t.Messages, parsed...)
		}
		// An AI message with no displayable content still counts as a
		// turn boundary for billing: its credits field describes spend
		// even when nothing rendered. Record the fact either way. A
		// present-but-unusable credits value (null, a string) still marks
		// presence, matching gjson's Exists() semantics this replaces.
		if len(m.Credits) > 0 {
			credits, creditsRaw := codebuffWireCredits(m.Credits)
			t.TurnFacts = append(t.TurnFacts, codebuffTurnFact{
				MessageID:      m.ID,
				Ordinal:        firstOrdinal,
				Timestamp:      ts,
				CreditsPresent: true,
				Credits:        credits,
				CreditsRaw:     creditsRaw,
				RunState:       runStateResultFor(m),
			})
		}

	case "error":
		// Error messages from the upstream CLI (API failures, rate
		// limits, country blocks). Emit as a system message so the error
		// is visible in the transcript. The attachment envelope rides on
		// every variant upstream, so an error message that carries one
		// keeps its markers too.
		content := strings.TrimSpace(m.Content)
		envelope := codebuffEnvelopeAttachmentLines(m)
		if len(envelope) > 0 {
			content = strings.TrimSpace(
				content + "\n" + strings.Join(envelope, "\n"),
			)
		}
		codebuffEmitSystem(t, content, ts, ordinal)

	default:
		// agent and any future variant: content-bearing, rendered as an
		// assistant message (upstream's roleHeading maps everything
		// unlisted to "## Assistant"). Empty content and no blocks still
		// produce nothing, as before.
		content := strings.TrimSpace(m.Content)
		envelope := codebuffEnvelopeAttachmentLines(m)
		if len(envelope) > 0 {
			content = strings.TrimSpace(
				content + "\n" + strings.Join(envelope, "\n"),
			)
		}
		if content != "" {
			t.Messages = append(t.Messages, ParsedMessage{
				Ordinal:       *ordinal,
				Role:          RoleAssistant,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			*ordinal++
		}
	}

	// Runtime notices, any variant: userError is the error banner the app
	// displayed; validationErrors are per-entry validation failures. Both
	// are system messages so they never read as conversation content, and
	// they emit even when the message itself rendered nothing, because a
	// notice with no surrounding context is still a record of what the
	// user saw.
	if m.UserError != "" {
		codebuffEmitSystem(t, m.UserError, ts, ordinal)
	}
	for _, v := range m.ValidationErrors {
		codebuffEmitSystem(t, "[Validation error] "+v.Message, ts, ordinal)
	}
}

// codebuffWireCredits mirrors the gjson semantics this decoder replaces for
// the credits member: a JSON number keeps its literal text; a JSON string is
// unwrapped and parsed as a number (zero on failure, like gjson's Float());
// an explicit null counts as present with zero. The returned raw text is
// what plan 020's codebuffTurnCost parses, so a string value flows through
// as an unparsable raw and emits no row -- while still marking presence.
func codebuffWireCredits(raw jsontext.Value) (float64, string) {
	text := strings.TrimSpace(string(raw))
	if len(text) > 1 && text[0] == '"' && text[len(text)-1] == '"' {
		if inner, err := strconv.Unquote(text); err == nil {
			f, _ := strconv.ParseFloat(inner, 64)
			return f, text
		}
		return 0, text
	}
	f, _ := strconv.ParseFloat(text, 64)
	return f, text
}

// runStateResultFor re-derives the plan-020 gjson view of one message's
// metadata.runState from the decoded wire struct, so codebuffTurnModel's
// resolution order is unchanged. Only the retained members can appear in the
// re-encoded bytes; everything else was skipped at decode time.
func runStateResultFor(m *codebuffWireMessage) gjson.Result {
	if m.Metadata == nil || m.Metadata.RunState == nil {
		return gjson.Parse("")
	}
	raw, err := json.Marshal(m.Metadata.RunState)
	if err != nil {
		return gjson.Parse("")
	}
	return gjson.Parse(string(raw))
}

// codebuffAskUserAnswerLines renders the user's answers below the questions,
// resolved by questionIndex. The question's header labels the answer when
// present, else its truncated question text; an out-of-range index renders
// without a label rather than panicking. A skipped block with no answers
// renders [Skipped].
func codebuffAskUserAnswerLines(b *codebuffWireBlock) []string {
	if b.Skipped && len(b.Answers) == 0 {
		return []string{"[Skipped]"}
	}
	var lines []string
	for _, a := range b.Answers {
		var choice string
		switch {
		case a.SelectedOption != "":
			choice = a.SelectedOption
		case len(a.SelectedOptions) > 0:
			choice = strings.Join(a.SelectedOptions, ", ")
		case a.OtherText != "":
			choice = a.OtherText
		}
		if choice == "" {
			continue
		}
		label := ""
		if a.QuestionIndex >= 0 && a.QuestionIndex < len(b.Questions) {
			q := b.Questions[a.QuestionIndex]
			if q.Header != "" {
				label = q.Header
			} else {
				label = stringutil.SafeTruncate(
					q.Question, codebuffAskUserLabelMaxBytes)
			}
		}
		if label != "" {
			lines = append(lines, "[Answer: "+label+"] "+choice)
		} else {
			lines = append(lines, "[Answer] "+choice)
		}
	}
	return lines
}

// codebuffEmitSystemBlock appends one system message to a block walk's
// output, the immediate-emission shape mode-divider and plan use. Notices
// (sponsored-proposal, agent-list) are records, not agent speech, so they
// never classify as the transcript's final assistant turn.
func codebuffEmitSystemBlock(out *[]ParsedMessage, content string, ts time.Time) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	*out = append(*out, ParsedMessage{
		Role:          RoleSystem,
		Content:       content,
		Timestamp:     ts,
		ContentLength: len(content),
		IsSystem:      true,
	})
}

// codebuffBlockMessages converts one block stream into ParsedMessages,
// preserving text grouping, the [Thinking] wrapper, tool-run batching, and
// per-block emission order. The main agent's AI messages and every subagent's
// nested blocks share it, so a subagent's work is stored the same way as the
// main agent's. sessionID is the session that owns the blocks; agent blocks
// become child sessions of it through subs.
func codebuffBlockMessages(
	blocks []codebuffWireBlock, ts time.Time, sessionID string,
	subs *codebuffSubagentSink,
) []ParsedMessage {
	if len(blocks) == 0 {
		return nil
	}

	// textEntry tracks a text block with its type to preserve interleaving.
	type textEntry struct {
		content  string
		isReason bool
	}
	var (
		out         []ParsedMessage
		textBuf     []textEntry
		toolCalls   []ParsedToolCall
		toolResults []ParsedToolResult
	)

	// flushText emits accumulated text entries in order, grouping
	// consecutive entries of the same type.
	flushText := func() {
		if len(textBuf) == 0 {
			return
		}
		var thinkingParts, regularParts []string
		for _, entry := range textBuf {
			if entry.isReason {
				// Flush regular text before starting a thinking block.
				if len(regularParts) > 0 {
					text := strings.Join(regularParts, "\n\n")
					out = append(out, ParsedMessage{
						Role:          RoleAssistant,
						Content:       text,
						Timestamp:     ts,
						ContentLength: len(text),
					})
					regularParts = nil
				}
				thinkingParts = append(thinkingParts, entry.content)
			} else {
				// Flush thinking before starting regular text.
				if len(thinkingParts) > 0 {
					thinkingText := strings.Join(thinkingParts, "\n\n")
					out = append(out, ParsedMessage{
						Role:          RoleAssistant,
						Content:       "[Thinking]\n" + thinkingText + "\n[/Thinking]",
						ThinkingText:  thinkingText,
						HasThinking:   true,
						Timestamp:     ts,
						ContentLength: len(thinkingText),
					})
					thinkingParts = nil
				}
				regularParts = append(regularParts, entry.content)
			}
		}
		// Flush any remaining.
		if len(thinkingParts) > 0 {
			thinkingText := strings.Join(thinkingParts, "\n\n")
			out = append(out, ParsedMessage{
				Role:          RoleAssistant,
				Content:       "[Thinking]\n" + thinkingText + "\n[/Thinking]",
				ThinkingText:  thinkingText,
				HasThinking:   true,
				Timestamp:     ts,
				ContentLength: len(thinkingText),
			})
		}
		if len(regularParts) > 0 {
			text := strings.Join(regularParts, "\n\n")
			out = append(out, ParsedMessage{
				Role:          RoleAssistant,
				Content:       text,
				Timestamp:     ts,
				ContentLength: len(text),
			})
		}
		textBuf = nil
	}

	// flushTools emits accumulated tool calls as a single assistant
	// message, then emits each tool result as a user message.
	flushTools := func() {
		if len(toolCalls) > 0 {
			out = append(out, ParsedMessage{
				Role:       RoleAssistant,
				Timestamp:  ts,
				HasToolUse: true,
				ToolCalls:  toolCalls,
			})
			toolCalls = nil
		}
		for _, tr := range toolResults {
			out = append(out, ParsedMessage{
				Role:          RoleUser,
				Timestamp:     ts,
				ToolResults:   []ParsedToolResult{tr},
				ContentLength: tr.ContentLength,
			})
		}
		toolResults = nil
	}

	// Track whether we're currently accumulating tool calls to batch
	// consecutive tool blocks together.
	inToolRun := false

	for _, block := range blocks {
		blockType := block.Type
		isTool := blockType == "tool" || blockType == "agent"

		// Flush on transition away from a tool run.
		if inToolRun && !isTool {
			flushText()
			flushTools()
			inToolRun = false
		}

		switch blockType {
		case "text":
			// Flush accumulated tools before text to preserve ordering.
			if len(toolCalls) > 0 {
				flushTools()
			}
			if strings.TrimSpace(block.Content) == "" {
				continue
			}
			textBuf = append(textBuf, textEntry{
				content:  block.Content,
				isReason: block.TextType == "reasoning",
			})

		case "tool":
			if !inToolRun {
				flushText()
				inToolRun = true
			}
			tc := parseCodebuffWireToolCall(&block)
			if tc != nil {
				toolCalls = append(toolCalls, *tc)
				if len(block.Output) > 0 {
					toolResults = append(toolResults, ParsedToolResult{
						ToolUseID:     tc.ToolUseID,
						ContentRaw:    string(block.Output),
						ContentLength: len(block.Output),
					})
				}
			}

		case "agent":
			if !inToolRun {
				flushText()
				inToolRun = true
			}

			inputParts := map[string]any{
				"agentType": block.AgentType,
				"agentName": block.AgentName,
			}
			if len(block.Params) > 0 && string(block.Params) != "null" {
				var v any
				if err := json.Unmarshal(block.Params, &v); err == nil {
					inputParts["params"] = v
				}
			}
			if block.InitialPrompt != "" {
				inputParts["prompt"] = block.InitialPrompt
			}
			// The agent's lifecycle status (spawned, complete, ...) used
			// to be rendered in the assistant text for the block; now
			// that agent output is emitted as a linked ParsedToolResult,
			// carry the status in the tool-call input so it stays visible
			// in the parsed session.
			status := block.Status
			if status == "" {
				status = "spawned"
			}
			inputParts["status"] = status

			inputJSON, _ := json.Marshal(inputParts, json.Deterministic(true))

			tc := ParsedToolCall{
				ToolUseID: block.AgentID,
				ToolName:  block.AgentType,
				Category:  "Task",
				InputJSON: string(inputJSON),
				// The nested blocks become a linked child session; its tool
				// calls carry their own categories, so result blocking
				// applies to them per call like any other stored result.
				SubagentSessionID: subs.collect(&block, ts, sessionID),
			}
			toolCalls = append(toolCalls, tc)

			// Emit the subagent's final answer as ONE linked
			// ParsedToolResult rather than an ordinary assistant text
			// message, so the configured result-content blocking strips it
			// when the Task category is blocked. One result per call
			// matches the archive: db.ToolCall carries a single
			// ResultContent and the pairing loop keeps the LAST result per
			// tool_use_id.
			if block.Content != "" {
				// The upstream content member is a JSON string, so encode
				// it as a JSON string value and size it by the decoded
				// length, matching how tool blocks store output.Raw and
				// how convertToolResultsContext consumes ContentRaw.
				quoted, err := json.Marshal(block.Content)
				if err == nil {
					toolResults = append(toolResults, ParsedToolResult{
						ToolUseID:     block.AgentID,
						ContentRaw:    string(quoted),
						ContentLength: len(block.Content),
					})
				}
			}

		case "mode-divider":
			flushText()
			flushTools()
			if block.Mode != "" {
				// Emit system blocks immediately, not deferred.
				out = append(out, ParsedMessage{
					Role:          RoleSystem,
					Content:       "[Mode: " + block.Mode + "]",
					Timestamp:     ts,
					ContentLength: len("[Mode: " + block.Mode + "]"),
					IsSystem:      true,
				})
			}

		case "plan":
			flushText()
			flushTools()
			if strings.TrimSpace(block.Content) != "" {
				// Emit system blocks immediately, not deferred.
				out = append(out, ParsedMessage{
					Role:          RoleSystem,
					Content:       "[Plan]\n" + block.Content,
					Timestamp:     ts,
					ContentLength: len("[Plan]\n" + block.Content),
					IsSystem:      true,
				})
			}

		case "ask-user":
			flushText()
			flushTools()
			var parts []string
			for _, q := range block.Questions {
				if strings.TrimSpace(q.Question) != "" {
					parts = append(parts, "[Agent asked] "+q.Question)
				}
			}
			parts = append(parts, codebuffAskUserAnswerLines(&block)...)
			if len(parts) > 0 {
				content := strings.Join(parts, "\n")
				out = append(out, ParsedMessage{
					Role:          RoleSystem,
					Content:       content,
					Timestamp:     ts,
					ContentLength: len(content),
					IsSystem:      true,
				})
			}

		case "sponsored-proposal":
			flushText()
			flushTools()
			// target plus consent headline only; the proposal payload and
			// consent body/folder/branch/runId are structurally excluded.
			content := "[Sponsored proposal] " + block.Target
			if block.Consent != nil && block.Consent.Headline != "" {
				content += "\n" + block.Consent.Headline
			}
			codebuffEmitSystemBlock(&out, content, ts)

		case "agent-list":
			flushText()
			flushTools()
			// Display names with id fallback, in the order given; agentsDir
			// is structurally excluded.
			names := make([]string, 0, len(block.Agents))
			for _, a := range block.Agents {
				if a.DisplayName != "" {
					names = append(names, a.DisplayName)
				} else {
					names = append(names, a.ID)
				}
			}
			if len(names) > 0 {
				codebuffEmitSystemBlock(&out, "[Agents: "+strings.Join(names, ", ")+"]", ts)
			}

		case "image":
			if block.Filename != "" {
				textBuf = append(textBuf, textEntry{
					content:  "[Image: " + block.Filename + "]",
					isReason: false,
				})
			} else {
				textBuf = append(textBuf, textEntry{
					content:  "[Image attached]",
					isReason: false,
				})
			}
		}
	}

	// Flush any remaining accumulated content.
	flushText()
	flushTools()

	if len(out) == 0 {
		return nil
	}
	return out
}

// parseCodebuffWireToolCall mirrors parseCodebuffToolCall on a wire block.
func parseCodebuffWireToolCall(b *codebuffWireBlock) *ParsedToolCall {
	if b.ToolName == "" {
		return nil
	}
	inputJSON := ""
	if len(b.Input) > 0 && string(b.Input) != "null" {
		inputJSON = string(b.Input)
	}
	return &ParsedToolCall{
		ToolUseID: b.ToolCallID,
		ToolName:  b.ToolName,
		Category:  NormalizeToolCategory(b.ToolName),
		InputJSON: inputJSON,
	}
}
