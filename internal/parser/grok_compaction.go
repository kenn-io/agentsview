package parser

import (
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/ctxio"
)

// grokCompactionSummaryPreamble opens the user_meta row that carries a
// compaction summary. Grok Build's format_compact_summary_content writes it
// verbatim; the other compaction_meta rows (the user-info prefix and the
// attached-image note) never start with it.
const grokCompactionSummaryPreamble = "This session is being continued from a previous conversation that ran out of context."

// grokMaxCheckpointFileSize bounds the compaction checkpoint read. A
// checkpoint holds one compacted context window, so a larger file is not a
// checkpoint this parser understands.
const grokMaxCheckpointFileSize = 64 << 20

// grokCheckpoint is one compaction_checkpoint record from updates.jsonl.
type grokCheckpoint struct {
	file      string
	timestamp time.Time
}

// grokCompactedSegment is the transcript Grok Build dropped from
// chat_history.jsonl at one compaction, rebuilt from updates.jsonl, and the
// checkpoint that ended it.
type grokCompactedSegment struct {
	messages   []ParsedMessage
	checkpoint grokCheckpoint
}

// grokCompactBoundary is the row a compaction leaves in the transcript, shaped
// like the Claude and Codex boundaries so compaction counts and dividers treat
// every provider alike.
func grokCompactBoundary(summary string, timestamp time.Time) ParsedMessage {
	return ParsedMessage{
		Role:              RoleAssistant,
		Content:           summary,
		ContentLength:     len(summary),
		Timestamp:         timestamp,
		IsSystem:          true,
		SourceType:        "system",
		SourceSubtype:     "compact_boundary",
		IsCompactBoundary: true,
	}
}

// grokCompactionSummary returns the summary text of a compaction_meta row,
// or false for the other compaction_meta rows.
func grokCompactionSummary(content gjson.Result) (string, bool) {
	text := strings.TrimSpace(grokTextParts(content))
	if !strings.HasPrefix(text, grokCompactionSummaryPreamble) {
		return "", false
	}
	return text, true
}

// grokTextParts joins the text parts of a Grok content value.
func grokTextParts(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.Str
	}
	var parts []string
	content.ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").Str == "text" {
			if text := part.Get("text").Str; text != "" {
				parts = append(parts, text)
			}
		}
		return true
	})
	return strings.Join(parts, "\n")
}

// grokCheckpointContent is what a checkpoint file's compacted_history
// holds that the transcript needs: the compaction summary and the tool
// results carried into the compacted context.
type grokCheckpointContent struct {
	summary string
	results []ParsedMessage
	// read reports that the file was present and held valid JSON. When it
	// is false the parse lacks this compaction's summary and carried results.
	read bool
}

// grokReadCheckpoint reads a checkpoint file's compacted_history. A missing,
// unreadable, or unrecognized file yields no content; the boundary still
// marks the compaction.
func grokReadCheckpoint(
	ctx context.Context, sessionDir, rel string,
) (grokCheckpointContent, error) {
	var out grokCheckpointContent
	rel = filepath.FromSlash(strings.TrimSpace(rel))
	if rel == "" || !filepath.IsLocal(rel) {
		return out, ctx.Err()
	}
	f, err := os.Open(filepath.Join(sessionDir, rel))
	if err != nil {
		return out, ctx.Err()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > grokMaxCheckpointFileSize {
		return out, ctx.Err()
	}
	data, err := io.ReadAll(ctxio.Reader{Context: ctx, Reader: f})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, ctxErr
	}
	if err != nil || !gjson.ValidBytes(data) {
		return out, nil
	}
	out.read = true
	gjson.GetBytes(data, "compacted_history").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").Str {
		case "tool_result":
			if msg, ok := grokToolResultMessage(item); ok {
				out.results = append(out.results, msg)
			}
		case "user":
			if out.summary != "" || item.Get("synthetic_reason").Str != "compaction_meta" {
				return true
			}
			if text, ok := grokCompactionSummary(item.Get("content")); ok {
				out.summary = text
			}
		}
		return true
	})
	return out, nil
}

// parseGrokCompactedSegments rebuilds the transcript before each compaction
// checkpoint. Grok Build truncates chat_history.jsonl at every checkpoint and
// keeps updates.jsonl, its source of truth, whole; without this pass a
// compacted session loses everything before its last compaction.
//
// One reducer reads the whole stream, as Grok Build's rebuild does, and each
// checkpoint closes the open items and cuts a segment. A turn can run across
// a checkpoint, so its turn_completed record, which names the turn's model,
// may come after the cut; rows past the last checkpoint are dropped, since
// chat_history.jsonl holds them.
func parseGrokCompactedSegments(
	ctx context.Context, path string, checkpoints []grokCheckpoint,
) ([]grokCompactedSegment, error) {
	if len(checkpoints) == 0 {
		return nil, nil
	}
	cuts := make([]int, 0, len(checkpoints))
	reducer := newGrokSegmentReducer(ctx)
	_, err := readJSONLFrom(ctx, path, 0, func(line string) {
		if !gjson.Valid(line) {
			return
		}
		root := gjson.Parse(line)
		update := grokUpdatePayload(root)
		if !update.Exists() {
			return
		}
		if update.Get("sessionUpdate").Str == "compaction_checkpoint" {
			if len(cuts) < len(checkpoints) {
				reducer.closeItems()
				cuts = append(cuts, len(reducer.messages))
			}
			return
		}
		reducer.process(update, hermesUnixTime(root.Get("timestamp").Float()))
	})
	if err != nil {
		return nil, err
	}
	segments := make([]grokCompactedSegment, len(cuts))
	from := 0
	for i, cut := range cuts {
		segments[i] = grokCompactedSegment{
			messages:   reducer.messages[from:cut:cut],
			checkpoint: checkpoints[i],
		}
		from = cut
	}
	return segments, nil
}

// grokUpdatePayload returns the session update a updates.jsonl record wraps.
func grokUpdatePayload(root gjson.Result) gjson.Result {
	if update := root.Get("params.update"); update.Exists() {
		return update
	}
	return root.Get("update")
}

// grokSegmentReducer turns an updates.jsonl stream into transcript rows with
// the turn rules of Grok Build's chat_rebuild reducer: a switch between user
// and agent output closes the open item, and a finished tool call closes the
// agent item before its result. Unlike that reducer it keeps the streamed
// thinking, which the dropped reasoning rows used to carry, and the record
// timestamps.
type grokSegmentReducer struct {
	ctx      context.Context
	messages []ParsedMessage

	inUserTurn       bool
	userParts        []string
	userTimestamp    time.Time
	userInterjection bool
	userSynthetic    bool

	agentText      strings.Builder
	thinking       strings.Builder
	agentTimestamp time.Time
	calls          []ParsedToolCall
	// turnStart is the first row of the turn still running. At its
	// turn_completed record the turn's model is stamped on its assistant
	// rows; agent output records carry no model. completedModel holds that
	// model for the agent item still open when the turn completed.
	turnStart      int
	completedModel string
	closesTurn     bool

	// callAt locates a call by ID: a negative message index means it is
	// still in calls, waiting for its agent item to close.
	callAt  map[string]grokCallPosition
	emitted map[string]struct{}
}

type grokCallPosition struct {
	message int
	call    int
}

func newGrokSegmentReducer(ctx context.Context) *grokSegmentReducer {
	return &grokSegmentReducer{
		ctx:     ctx,
		callAt:  make(map[string]grokCallPosition),
		emitted: make(map[string]struct{}),
	}
}

func (r *grokSegmentReducer) process(update gjson.Result, timestamp time.Time) {
	switch update.Get("sessionUpdate").Str {
	case "user_message_chunk":
		r.onUserChunk(update, timestamp)
	case "agent_message_chunk":
		if r.hostTurnBoundary(update) {
			return
		}
		r.closeUserTurn()
		if update.Get("content.type").Str == "text" {
			r.markAgent(timestamp)
			r.agentText.WriteString(update.Get("content.text").Str)
		}
	case "agent_thought_chunk":
		r.closeUserTurn()
		if update.Get("content.type").Str == "text" {
			r.markAgent(timestamp)
			r.thinking.WriteString(update.Get("content.text").Str)
		}
	case "tool_call":
		r.onToolCall(update, timestamp)
	case "tool_call_update":
		r.onToolCallUpdate(update, timestamp)
	case "turn_completed":
		r.onTurnCompleted(update)
	}
}

// onTurnCompleted stamps the turn's model on its assistant rows. The model is
// the one key of the turn's usage.modelUsage, the name chat_history.jsonl and
// the usage events record. A turn billed to several models leaves its rows
// without one, since the record does not say which row each model wrote.
func (r *grokSegmentReducer) onTurnCompleted(update gjson.Result) {
	var models []string
	update.Get("usage.modelUsage").ForEach(func(key, _ gjson.Result) bool {
		models = append(models, key.Str)
		return true
	})
	model := ""
	if len(models) == 1 {
		model = strings.TrimSpace(models[0])
	}
	for i := r.turnStart; i < len(r.messages); i++ {
		if r.messages[i].Role == RoleAssistant && r.messages[i].Model == "" {
			r.messages[i].Model = model
		}
	}
	if r.agentText.Len() > 0 || r.thinking.Len() > 0 || len(r.calls) > 0 {
		r.completedModel, r.closesTurn = model, true
		return
	}
	r.turnStart = len(r.messages)
}

func (r *grokSegmentReducer) hostTurnBoundary(update gjson.Result) bool {
	if !update.Get("_meta.hostTurn").Bool() {
		return false
	}
	r.closeUserTurn()
	r.flushAgent()
	return true
}

func (r *grokSegmentReducer) onUserChunk(update gjson.Result, timestamp time.Time) {
	if r.hostTurnBoundary(update) {
		return
	}
	if !r.inUserTurn {
		r.flushAgent()
		r.inUserTurn = true
	}
	interjection := update.Get("_meta.interjection").Bool()
	synthetic := update.Get("_meta.syntheticPrompt").Bool()
	isText := update.Get("content.type").Str == "text"
	// Each interjection text chunk opens its own item, as Grok Build's
	// rebuild does; synthetic prompts stay apart so they can be dropped.
	if interjection != r.userInterjection || synthetic != r.userSynthetic ||
		(interjection && isText) {
		r.flushUser()
	}
	r.userInterjection = interjection
	r.userSynthetic = synthetic
	if !isText {
		return
	}
	if r.userTimestamp.IsZero() {
		r.userTimestamp = timestamp
	}
	r.userParts = append(r.userParts, update.Get("content.text").Str)
}

func (r *grokSegmentReducer) onToolCall(update gjson.Result, timestamp time.Time) {
	id := strings.TrimSpace(update.Get("toolCallId").Str)
	name := firstNonEmptyJSONLString(
		update.Get(`_meta.x\.ai/tool.name`).Str,
		grokStreamBackendToolName(update.Get("rawInput")),
		update.Get("title").Str,
	)
	if name == "" {
		return
	}
	r.closeUserTurn()
	r.markAgent(timestamp)
	input := ""
	if name != grokStreamWebSearch {
		input = grokRawInputJSON(update.Get("rawInput"))
	}
	call := ParsedToolCall{
		ToolUseID: id,
		ToolName:  name,
		Category:  NormalizeToolCategory(name),
		InputJSON: input,
		SkillName: inferToolSkillName(r.ctx, name, input),
	}
	if id != "" {
		call.ResultEvents = []ParsedToolResultEvent{{
			ToolUseID: id,
			Source:    "tool_execution",
			Status:    "started",
			Timestamp: timestamp,
		}}
		r.callAt[id] = grokCallPosition{message: -1, call: len(r.calls)}
	}
	r.calls = append(r.calls, call)
}

func (r *grokSegmentReducer) onToolCallUpdate(update gjson.Result, timestamp time.Time) {
	id := strings.TrimSpace(update.Get("toolCallId").Str)
	if id == "" {
		return
	}
	call := r.call(id)
	if call != nil && call.InputJSON == "" {
		if input := grokToolUpdateInputJSON(call.ToolName, update); input != "" {
			call.InputJSON = input
			call.SkillName = inferToolSkillName(r.ctx, call.ToolName, input)
			if pos := r.callAt[id]; pos.message >= 0 {
				r.rerender(pos.message)
			}
		}
	}
	status := update.Get("status").Str
	if status != "completed" && status != "failed" {
		return
	}
	if _, done := r.emitted[id]; done {
		return
	}
	r.emitted[id] = struct{}{}
	r.flushAgent()
	content := grokToolUpdateResultText(update)
	if call = r.call(id); call != nil {
		eventStatus := status
		if status == "failed" {
			eventStatus = "errored"
		}
		call.ResultEvents = append(call.ResultEvents, ParsedToolResultEvent{
			ToolUseID: id,
			Source:    "tool_execution",
			Status:    eventStatus,
			Content:   content,
			Timestamp: timestamp,
		})
	}
	quoted, _ := json.Marshal(content)
	r.messages = append(r.messages, ParsedMessage{
		Role:          RoleUser,
		Timestamp:     timestamp,
		ContentLength: len(content),
		ToolResults: []ParsedToolResult{{
			ToolUseID:     id,
			ContentRaw:    string(quoted),
			ContentLength: len(content),
		}},
	})
}

func (r *grokSegmentReducer) call(id string) *ParsedToolCall {
	pos, ok := r.callAt[id]
	if !ok {
		return nil
	}
	if pos.message < 0 {
		return &r.calls[pos.call]
	}
	return &r.messages[pos.message].ToolCalls[pos.call]
}

func (r *grokSegmentReducer) markAgent(timestamp time.Time) {
	if r.agentTimestamp.IsZero() {
		r.agentTimestamp = timestamp
	}
}

func (r *grokSegmentReducer) closeUserTurn() {
	if r.inUserTurn {
		r.flushUser()
		r.inUserTurn = false
	}
}

func (r *grokSegmentReducer) flushUser() {
	parts, timestamp, synthetic := r.userParts, r.userTimestamp, r.userSynthetic
	r.userParts, r.userTimestamp = nil, time.Time{}
	r.userInterjection, r.userSynthetic = false, false
	if len(parts) == 0 || synthetic {
		return
	}
	content := grokNormalizeUserText(strings.Join(parts, "\n"))
	if content == "" {
		return
	}
	r.messages = append(r.messages, ParsedMessage{
		Role:          RoleUser,
		Content:       content,
		ContentLength: len(content),
		Timestamp:     timestamp,
	})
}

func (r *grokSegmentReducer) flushAgent() {
	text := strings.TrimSpace(r.agentText.String())
	thinking := strings.TrimSpace(r.thinking.String())
	calls, timestamp := r.calls, r.agentTimestamp
	model, closesTurn := r.completedModel, r.closesTurn
	r.agentText.Reset()
	r.thinking.Reset()
	r.calls, r.agentTimestamp = nil, time.Time{}
	r.completedModel, r.closesTurn = "", false
	if text != "" || thinking != "" || len(calls) > 0 {
		msg := grokAssistantMessage(text, thinking, model, calls)
		msg.Timestamp = timestamp
		index := len(r.messages)
		for i, call := range msg.ToolCalls {
			if call.ToolUseID != "" {
				r.callAt[call.ToolUseID] = grokCallPosition{message: index, call: i}
			}
		}
		r.messages = append(r.messages, msg)
	}
	if closesTurn {
		// This item closed a completed turn; the next row starts a new one.
		r.turnStart = len(r.messages)
	}
}

// rerender rebuilds an emitted assistant row after one of its calls
// received its input late, so the rendered blocks match the arguments.
func (r *grokSegmentReducer) rerender(index int) {
	old := r.messages[index]
	text := ""
	if old.DialogueText != nil {
		text = *old.DialogueText
	}
	msg := grokAssistantMessage(text, old.ThinkingText, old.Model, old.ToolCalls)
	msg.Timestamp = old.Timestamp
	r.messages[index] = msg
}

// closeItems closes the open user and agent items, as a compaction does.
func (r *grokSegmentReducer) closeItems() {
	r.flushUser()
	r.inUserTurn = false
	r.flushAgent()
}

// finish closes the open items and returns the rows.
func (r *grokSegmentReducer) finish() []ParsedMessage {
	r.closeItems()
	return r.messages
}

// grokStreamWebSearch names a backend web search rebuilt from the stream, the
// name chat_history.jsonl gives the same call.
const grokStreamWebSearch = "web_search"

// grokStreamBackendToolName names a backend tool call whose stream record has
// no tool name. Grok Build titles a backend web search "Web search:" and gives
// it only {"variant":"WebSearch","backend":true} as input; its query arrives
// with the finished update.
func grokStreamBackendToolName(rawInput gjson.Result) string {
	if rawInput.Get("backend").Bool() && rawInput.Get("variant").Str == "WebSearch" {
		return grokStreamWebSearch
	}
	return ""
}

// grokToolUpdateInputJSON returns the input a tool_call_update supplies for a
// call that started without one. A backend web search's input is the action
// in its raw output, the same input chat_history.jsonl records.
func grokToolUpdateInputJSON(name string, update gjson.Result) string {
	if name == grokStreamWebSearch {
		return update.Get("rawOutput.action").Raw
	}
	return grokRawInputJSON(update.Get("rawInput"))
}

// grokRawInputJSON keeps a tool call's raw input as JSON text. An absent or
// null input leaves the call without one, as Grok Build's rebuild does.
func grokRawInputJSON(raw gjson.Result) string {
	if !raw.Exists() || raw.Type == gjson.Null {
		return ""
	}
	if raw.Type == gjson.String {
		return raw.Str
	}
	return raw.Raw
}

// grokToolUpdateResultText extracts a finished tool call's output the way
// Grok Build writes the tool_result row: the joined text content blocks, or
// the raw output's JSON when there are none.
func grokToolUpdateResultText(update gjson.Result) string {
	var text strings.Builder
	update.Get("content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").Str == "content" &&
			block.Get("content.type").Str == "text" {
			text.WriteString(block.Get("content.text").Str)
		}
		return true
	})
	if text.Len() > 0 {
		return text.String()
	}
	if raw := update.Get("rawOutput"); raw.Exists() {
		return raw.Raw
	}
	return ""
}

// grokAssembleTranscript places the rebuilt pre-compaction segments, each
// followed by its boundary, ahead of the live chat_history.jsonl rows.
//
// After a compaction chat_history.jsonl opens with the compacted context:
// the system prompt, injected metadata, the carried user query and recent
// messages, then the summary row. That prefix replays rows the rebuilt
// segment already holds, so it is dropped up to the summary, which becomes
// the last checkpoint's boundary. When chat_history.jsonl has no summary
// row, the last boundary comes from the checkpoint file and every row stays.
//
// A call the stream never finished can have its only result in a compacted
// context: an earlier checkpoint file or the dropped prefix. Such results
// are kept after the segment that holds the call.
//
// complete is false when a checkpoint file the transcript needs could not be
// read, so the transcript lacks content an earlier sync may have stored.
func grokAssembleTranscript(
	ctx context.Context, sessionDir string,
	segments []grokCompactedSegment, chat []ParsedMessage,
) (messages []ParsedMessage, complete bool, err error) {
	if len(segments) == 0 {
		return chat, true, nil
	}
	complete = true
	summaryAt := -1
	for i := range chat {
		if chat[i].IsCompactBoundary {
			summaryAt = i
			break
		}
	}
	total := len(chat) + len(segments)
	for _, segment := range segments {
		total += len(segment.messages)
	}
	out := make([]ParsedMessage, 0, total)
	open := grokOpenCalls{}
	for i, segment := range segments {
		open.add(segment.messages)
		out = append(out, segment.messages...)
		if i == len(segments)-1 && summaryAt >= 0 {
			out = append(out, open.answer(chat[:summaryAt])...)
			break
		}
		checkpoint, err := grokReadCheckpoint(ctx, sessionDir, segment.checkpoint.file)
		if err != nil {
			return nil, false, err
		}
		complete = complete && checkpoint.read
		out = append(out, open.answer(checkpoint.results)...)
		out = append(out, grokCompactBoundary(checkpoint.summary, segment.checkpoint.timestamp))
	}
	if summaryAt >= 0 {
		chat = chat[summaryAt:]
		if chat[0].Timestamp.IsZero() {
			chat[0].Timestamp = segments[len(segments)-1].checkpoint.timestamp
		}
	}
	out = append(out, chat...)
	for i := range out {
		out[i].Ordinal = i
	}
	return out, complete, nil
}

// grokOpenCalls tracks rebuilt calls that no result row answers yet.
type grokOpenCalls map[string]bool

func (o grokOpenCalls) add(messages []ParsedMessage) {
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if call.ToolUseID != "" {
				o[call.ToolUseID] = true
			}
		}
		for _, result := range msg.ToolResults {
			delete(o, result.ToolUseID)
		}
	}
}

// answer returns the result rows among carried that answer an open call,
// and closes those calls.
func (o grokOpenCalls) answer(carried []ParsedMessage) []ParsedMessage {
	var kept []ParsedMessage
	for _, msg := range carried {
		answers := false
		for _, result := range msg.ToolResults {
			if o[result.ToolUseID] {
				answers = true
			}
		}
		if !answers {
			continue
		}
		for _, result := range msg.ToolResults {
			delete(o, result.ToolUseID)
		}
		kept = append(kept, msg)
	}
	return kept
}

// grokDropLoneStartEvents clears a call's events when they hold only its
// start but a result row answers it. An interrupted call can have its start
// in updates.jsonl and its result only in chat_history.jsonl; the call
// summary built from events would otherwise replace that result with the
// start event's empty text.
func grokDropLoneStartEvents(messages []ParsedMessage) {
	answered := map[string]bool{}
	for _, msg := range messages {
		for _, result := range msg.ToolResults {
			if strings.TrimSpace(DecodeContent(result.ContentRaw)) != "" {
				answered[result.ToolUseID] = true
			}
		}
	}
	for i := range messages {
		for j := range messages[i].ToolCalls {
			call := &messages[i].ToolCalls[j]
			if answered[call.ToolUseID] && !grokHasFinishedEvent(call.ResultEvents) {
				call.ResultEvents = nil
			}
		}
	}
}

func grokHasFinishedEvent(events []ParsedToolResultEvent) bool {
	for _, event := range events {
		if event.Status != "started" {
			return true
		}
	}
	return false
}
