package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/stringutil"
)

// Wire structs for chat-messages.json. Only fields the parser consumes are
// declared, so decoded messages stay small even though each AI message's
// metadata.runState embeds the full project context.
type codebuffWireMessage struct {
	ID      string `json:"id"`
	Variant string `json:"variant"`
	Content string `json:"content"`
	// Timestamp is a locale-formatted hour and minute; the date comes from
	// the session directory (see parseCodebuffTimestamp).
	Timestamp string `json:"timestamp"`
	// Credits stays raw: presence (even null or a string) suppresses the
	// legacy run-state fallback, and the raw text is parsed as a decimal.
	Credits  jsontext.Value      `json:"credits"`
	Metadata *codebuffWireMeta   `json:"metadata"`
	Blocks   []codebuffWireBlock `json:"blocks"`

	// UserError is a runtime notice the app displayed; empty means none.
	UserError        string                        `json:"userError"`
	ValidationErrors []codebuffWireValidationError `json:"validationErrors"`
	Attachments      []codebuffWireImageAttachment `json:"attachments"`
	TextAttachments  []codebuffWireTextAttachment  `json:"textAttachments"`
	FileAttachments  []codebuffWireFileAttachment  `json:"fileAttachments"`
}

type codebuffWireValidationError struct {
	Message string `json:"message"`
}

// codebuffWireImageAttachment is upstream ImageAttachment. The path member
// is deliberately undeclared: local filesystem paths are never stored.
type codebuffWireImageAttachment struct {
	Filename string `json:"filename"`
}

// codebuffWireTextAttachment is upstream TextAttachment. The full content
// member is deliberately undeclared; only the preview and charCount are kept.
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

// codebuffWireRunState is the subset of upstream RunState (sdk/src/run-state.ts)
// that model resolution reads. It decodes both a message's
// metadata.runState and the standalone run-state.json.
type codebuffWireRunState struct {
	Inference    *codebuffWireInference    `json:"inference"`
	SessionState *codebuffWireSessionState `json:"sessionState"`
}

type codebuffWireInference struct {
	Source string `json:"source"`
	Model  string `json:"model"`
}

type codebuffWireSessionState struct {
	MainAgentState *codebuffWireMainAgentState `json:"mainAgentState"`
	FileContext    *codebuffWireFileContext    `json:"fileContext"`
}

type codebuffWireMainAgentState struct {
	AgentType string `json:"agentType"`
}

type codebuffWireFileContext struct {
	AgentTemplates map[string]codebuffWireTemplate `json:"agentTemplates"`
}

type codebuffWireTemplate struct {
	Model string `json:"model"`
}

// codebuffWireBlock is a tagged union over `type`: one struct whose members
// cover the per-type shapes the parser reads. Params/Input/Output stay raw so
// their bytes are stored verbatim.
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
	AgentType string `json:"agentType"`
	AgentName string `json:"agentName"`
	// AgentID is required and unique within a transcript upstream
	// (cli/src/types/chat.ts).
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
	Headline string `json:"headline"`
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
// transcript: parsed messages, per-turn billing facts, the timestamp
// envelope, and the nested subagents lifted into their own sessions.
type codebuffTranscript struct {
	Messages  []ParsedMessage
	TurnFacts []codebuffTurnFact
	Subagents []codebuffSubagent
	StartedAt time.Time
	EndedAt   time.Time
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
	// AgentID is the raw upstream agentId.
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
	out    []codebuffSubagent
}

func newCodebuffSubagentSink(rootID string) *codebuffSubagentSink {
	return &codebuffSubagentSink{rootID: rootID}
}

// allocate returns the child session ID for one agent block: the owning
// session's full ID plus the block's agentId, so IDs are stable across
// reparses.
func (s *codebuffSubagentSink) allocate(agentID string) string {
	return s.rootID + codebuffSubagentIDSep + codebuffSubagentKey(agentID)
}

// codebuffSubagentKey maps an upstream agentId to the key used in the child
// session ID. Upstream does not document the server's agent-ID alphabet, so
// an agentId outside [A-Za-z0-9._-] is replaced by a short digest to keep
// the session ID a safe single path component.
func codebuffSubagentKey(agentID string) string {
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

// decodeCodebuffMessages streams chat-messages.json one array element at a
// time, so peak memory is one message rather than the whole file. Any
// malformed JSON is an error. sessionID is the full ID of the session being
// decoded; nested agent blocks become child sessions whose IDs derive from it.
func decodeCodebuffMessages(
	r io.Reader, sessionDate time.Time, sessionID string,
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
		ordinal     int
		currentDate = sessionDate
		prevHour    = -1
	)
	// Seed the rollover state from the session creation time-of-day so the
	// first time-only message can roll past midnight.
	if !sessionDate.IsZero() {
		prevHour = sessionDate.Hour()
	}

	for dec.PeekKind() != jsontext.KindEndArray {
		var raw jsontext.Value
		if err := json.UnmarshalDecode(dec, &raw); err != nil {
			return codebuffTranscript{}, fmt.Errorf("decode chat-messages: %w", err)
		}
		var m codebuffWireMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			// A valid element that is not a message object is skipped.
			continue
		}

		var ts time.Time
		ts, currentDate, prevHour = codebuffFoldTimestamp(
			m.Timestamp, currentDate, prevHour,
		)
		if !ts.IsZero() {
			if t.StartedAt.IsZero() || ts.Before(t.StartedAt) {
				t.StartedAt = ts
			}
			if ts.After(t.EndedAt) {
				t.EndedAt = ts
			}
		}
		appendCodebuffWireMessage(&t, &m, ts, &ordinal, sessionID, subs)
	}
	if _, err := dec.ReadToken(); err != nil {
		return codebuffTranscript{}, fmt.Errorf("decode chat-messages: %w", err)
	}
	if _, err := dec.ReadToken(); err == nil {
		return codebuffTranscript{}, errors.New(
			"decode chat-messages: trailing data after array")
	} else if !errors.Is(err, io.EOF) {
		return codebuffTranscript{}, fmt.Errorf(
			"decode chat-messages: trailing data after array: %w", err)
	}

	t.Subagents = subs.out
	return t, nil
}

// codebuffFoldTimestamp applies the midnight-rollover state machine to one
// raw timestamp string: time-only timestamps roll the date forward when the
// hour wraps past midnight; absolute timestamps anchor the current date to
// their own local calendar date and reset the rollover tracker.
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
}

// codebuffTextPreviewMaxBytes caps a pasted-text attachment's stored
// preview, including the ellipsis marker appended at the call site when
// truncation happens (its bytes are reserved from the limit).
const codebuffTextPreviewMaxBytes = 200

// codebuffAskUserLabelMaxBytes caps an ask-user answer's question label when
// the question has no header.
const codebuffAskUserLabelMaxBytes = 80

// codebuffEnvelopeAttachmentLines renders the attachment envelope as marker
// lines: images, then pasted text, then files. Local paths and full pasted
// content are not declared on the wire structs, so they are never stored.
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
// more ParsedMessages plus its turn fact. user renders as a user message, ai
// through the block walker, error as a system message, and every other
// variant (agent, future ones) as an assistant message, matching upstream's
// roleHeading default. userError and validationErrors become system messages
// for any variant.
func appendCodebuffWireMessage(
	t *codebuffTranscript, m *codebuffWireMessage, ts time.Time, ordinal *int,
	sessionID string, subs *codebuffSubagentSink,
) {
	switch m.Variant {
	case "user":
		content := strings.TrimSpace(m.Content)
		// User messages can also carry image blocks; append a marker line
		// for each.
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
		// Attachment markers follow the image-block markers, so an
		// attachment-only prompt still renders as a user message.
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
		// An AI message is a billing turn even when nothing rendered.
		if len(m.Credits) > 0 {
			t.TurnFacts = append(t.TurnFacts, codebuffTurnFact{
				MessageID:  m.ID,
				Ordinal:    firstOrdinal,
				Timestamp:  ts,
				CreditsRaw: strings.TrimSpace(string(m.Credits)),
				RunState:   codebuffMessageRunState(m),
			})
		}

	case "error":
		// API failures, rate limits, and similar errors from the CLI.
		content := strings.TrimSpace(m.Content)
		envelope := codebuffEnvelopeAttachmentLines(m)
		if len(envelope) > 0 {
			content = strings.TrimSpace(
				content + "\n" + strings.Join(envelope, "\n"),
			)
		}
		codebuffEmitSystem(t, content, ts, ordinal)

	default:
		// agent and any future variant render as assistant text.
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

	if m.UserError != "" {
		codebuffEmitSystem(t, m.UserError, ts, ordinal)
	}
	for _, v := range m.ValidationErrors {
		codebuffEmitSystem(t, "[Validation error] "+v.Message, ts, ordinal)
	}
}

// codebuffMessageRunState returns the message's metadata.runState, or nil.
func codebuffMessageRunState(m *codebuffWireMessage) *codebuffWireRunState {
	if m.Metadata == nil {
		return nil
	}
	return m.Metadata.RunState
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
				ToolUseID:         block.AgentID,
				ToolName:          block.AgentType,
				Category:          "Task",
				InputJSON:         string(inputJSON),
				SubagentSessionID: subs.collect(&block, ts, sessionID),
			}
			toolCalls = append(toolCalls, tc)

			// The subagent's final answer is the Task call's single result,
			// so result-content blocking for Task applies to it.
			if block.Content != "" {
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
