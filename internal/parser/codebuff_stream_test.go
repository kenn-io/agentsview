package parser

import (
	"bytes"
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
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/stringutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// This file holds the reference gjson decoder for chat-messages.json, kept
// from the pre-plan-021 implementation. It is the cheapest guard for the
// next person who changes block handling: any divergence between the
// streaming decoder and this reference shows up as an equivalence-test
// failure on the same fixture. Delete it only when a replacement guard
// exists (plan 021 maintenance note).

// refCodebuffTurnFact is the reference decoder's turn-fact carrier; identical
// in shape to the production codebuffTurnFact.
type refCodebuffTurnFact = codebuffTurnFact

// refParseCodebuffMessages is the pre-plan-021 gjson implementation,
// preserved verbatim as the streaming decoder's reference. Any change to
// block handling here must be mirrored in codebuff_decode.go and vice versa.
func refParseCodebuffMessages(
	data []byte, sessionDate time.Time, sessionID string,
) ([]ParsedMessage, []codebuffTurnFact, time.Time, time.Time, []codebuffSubagent, error) {
	root := gjson.ParseBytes(data)
	if !root.IsArray() {
		return nil, nil, time.Time{}, time.Time{}, nil,
			errors.New("chat-messages.json root is not an array")
	}
	subs := &refCodebuffSubagentSink{rootID: sessionID}

	var (
		messages  []ParsedMessage
		turnFacts []codebuffTurnFact
		startedAt time.Time
		endedAt   time.Time
		ordinal   int
		// Track the current date for cross-midnight sessions. Start with
		// the session directory date and advance when time-of-day wraps
		// past midnight.
		currentDate = sessionDate
		prevHour    = -1
	)
	// Seed the rollover state from the session creation time-of-day so
	// the first time-only message can roll past midnight. A session
	// created late in the local evening (directory
	// 2026-07-17T06-58-00.000Z = 23:58 July 16 in UTC-7) whose first
	// message reads "12:01 AM" belongs to the next local calendar day;
	// without the seed, prevHour stays -1 until the second message and
	// the first message would be stamped ~24h before the session
	// started, skewing StartedAt.
	if !sessionDate.IsZero() {
		prevHour = sessionDate.Hour()
	}

	root.ForEach(func(_, msg gjson.Result) bool {
		variant := msg.Get("variant").Str
		ts := parseCodebuffTimestamp(
			msg.Get("timestamp").Str, currentDate,
		)

		// Detect midnight rollover for time-only timestamps only.
		// RFC3339 timestamps retain their timezone, so their hours
		// should not be compared with local time-only timestamps.
		rawTS := strings.TrimSpace(msg.Get("timestamp").Str)
		isTimeOnly := !strings.Contains(rawTS, "T") &&
			!strings.Contains(rawTS, "-") &&
			strings.Contains(rawTS, ":")
		if !ts.IsZero() && prevHour >= 0 && isTimeOnly {
			if ts.Hour() < prevHour {
				currentDate = currentDate.AddDate(0, 0, 1)
				// Re-parse with the advanced date.
				ts = parseCodebuffTimestamp(
					msg.Get("timestamp").Str, currentDate,
				)
			}
		}
		// Only track prevHour for time-only timestamps to avoid
		// incorrect rollover when formats are mixed. Reset prevHour
		// when a non-time-only timestamp is encountered, and also
		// anchor currentDate to the absolute timestamp's local
		// calendar date so subsequent time-only messages don't get
		// assigned to the previous session directory date across
		// midnight.
		if !ts.IsZero() {
			if isTimeOnly {
				prevHour = ts.Hour()
			} else {
				prevHour = -1
				tsLocal := ts.In(currentDate.Location())
				tsDate := time.Date(
					tsLocal.Year(), tsLocal.Month(), tsLocal.Day(),
					0, 0, 0, 0, currentDate.Location(),
				)
				if !tsDate.Equal(currentDate) {
					currentDate = tsDate
				}
			}
		}

		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		switch variant {
		case "user":
			content := strings.TrimSpace(msg.Get("content").Str)
			// User messages can also carry blocks (e.g. images).
			// Collect image references from blocks to append to content.
			if blocks := msg.Get("blocks"); blocks.IsArray() {
				blocks.ForEach(func(_, block gjson.Result) bool {
					if block.Get("type").Str == "image" {
						filename := block.Get("filename").Str
						if filename != "" {
							content += "\n[Image: " + filename + "]"
						} else {
							content += "\n[Image attached]"
						}
					}
					return true
				})
				content = strings.TrimSpace(content)
			}
			if content == "" {
				return true
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			ordinal++

		case "ai":
			firstOrdinal := ordinal
			parsed := refParseCodebuffAIMessage(msg.Get("blocks"), ts, sessionID, subs)
			if len(parsed) == 0 {
				// An AI message with no displayable content still counts
				// as a turn boundary for billing: its credits field
				// describes spend even when nothing rendered. Record the
				// fact before skipping.
				if credits, present := refCodebuffMessageCredits(msg); present {
					turnFacts = append(turnFacts, refCodebuffTurnFact{
						MessageID:      msg.Get("id").Str,
						Ordinal:        firstOrdinal,
						Timestamp:      ts,
						CreditsPresent: true,
						Credits:        credits,
						CreditsRaw:     msg.Get("credits").Raw,
						RunState:       msg.Get("metadata.runState"),
					})
				}
				return true
			}
			for i := range parsed {
				parsed[i].Ordinal = ordinal
				ordinal++
			}
			messages = append(messages, parsed...)
			if credits, present := refCodebuffMessageCredits(msg); present {
				turnFacts = append(turnFacts, refCodebuffTurnFact{
					MessageID:      msg.Get("id").Str,
					Ordinal:        firstOrdinal,
					Timestamp:      ts,
					CreditsPresent: true,
					Credits:        credits,
					CreditsRaw:     msg.Get("credits").Raw,
					RunState:       msg.Get("metadata.runState"),
				})
			}

		case "error":
			// Error messages from the upstream CLI (API failures, rate
			// limits, country blocks). Emit as a system message so the
			// error is visible in the transcript.
			content := strings.TrimSpace(msg.Get("content").Str)
			if content == "" {
				return true
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleSystem,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
				IsSystem:      true,
			})
			ordinal++
		}

		return true
	})

	return messages, turnFacts, startedAt, endedAt, subs.out, nil
}

// refCodebuffSubagentSink mirrors codebuffSubagentSink over gjson values:
// the same ID allocation (agentId key, digest for unsafe ids) and the same
// document-order collection. Keep it in lockstep with the production sink.
type refCodebuffSubagentSink struct {
	rootID string
	out    []codebuffSubagent
}

func (s *refCodebuffSubagentSink) allocate(agentID string) string {
	key := agentID
	for _, r := range agentID {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			sum := sha256.Sum256([]byte(agentID))
			key = "h" + hex.EncodeToString(sum[:8])
			break
		}
	}
	return s.rootID + "__subagent__" + key
}

func (s *refCodebuffSubagentSink) collect(
	block gjson.Result, ts time.Time, parentID string,
) string {
	id := s.allocate(block.Get("agentId").Str)
	idx := len(s.out)
	s.out = append(s.out, codebuffSubagent{
		ID:        id,
		ParentID:  parentID,
		AgentID:   block.Get("agentId").Str,
		AgentType: block.Get("agentType").Str,
		AgentName: block.Get("agentName").Str,
		Timestamp: ts,
	})
	var msgs []ParsedMessage
	if prompt := strings.TrimSpace(block.Get("initialPrompt").Str); prompt != "" {
		msgs = append(msgs, ParsedMessage{
			Role: RoleUser, Content: prompt, Timestamp: ts,
			ContentLength: len(prompt),
		})
	}
	msgs = append(msgs, refParseCodebuffAIMessage(block.Get("blocks"), ts, id, s)...)
	if len(msgs) == 0 {
		s.out = s.out[:idx]
		return ""
	}
	for i := range msgs {
		msgs[i].Ordinal = i
	}
	s.out[idx].Messages = msgs
	return id
}

// refCodebuffMessageCredits is the reference implementation of the credits
// extraction the streaming decoder must match.
func refCodebuffMessageCredits(msg gjson.Result) (float64, bool) {
	v := msg.Get("credits")
	if !v.Exists() {
		return 0, false
	}
	return v.Float(), true
}

// refParseCodebuffAIMessage is the pre-plan-021 gjson block walker,
// preserved as the streaming decoder's reference.
func refParseCodebuffAIMessage(
	blocks gjson.Result,
	ts time.Time,
	sessionID string,
	subs *refCodebuffSubagentSink,
) []ParsedMessage {
	if !blocks.IsArray() {
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
		// Group consecutive entries of the same type.
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

	// flushTools emits accumulated tool calls as a single assistant message,
	// then emits each tool result as a user message.
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

	blocks.ForEach(func(_, block gjson.Result) bool {
		blockType := block.Get("type").Str
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
			textType := block.Get("textType").Str
			content := block.Get("content").Str
			if strings.TrimSpace(content) == "" {
				return true
			}
			isReason := textType == "reasoning"
			textBuf = append(textBuf, textEntry{content: content, isReason: isReason})

		case "tool":
			if !inToolRun {
				flushText()
				inToolRun = true
			}
			tc := refParseCodebuffToolCall(block)
			if tc != nil {
				toolCalls = append(toolCalls, *tc)
				if output := block.Get("output"); output.Exists() {
					toolResults = append(toolResults, ParsedToolResult{
						ToolUseID:     tc.ToolUseID,
						ContentRaw:    output.Raw,
						ContentLength: len(output.Raw),
					})
				}
			}

		case "agent":
			if !inToolRun {
				flushText()
				inToolRun = true
			}

			agentType := block.Get("agentType").Str
			agentName := block.Get("agentName").Str
			agentID := block.Get("agentId").Str
			agentStatus := block.Get("status").Str

			inputParts := map[string]any{
				"agentType": agentType,
				"agentName": agentName,
			}
			if params := block.Get("params"); params.Exists() &&
				params.Raw != "null" {
				inputParts["params"] = params.Value()
			}
			if prompt := block.Get("initialPrompt"); prompt.Exists() &&
				prompt.Str != "" {
				inputParts["prompt"] = prompt.Str
			}
			// The agent's lifecycle status (spawned, complete, ...) used to
			// be rendered in the assistant text for the block; now that
			// agent output is emitted as a linked ParsedToolResult, carry
			// the status in the tool-call input so it stays visible in the
			// parsed session.
			status := agentStatus
			if status == "" {
				status = "spawned"
			}
			inputParts["status"] = status

			inputJSON, _ := json.Marshal(inputParts, json.Deterministic(true))

			tc := ParsedToolCall{
				ToolUseID:         agentID,
				ToolName:          agentType,
				Category:          "Task",
				InputJSON:         string(inputJSON),
				SubagentSessionID: subs.collect(block, ts, sessionID),
			}
			toolCalls = append(toolCalls, tc)

			// The final answer is the Task call's single result; the nested
			// blocks live in the linked child session.
			if content := block.Get("content").Str; content != "" {
				quoted, _ := json.Marshal(content)
				toolResults = append(toolResults, ParsedToolResult{
					ToolUseID:     agentID,
					ContentRaw:    string(quoted),
					ContentLength: len(content),
				})
			}

		case "mode-divider":
			flushText()
			flushTools()
			mode := block.Get("mode").Str
			if mode != "" {
				// Emit system blocks immediately, not deferred.
				out = append(out, ParsedMessage{
					Role:          RoleSystem,
					Content:       "[Mode: " + mode + "]",
					Timestamp:     ts,
					ContentLength: len("[Mode: " + mode + "]"),
					IsSystem:      true,
				})
			}

		case "plan":
			flushText()
			flushTools()
			content := block.Get("content").Str
			if strings.TrimSpace(content) != "" {
				// Emit system blocks immediately, not deferred.
				out = append(out, ParsedMessage{
					Role:          RoleSystem,
					Content:       "[Plan]\n" + content,
					Timestamp:     ts,
					ContentLength: len("[Plan]\n" + content),
					IsSystem:      true,
				})
			}

		case "ask-user":
			flushText()
			flushTools()
			var parts []string
			block.Get("questions").ForEach(func(_, q gjson.Result) bool {
				questionText := q.Get("question").Str
				if strings.TrimSpace(questionText) != "" {
					parts = append(parts, "[Agent asked] "+questionText)
				}
				return true
			})
			parts = append(parts, refCodebuffAskUserAnswerLines(block)...)
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
			if block.Get("filename").Str != "" {
				textBuf = append(textBuf, textEntry{
					content:  "[Image: " + block.Get("filename").Str + "]",
					isReason: false,
				})
			} else {
				textBuf = append(textBuf, textEntry{
					content:  "[Image attached]",
					isReason: false,
				})
			}
		}
		return true
	})

	// Flush any remaining accumulated content.
	flushText()
	flushTools()

	if len(out) == 0 {
		return nil
	}
	return out
}

// refParseCodebuffToolCall is the reference tool-call extraction.
func refParseCodebuffToolCall(block gjson.Result) *ParsedToolCall {
	toolName := block.Get("toolName").Str
	if toolName == "" {
		return nil
	}
	toolCallID := block.Get("toolCallId").Str
	input := block.Get("input")

	inputJSON := ""
	if input.Exists() && input.Raw != "" && input.Raw != "null" {
		inputJSON = input.Raw
	}

	return &ParsedToolCall{
		ToolUseID: toolCallID,
		ToolName:  toolName,
		Category:  NormalizeToolCategory(toolName),
		InputJSON: inputJSON,
	}
}

func refCodebuffAskUserAnswerLines(b gjson.Result) []string {
	var lines []string
	b.Get("answers").ForEach(func(_, a gjson.Result) bool {
		var choice string
		switch {
		case a.Get("selectedOption").Str != "":
			choice = a.Get("selectedOption").Str
		case len(a.Get("selectedOptions").Array()) > 0:
			var opts []string
			a.Get("selectedOptions").ForEach(func(_, o gjson.Result) bool {
				opts = append(opts, o.Str)
				return true
			})
			choice = strings.Join(opts, ", ")
		case a.Get("otherText").Str != "":
			choice = a.Get("otherText").Str
		}
		if choice == "" {
			return true
		}
		label := ""
		qIndex := int(a.Get("questionIndex").Int())
		questions := b.Get("questions").Array()
		if qIndex >= 0 && qIndex < len(questions) {
			q := questions[qIndex]
			if q.Get("header").Str != "" {
				label = q.Get("header").Str
			} else {
				label = stringutil.SafeTruncate(
					q.Get("question").Str, codebuffAskUserLabelMaxBytes)
			}
		}
		if label != "" {
			lines = append(lines, "[Answer: "+label+"] "+choice)
		} else {
			lines = append(lines, "[Answer] "+choice)
		}
		return true
	})
	if b.Get("skipped").Bool() && len(lines) == 0 {
		return []string{"[Skipped]"}
	}
	return lines
}

// codebuffGoldenTranscript builds a transcript exercising every block type
// the decoder models: text (regular and reasoning), tool with and without
// output, agent with params/prompt/status/content, mode-divider, plan,
// ask-user, image with and without filename, plus user/error variants and an
// unknown block type that must be skipped silently. Every AI message carries
// credits and a run state so the usage rows and model resolution are part of
// the golden assertions.
func codebuffGoldenTranscript() string {
	return `[
		{"id":"u1","variant":"user","content":"  do the thing  ","timestamp":"10:00 AM"},
		{"id":"ai-1","variant":"ai","timestamp":"10:01 AM","credits":2,"content":"",
		 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek","inference":{"source":"codebuff"}}}}}},
		{"id":"ai-2","variant":"ai","timestamp":"10:02 AM","credits":"10.5",
		 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek","inference":{"source":"byok","model":"claude-sonnet-4"}}}}},
		 "blocks":[
			{"type":"text","textType":"reasoning","content":"hmm"},
			{"type":"text","textType":"text","content":"step one"},
			{"type":"text","textType":"reasoning","content":"wait"},
			{"type":"text","textType":"text","content":"step two"},
			{"type":"mode-divider","mode":"plan"},
			{"type":"plan","content":"the plan"},
			{"type":"unknown-future-type","content":"skip me"},
			{"type":"tool","toolName":"run_terminal_command","toolCallId":"tc-1","input":{"command":"ls"},"output":"file1\nfile2"},
			{"type":"tool","toolName":"read_file","toolCallId":"tc-2","input":{"path":"a.go"}},
			{"type":"agent","agentType":"code-review","agentName":"Reviewer","agentId":"ag-1","status":"complete","params":{"model":"m1"},"initialPrompt":"review it","content":"agent said hi",
		 "blocks":[{"type":"text","textType":"reasoning","content":"nest think"},{"type":"text","textType":"text","content":"nest work"},
		  {"type":"tool","toolName":"read_file","toolCallId":"nc-1","input":{"path":"n.go"},"output":"nested out"},
		  {"type":"ask-user","questions":[{"question":"nested q?","header":"Scope"}],"answers":[{"questionIndex":0,"selectedOption":"yes"}],"skipped":false},
		  {"type":"agent","agentId":"ag-1-1","agentName":"inner","agentType":"basher","status":"complete","content":"inner says hi",
		   "blocks":[{"type":"text","textType":"text","content":"innermost"}]}]},
			{"type":"image","filename":"shot.png"},
			{"type":"image"},
			{"type":"ask-user","questions":[{"question":"continue?"},{"question":""},{"question":"sure?"}]},
			{"type":"text","textType":"text","content":"done"}
		 ]},
		{"id":"err-1","variant":"error","content":"rate limited","timestamp":"10:03 AM"},
		{"id":"u2","variant":"user","content":"with image","timestamp":"10:04 AM",
		 "blocks":[{"type":"image","filename":"pic.jpg"}]}
	]`
}

// requireEqualMessages compares the streaming and reference decoders' message
// slices element by element so a mismatch names the first differing field
// rather than dumping two structs.
func requireEqualMessages(
	t *testing.T, label string, got, want []ParsedMessage,
) {
	t.Helper()
	require.Len(t, got, len(want), label)
	for i := range want {
		assert.Equal(t, want[i].Ordinal, got[i].Ordinal, label+" ordinal "+strconv.Itoa(i))
		assert.Equal(t, want[i].Role, got[i].Role, label+" role "+strconv.Itoa(i))
		assert.Equal(t, want[i].Content, got[i].Content, label+" content "+strconv.Itoa(i))
		assert.Equal(t, want[i].Timestamp, got[i].Timestamp, label+" ts "+strconv.Itoa(i))
		assert.Equal(t, want[i].ContentLength, got[i].ContentLength, label+" len "+strconv.Itoa(i))
		assert.Equal(t, want[i].IsSystem, got[i].IsSystem, label+" system "+strconv.Itoa(i))
		assert.Equal(t, want[i].HasThinking, got[i].HasThinking, label+" thinking-flag "+strconv.Itoa(i))
		assert.Equal(t, want[i].ThinkingText, got[i].ThinkingText, label+" thinking-text "+strconv.Itoa(i))
		assert.Equal(t, want[i].HasToolUse, got[i].HasToolUse, label+" tool-flag "+strconv.Itoa(i))
		require.Len(t, got[i].ToolCalls, len(want[i].ToolCalls), label+" toolcalls "+strconv.Itoa(i))
		for j := range want[i].ToolCalls {
			assert.Equal(t, want[i].ToolCalls[j], got[i].ToolCalls[j],
				label+" toolcall "+strconv.Itoa(i)+":"+strconv.Itoa(j))
		}
		require.Len(t, got[i].ToolResults, len(want[i].ToolResults), label+" toolresults "+strconv.Itoa(i))
		for j := range want[i].ToolResults {
			assert.Equal(t, want[i].ToolResults[j], got[i].ToolResults[j],
				label+" toolresult "+strconv.Itoa(i)+":"+strconv.Itoa(j))
		}
	}
}

// requireEqualSubagents compares the two decoders' child sessions: identity,
// linkage, and the full message content of each.
func requireEqualSubagents(
	t *testing.T, label string, got, want []codebuffSubagent,
) {
	t.Helper()
	require.Len(t, got, len(want), label)
	for i := range want {
		l := label + " subagent " + strconv.Itoa(i)
		assert.Equal(t, want[i].ID, got[i].ID, l+" id")
		assert.Equal(t, want[i].ParentID, got[i].ParentID, l+" parent")
		assert.Equal(t, want[i].AgentID, got[i].AgentID, l+" agent id")
		assert.Equal(t, want[i].AgentType, got[i].AgentType, l+" agent type")
		assert.Equal(t, want[i].AgentName, got[i].AgentName, l+" agent name")
		assert.Equal(t, want[i].Timestamp, got[i].Timestamp, l+" ts")
		requireEqualMessages(t, l, got[i].Messages, want[i].Messages)
	}
}

// requireEqualTurnFacts compares turn facts including the gjson run-state
// model resolution outcome, which is the only part of the run state the
// parser keeps.
func requireEqualTurnFacts(
	t *testing.T, label string, got, want []codebuffTurnFact,
) {
	t.Helper()
	require.Len(t, got, len(want), label)
	for i := range want {
		assert.Equal(t, want[i].MessageID, got[i].MessageID, label+" id "+strconv.Itoa(i))
		assert.Equal(t, want[i].Ordinal, got[i].Ordinal, label+" ordinal "+strconv.Itoa(i))
		assert.Equal(t, want[i].Timestamp, got[i].Timestamp, label+" ts "+strconv.Itoa(i))
		assert.Equal(t, want[i].CreditsPresent, got[i].CreditsPresent, label+" present "+strconv.Itoa(i))
		assert.InDelta(t, want[i].Credits, got[i].Credits, 1e-9, label+" credits "+strconv.Itoa(i))
		assert.Equal(t, want[i].CreditsRaw, got[i].CreditsRaw, label+" raw "+strconv.Itoa(i))
		assert.Equal(t, want[i].RunState.Get("sessionState.mainAgentState.inference.model").Str,
			got[i].RunState.Get("sessionState.mainAgentState.inference.model").Str,
			label+" inference-model "+strconv.Itoa(i))
		assert.Equal(t, want[i].RunState.Get("sessionState.mainAgentState.agentType").Str,
			got[i].RunState.Get("sessionState.mainAgentState.agentType").Str,
			label+" agent-type "+strconv.Itoa(i))
	}
}

// TestDecodeCodebuffMessagesGolden pins the full streaming output on a
// fixture exercising every modeled block type. The per-block expectations
// mirror TestParseCodebuffSession_ThinkingBlocks, ..._ModeDivider,
// ..._PlanBlock, ..._AskUserBlock, ..._ImageBlock, ..._ToolCalls, and
// ..._SubagentToolCall, so a reviewer can diff against today's behavior.
func TestDecodeCodebuffMessagesGolden(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	const sessionID = "codebuff:p:s"
	refMsgs, refFacts, refStart, refEnd, refSubs, err := refParseCodebuffMessages(
		[]byte(codebuffGoldenTranscript()), sessionDate, sessionID,
	)
	require.NoError(t, err)
	require.NotEmpty(t, refMsgs)

	streamed, err := decodeCodebuffMessages(
		t.Context(),
		strings.NewReader(codebuffGoldenTranscript()),
		sessionDate, sessionID)
	require.NoError(t, err)
	assert.False(t, streamed.Truncated)
	assert.Equal(t, refStart, streamed.StartedAt)
	assert.Equal(t, refEnd, streamed.EndedAt)
	requireEqualMessages(t, "golden", streamed.Messages, refMsgs)
	requireEqualTurnFacts(t, "golden", streamed.TurnFacts, refFacts)
	requireEqualSubagents(t, "golden", streamed.Subagents, refSubs)

	// Golden specifics the reference comparison cannot name. The fixture
	// alternates reasoning and regular text, so each thinking entry flushes
	// separately (grouping only merges CONSECUTIVE same-type entries -- the
	// grouping behavior is pinned by TestParseCodebuffSession_ThinkingBlocks
	// and the equivalence test; here we pin the [Thinking] wrapper text and
	// the image/blank-line joining).
	var assistantTexts []string
	for _, m := range streamed.Messages {
		if m.Role == RoleAssistant {
			assistantTexts = append(assistantTexts, m.Content)
		}
	}
	assert.Contains(t, assistantTexts, "[Thinking]\nhmm\n[/Thinking]",
		"the [Thinking] wrapper format must not change")
	assert.Contains(t, assistantTexts, "[Thinking]\nwait\n[/Thinking]")
	assert.Contains(t, assistantTexts, "step one")
	assert.Contains(t, assistantTexts, "step two")
	assert.Contains(t, assistantTexts, "[Image: shot.png]\n\n[Image attached]",
		"image blocks join the text stream and group like text")
	assert.Contains(t, assistantTexts, "done")

	// Ordinal sequence across a transcript mixing text, tools, and
	// results: every emitted message gets its own sequential ordinal.
	for i, m := range streamed.Messages {
		assert.Equal(t, i, m.Ordinal, "ordinals must be sequential")
	}

	// Locate the batched tool-call message by its flag rather than a
	// fragile positional index.
	var toolMsg *ParsedMessage
	for i := range streamed.Messages {
		if streamed.Messages[i].HasToolUse {
			toolMsg = &streamed.Messages[i]
			break
		}
	}
	require.NotNil(t, toolMsg, "one batched tool-call message expected")
	require.True(t, toolMsg.HasToolUse)
	require.Len(t, toolMsg.ToolCalls, 3)
	assert.Equal(t, "run_terminal_command", toolMsg.ToolCalls[0].ToolName)
	assert.Equal(t, "tc-1", toolMsg.ToolCalls[0].ToolUseID)
	assert.Equal(t, `{"command":"ls"}`, toolMsg.ToolCalls[0].InputJSON)
	assert.Equal(t, "Task", toolMsg.ToolCalls[2].Category)
	assert.JSONEq(t,
		`{"agentType":"code-review","agentName":"Reviewer","params":{"model":"m1"},"prompt":"review it","status":"complete"}`,
		toolMsg.ToolCalls[2].InputJSON)
	// Results follow as user messages, each linked to its call: the tool
	// with output, and the agent block's content. read_file had no output
	// member, so it contributes no result.
	var results []ParsedToolResult
	for _, m := range streamed.Messages {
		if m.Role == RoleUser && len(m.ToolResults) > 0 {
			results = append(results, m.ToolResults...)
		}
	}
	require.Len(t, results, 2)
	assert.Equal(t, "tc-1", results[0].ToolUseID)
	assert.Equal(t, `"file1\nfile2"`, results[0].ContentRaw,
		"tool output raw stays the JSON-encoded value gjson stored")
	assert.Equal(t, "ag-1", results[1].ToolUseID)
	assert.Equal(t, `"agent said hi"`, results[1].ContentRaw,
		"the Task result is the subagent's final answer only")
	assert.Equal(t, len("agent said hi"), results[1].ContentLength)

	// The nested blocks are a linked child session, parsed like the main
	// agent's blocks, and the nested agent inside it is a grandchild.
	childID := sessionID + "__subagent__ag-1"
	grandchildID := sessionID + "__subagent__ag-1-1"
	assert.Equal(t, childID, toolMsg.ToolCalls[2].SubagentSessionID)
	require.Len(t, streamed.Subagents, 2)
	child, grandchild := streamed.Subagents[0], streamed.Subagents[1]
	assert.Equal(t, childID, child.ID)
	assert.Equal(t, sessionID, child.ParentID)
	assert.Equal(t, grandchildID, grandchild.ID)
	assert.Equal(t, childID, grandchild.ParentID)

	var childTexts []string
	var childCalls []ParsedToolCall
	var childResults []ParsedToolResult
	for _, m := range child.Messages {
		childTexts = append(childTexts, m.Content)
		childCalls = append(childCalls, m.ToolCalls...)
		childResults = append(childResults, m.ToolResults...)
	}
	require.NotEmpty(t, child.Messages)
	assert.Equal(t, RoleUser, child.Messages[0].Role)
	assert.Equal(t, "review it", child.Messages[0].Content,
		"the initial prompt opens the child session")
	assert.Contains(t, childTexts, "[Thinking]\nnest think\n[/Thinking]")
	assert.Contains(t, childTexts, "nest work")
	assert.Contains(t, childTexts, "[Agent asked] nested q?\n[Answer: Scope] yes")
	require.Len(t, childCalls, 2)
	assert.Equal(t, "read_file", childCalls[0].ToolName)
	assert.Equal(t, "Read", childCalls[0].Category)
	assert.Equal(t, "Task", childCalls[1].Category)
	assert.Equal(t, grandchildID, childCalls[1].SubagentSessionID)
	require.Len(t, childResults, 2)
	assert.Equal(t, "nc-1", childResults[0].ToolUseID)
	assert.Equal(t, `"nested out"`, childResults[0].ContentRaw)
	assert.Equal(t, `"inner says hi"`, childResults[1].ContentRaw)
	require.Len(t, grandchild.Messages, 1)
	assert.Equal(t, "innermost", grandchild.Messages[0].Content)

	// Usage rows: two billed turns under their resolved models.
	require.Len(t, streamed.TurnFacts, 2)
	assert.InDelta(t, 2.0, streamed.TurnFacts[0].Credits, 1e-9)
	assert.Equal(t, "2", streamed.TurnFacts[0].CreditsRaw)
	assert.InDelta(t, 10.5, streamed.TurnFacts[1].Credits, 1e-9)
	assert.Equal(t, `"10.5"`, streamed.TurnFacts[1].CreditsRaw,
		"a string credits value keeps presence and flows its raw text")
}

// TestDecodeCodebuffMessagesEquivalenceProperty parses a mixed-transcript
// variant through both decoders, including elements the golden fixture does
// not cover: a bare JSON element, a credits:null message, and a mid-file
// absolute timestamp anchoring the date.
func TestDecodeCodebuffMessagesEquivalenceProperty(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 23, 58, 0, 0, time.UTC)
	transcript := `[
		{"id":"u1","variant":"user","content":"late","timestamp":"11:59 PM"},
		{"id":"ai-1","variant":"ai","timestamp":"12:01 AM","credits":null,
		 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}}},
		42,
		"just a string",
		{"id":"ai-2","variant":"ai","timestamp":"2026-09-21T08:00:00Z","credits":1,
		 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}}},
		{"id":"u2","variant":"user","content":"morning","timestamp":"08:05 AM"}
	]`
	refMsgs, refFacts, refStart, refEnd, _, err := refParseCodebuffMessages(
		[]byte(transcript), sessionDate, "codebuff:p:s",
	)
	require.NoError(t, err)

	streamed, err := decodeCodebuffMessages(
		t.Context(), strings.NewReader(transcript), sessionDate, "codebuff:p:s")
	require.NoError(t, err)
	requireEqualMessages(t, "equiv", streamed.Messages, refMsgs)
	requireEqualTurnFacts(t, "equiv", streamed.TurnFacts, refFacts)
	assert.Equal(t, refStart, streamed.StartedAt)
	assert.Equal(t, refEnd, streamed.EndedAt)

	// The rollover seed: 11:59 PM then 12:01 AM rolls the date forward.
	// Both decoders must agree on the resolved timestamps. The 11:59 PM
	// user message stays on the 20th; the 12:01 AM AI message emits no
	// ParsedMessage (no blocks), so its rollover is observable through the
	// turn fact's timestamp.
	assert.Equal(t, refMsgs[0].Timestamp, streamed.Messages[0].Timestamp)
	assert.Equal(t, 20, streamed.Messages[0].Timestamp.Day())
	assert.Equal(t, 21, streamed.TurnFacts[0].Timestamp.Day(),
		"a 12:01 AM timestamp after an 11:59 PM one belongs to the next day")
}

// TestDecodeCodebuffMessagesTruncatedTail covers the mid-write recovery
// rule: at least one message decoded plus a broken tail yields the prefix
// with Truncated=true; a broken first element is a hard error; the caller
// wires the flag through IsTruncated and Classify.
func TestDecodeCodebuffMessagesTruncatedTail(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	prefix := `[
		{"id":"u1","variant":"user","content":"hello","timestamp":"10:00 AM"},
		{"id":"ai-1","variant":"ai","timestamp":"10:01 AM","credits":5,
		 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}}},
		{"id":"u2","variant":"user","content":"more","timestamp":"10:02 AM"}` + "\n"

	t.Run("tail cut off after real messages", func(t *testing.T) {
		streamed, err := decodeCodebuffMessages(
			t.Context(), strings.NewReader(prefix), sessionDate, "codebuff:p:s")
		require.NoError(t, err, "a truncated tail is not a parse failure")
		assert.True(t, streamed.Truncated)
		// The AI message carries no blocks, so it emits no ParsedMessage
		// (matching the reference); the two user messages survive.
		require.Len(t, streamed.Messages, 2)
		assert.Equal(t, "hello", streamed.Messages[0].Content)
		assert.Equal(t, "more", streamed.Messages[1].Content)
		require.Len(t, streamed.TurnFacts, 1)
	})

	t.Run("truncated session keeps TerminationTruncated end to end", func(t *testing.T) {
		runState := `{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}`
		dir := codebuffTestSession(t, prefix, runState, "")
		sess, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.Len(t, msgs, 2)
		assert.True(t, sess.IsTruncated)
		assert.Equal(t, TerminationTruncated, sess.TerminationStatus)
	})

	t.Run("zero messages then garbage errors", func(t *testing.T) {
		_, err := decodeCodebuffMessages(
			t.Context(),
			strings.NewReader(`[{"broken`), sessionDate, "codebuff:p:s")
		require.Error(t, err, "nothing survived: a hard error is correct")
	})

	t.Run("credits-only elements still make a cut tail truncation", func(t *testing.T) {
		// A valid AI element with credits but no displayable blocks
		// renders no ParsedMessage. Truncation detection must count the
		// decoded element, not the rendered messages, or a transcript of
		// such elements followed by a cut tail would hard-error and
		// discard the session.
		creditsOnly := `[
			{"id":"ai-1","variant":"ai","timestamp":"10:01 AM","credits":3,
			 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}}}` + "\n"
		streamed, err := decodeCodebuffMessages(
			t.Context(), strings.NewReader(creditsOnly), sessionDate, "codebuff:p:s")
		require.NoError(t, err, "a truncated tail after a credits-only element is not a parse failure")
		assert.True(t, streamed.Truncated)
		assert.Empty(t, streamed.Messages,
			"the credits-only element renders nothing")
		require.Len(t, streamed.TurnFacts, 1,
			"the turn fact survives: the credits are real spend")
	})

	t.Run("credits-only elements closed cleanly are not truncated", func(t *testing.T) {
		closed := `[
			{"id":"ai-1","variant":"ai","timestamp":"10:01 AM","credits":3,
			 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}}}` + "]"
		streamed, err := decodeCodebuffMessages(
			t.Context(), strings.NewReader(closed), sessionDate, "codebuff:p:s")
		require.NoError(t, err)
		assert.False(t, streamed.Truncated)
		assert.Empty(t, streamed.Messages)
		require.Len(t, streamed.TurnFacts, 1)
	})

	t.Run("empty array errors", func(t *testing.T) {
		_, err := decodeCodebuffMessages(
			t.Context(), strings.NewReader(`[]`), sessionDate, "codebuff:p:s")
		// Today's parser kept an empty array alive (no messages, no error,
		// meta counts fill the session in). The decoder must keep that
		// shape: no error, nothing truncated, nothing returned.
		require.NoError(t, err)
	})

	t.Run("session from empty array stays alive", func(t *testing.T) {
		runState := `{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}`
		dir := codebuffTestSession(t, `[]`, runState, "")
		sess, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		assert.Empty(t, msgs)
		assert.False(t, sess.IsTruncated)
	})
}

// TestDecodeCodebuffMessagesShapeErrors keeps today's behavior for a root
// that is not an array and for malformed JSON before the first element.
func TestDecodeCodebuffMessagesShapeErrors(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	t.Run("object root", func(t *testing.T) {
		_, err := decodeCodebuffMessages(
			t.Context(),
			strings.NewReader(`{"messages":[]}`), sessionDate, "codebuff:p:s")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "root is not an array")
	})

	t.Run("malformed before first element", func(t *testing.T) {
		_, err := decodeCodebuffMessages(
			t.Context(),
			strings.NewReader(`[not json`), sessionDate, "codebuff:p:s")
		require.Error(t, err)
	})

	t.Run("empty file", func(t *testing.T) {
		_, err := decodeCodebuffMessages(
			t.Context(), strings.NewReader(``), sessionDate, "codebuff:p:s")
		require.Error(t, err)
	})
}

// TestDecodeCodebuffMessagesCancellation pins the load-bearing rule:
// cancellation is not truncation. Both a pre-canceled context and a
// mid-array cancellation must return an error with an EMPTY transcript --
// never the partial buffer, because the Codebuff source set's ForceReplace
// would commit half a conversation as the whole one.
func TestDecodeCodebuffMessagesCancellation(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	mkMsg := func(i int) string {
		return fmt.Sprintf(
			`{"id":"ai-%d","variant":"ai","timestamp":"10:02 AM","credits":1,"content":"m%d"}`,
			i, i,
		)
	}
	transcript := "[" + mkMsg(1) + "," + mkMsg(2) + "," + mkMsg(3) + "]"

	t.Run("canceled before first element", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		out, err := decodeCodebuffMessages(ctx,
			strings.NewReader(transcript), sessionDate, "codebuff:p:s")
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, out.Messages, "no results on cancellation")
		assert.Empty(t, out.TurnFacts)
		assert.False(t, out.Truncated, "cancellation is not truncation")
	})

	t.Run("canceled mid-array after messages decoded", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		// The reader cancels the context after the Nth element so the loop
		// has already accumulated messages when the error fires.
		budget := 2
		r := &cancelingReader{
			ctx:    ctx,
			cancel: cancel,
			budget: &budget,
			source: strings.NewReader(transcript),
		}
		out, err := decodeCodebuffMessages(ctx, r, sessionDate, "codebuff:p:s")
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, out.Messages,
			"the partial buffer must never be returned: ForceReplace would commit it")
		assert.Empty(t, out.TurnFacts)
		assert.False(t, out.Truncated)
	})
}

// cancelingReader cancels its context after budget bytes have been read, so
// the decoder has already decoded elements when the context fires.
type cancelingReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	budget *int
	source io.Reader
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	if *r.budget <= 0 {
		r.cancel()
		return 0, context.Canceled
	}
	n, err := r.source.Read(p)
	*r.budget -= n
	if *r.budget <= 0 {
		r.cancel()
	}
	return n, err
}

// TestDecodeCodebuffMessagesRetainedBytes proves the memory bound this plan
// exists for: 200 AI messages each carrying a ~1 MB metadata.runState must
// keep peak observed retained bytes in the low megabytes -- a few times the
// decoded output, not the 200 MB a whole-file buffer would charge. It also
// verifies the retention accounting releases what it charges.
func TestDecodeCodebuffMessagesRetainedBytes(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	// bigRunState is ~1 MB of JSON the parser never reads.
	bigPayload := strings.Repeat("x", 1024*1024)
	bigRunState := fmt.Sprintf(
		`{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek","messageHistory":["%s"]}}}`,
		bigPayload,
	)
	var b strings.Builder
	b.WriteString("[")
	for i := range 200 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b,
			`{"id":"ai-%d","variant":"ai","timestamp":"10:02 AM","credits":1,"blocks":[{"type":"text","textType":"text","content":"m%d"}],"metadata":{"runState":%s}}`,
			i, i, bigRunState)
	}
	b.WriteString("]")
	transcript := b.String()

	var retained, peak int64
	ctx := WithStreamingRetainedBytesObserver(t.Context(), func(delta int64) {
		retained += delta
		peak = max(peak, retained)
	})
	out, err := decodeCodebuffMessages(ctx, strings.NewReader(transcript), sessionDate, "codebuff:p:s")
	require.NoError(t, err)
	assert.False(t, out.Truncated)
	require.Len(t, out.Messages, 200)
	assert.Zero(t, retained, "every charge must be released by the end")

	// The bound: the decoder's peak is ONE element's transient footprint
	// (~1 MB raw, conservatively charged), independent of the 200 elements
	// -- that independence is the point. A peak near 200+ MB would mean the
	// whole file was buffered; a peak growing with element count would mean
	// the skipped members leaked into retention. 16 MB is a generous
	// ceiling that still fails loudly in either case.
	assert.LessOrEqual(t, peak, int64(16*1024*1024),
		"peak retained bytes must stay at one element's transient footprint, not scale with the transcript")

	// Sanity: the observer must have seen real charges, or the accounting
	// itself is broken and the bound proves nothing.
	assert.Greater(t, peak, int64(1024),
		"the retention observer must have observed the decode's accounting")
}

// TestDecodeCodebuffMessagesSkipsRunStateBytes pins the mechanism behind the
// bound: an undeclared run-state member must not be materialized by the
// decoder. The fileTree member is oversized and the decoder must never hold
// it; the test asserts via a tiny retention charge, since jsontext skips
// bytes without buffering them.
func TestDecodeCodebuffMessagesSkipsRunStateBytes(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	oversized := strings.Repeat("y", 512*1024)
	transcript := fmt.Sprintf(
		`[{"id":"ai-1","variant":"ai","timestamp":"10:02 AM","credits":1,
		  "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}},
		  "fileContext":{"agentTemplates":{"base2-deepseek":{"model":"deepseek-v3"}},"fileTree":["%s"]}}}}]`,
		oversized,
	)
	var retained, peak int64
	ctx := WithStreamingRetainedBytesObserver(t.Context(), func(delta int64) {
		retained += delta
		peak = max(peak, retained)
	})
	out, err := decodeCodebuffMessages(ctx, strings.NewReader(transcript), sessionDate, "codebuff:p:s")
	require.NoError(t, err)
	require.Len(t, out.TurnFacts, 1)
	assert.Equal(t, "deepseek-v3",
		out.TurnFacts[0].RunState.Get("fileContext.agentTemplates.base2-deepseek.model").Str,
		"the template lookup must survive the streaming round-trip (run-state-root fileContext is one of codebuffTurnModel's query paths)")
	// The oversized fileTree member is skipped at decode time; the only
	// charge is the element's transient raw footprint (well under the 512 KB
	// member being doubled, let alone accumulated), fully released.
	assert.Zero(t, retained, "every charge must be released")
	assert.LessOrEqual(t, peak, int64(4*1024*1024),
		"one element's transient footprint, not the file's")
}

// TestDecodeCodebuffMessagesReaderErrorsAreHard keeps environment failures
// distinct from truncation: a reader that fails mid-stream surfaces its own
// error even though messages have already been decoded.
func TestDecodeCodebuffMessagesReaderErrorsAreHard(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	transcript := `[{"id":"u1","variant":"user","content":"hello","timestamp":"10:00 AM"}]`
	boom := errors.New("disk exploded")
	r := io.MultiReader(
		bytes.NewReader([]byte(transcript[:40])),
		&errReader{err: boom},
	)
	_, err := decodeCodebuffMessages(t.Context(), r, sessionDate, "codebuff:p:s")
	require.ErrorIs(t, err, boom, "an environment failure must not become truncation")
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// TestDecodeCodebuffMessagesDecodesTrailingToken ensures the closing bracket
// is consumed and trailing garbage after a complete array is treated like
// the gjson whole-file parse would: gjson.ValidBytes rejected it, so the
// parse failed hard.
func TestDecodeCodebuffMessagesDecodesTrailingToken(t *testing.T) {
	sessionDate := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	transcript := `[{"id":"u1","variant":"user","content":"hello","timestamp":"10:00 AM"}] oops`
	_, err := decodeCodebuffMessages(
		t.Context(), strings.NewReader(transcript), sessionDate, "codebuff:p:s")
	require.Error(t, err,
		"trailing garbage after a complete array must fail like the whole-file validation did")
}

// Ensure the jsontext import stays used if test bodies evolve.
var _ = jsontext.KindBeginArray
