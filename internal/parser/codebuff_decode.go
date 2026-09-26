package parser

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
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
	ID        string `json:"id"`
	Variant   string `json:"variant"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	ParentID  string `json:"parentId"`
	// Credits stays a raw value, not a float: presence (including an
	// explicit null or a non-numeric string) is what suppresses the legacy
	// run-state fallback, and the raw text feeds plan 020's decimal-safe
	// cost parsing exactly as gjson's .Raw did.
	Credits  jsontext.Value      `json:"credits"`
	Metadata *codebuffWireMeta   `json:"metadata"`
	Blocks   []codebuffWireBlock `json:"blocks"`

	// Fields plans 022-023 build on: declared now so the decode path is
	// stable, consumed by later plans. Attachment member shapes are
	// refined when plan 022 lands.
	UserError       *bool                    `json:"userError"`
	IsComplete      *bool                    `json:"isComplete"`
	CompletionTime  string                   `json:"completionTime"`
	IsCompletion    *bool                    `json:"isCompletion"`
	Attachments     []codebuffWireAttachment `json:"attachments"`
	TextAttachments []codebuffWireAttachment `json:"textAttachments"`
	FileAttachments []codebuffWireAttachment `json:"fileAttachments"`
}

type codebuffWireAttachment struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
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
}

type codebuffWireQuestion struct {
	Question string `json:"question"`
}

// codebuffTranscript carries everything parseCodebuffSession needs from the
// transcript: parsed messages, per-turn billing facts (plan 020), the
// timestamp envelope, and whether the file ended mid-write.
type codebuffTranscript struct {
	Messages  []ParsedMessage
	TurnFacts []codebuffTurnFact
	StartedAt time.Time
	EndedAt   time.Time
	Truncated bool
}

// decodeCodebuffMessages streams chat-messages.json element by element with
// the encoding/json/v2 jsontext decoder, so the megabytes of ignored
// metadata.runState in each AI message are skipped instead of buffered. The
// parsed output is byte-identical to the previous gjson implementation
// (guarded by the reference-decoder equivalence test); any difference in
// ParsedMessage content would be a stored-data change requiring a
// dataVersion bump, which plan 021 must not do.
//
// Truncation: an unexpected-EOF or syntax error after at least one element
// decoded yields the messages so far with Truncated=true -- a transcript
// interrupted mid-write shows what survived instead of disappearing from the
// archive. Zero elements decoded keeps today's hard error: there is nothing
// to show.
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
	ctx context.Context, r io.Reader, sessionDate time.Time,
) (codebuffTranscript, error) {
	dec := jsontext.NewDecoder(r)

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
			if len(t.Messages) > 0 && decodeIsTruncation(err) {
				t.Truncated = true
				t.StartedAt, t.EndedAt = startedAt, endedAt
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
		appendCodebuffWireMessage(&t, &m, ts, &ordinal)
		release()
	}
	if _, err := dec.ReadToken(); err != nil {
		// Consumes the closing bracket. An EOF or syntax error here means
		// the file was cut off after at least one complete element.
		if len(t.Messages) > 0 && decodeIsTruncation(err) {
			t.Truncated = true
			t.StartedAt, t.EndedAt = startedAt, endedAt
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
}

// appendCodebuffWireMessage converts one decoded wire message into zero or
// more ParsedMessages plus its plan-020 turn fact, preserving
// parseCodebuffMessages' variant handling and ordinal assignment.
func appendCodebuffWireMessage(
	t *codebuffTranscript, m *codebuffWireMessage, ts time.Time, ordinal *int,
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
		if len(imageRefs) > 0 {
			content = strings.TrimSpace(
				content + "\n" + strings.Join(imageRefs, "\n"),
			)
		}
		if content == "" {
			return
		}
		t.Messages = append(t.Messages, ParsedMessage{
			Ordinal:       *ordinal,
			Role:          RoleUser,
			Content:       content,
			Timestamp:     ts,
			ContentLength: len(content),
		})
		*ordinal++

	case "ai":
		firstOrdinal := *ordinal
		parsed := codebuffParsedAIMessages(m, ts)
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
		// is visible in the transcript.
		content := strings.TrimSpace(m.Content)
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

// codebuffParsedAIMessages re-shapes parseCodebuffAIMessage's block loop onto
// the decoded wire structs, preserving text grouping, the [Thinking] wrapper,
// tool-run batching, and per-block emission order.
func codebuffParsedAIMessages(m *codebuffWireMessage, ts time.Time) []ParsedMessage {
	if len(m.Blocks) == 0 {
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

	for _, block := range m.Blocks {
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
			}
			toolCalls = append(toolCalls, tc)

			// Emit agent output as a linked ParsedToolResult rather
			// than an ordinary assistant text message. Representing
			// the output as a tool result lets the configured result-
			// content blocking system (BlockedResultCategories) strip
			// it when the Task category is blocked. The upstream
			// content member is a JSON string, so re-encode it to keep
			// the stored raw form identical to gjson's .Raw (quoted).
			if block.Content != "" {
				quoted, err := json.Marshal(block.Content)
				if err == nil {
					toolResults = append(toolResults, ParsedToolResult{
						ToolUseID:     block.AgentID,
						ContentRaw:    string(quoted),
						ContentLength: len(quoted),
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
