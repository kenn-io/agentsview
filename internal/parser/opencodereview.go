package parser

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"encoding/json/jsontext"
)

type openCodeReviewRecord struct {
	Type          string                     `json:"type"`
	UUID          string                     `json:"uuid"`
	ParentUUID    string                     `json:"parentUuid"`
	SessionID     string                     `json:"sessionId"`
	Timestamp     string                     `json:"timestamp"`
	Cwd           string                     `json:"cwd"`
	GitBranch     string                     `json:"gitBranch"`
	Model         string                     `json:"model"`
	ReviewMode    string                     `json:"reviewMode"`
	DiffFrom      string                     `json:"diffFrom"`
	DiffTo        string                     `json:"diffTo"`
	DiffCommit    string                     `json:"diffCommit"`
	ResumedFrom   string                     `json:"resumedFrom"`
	FilePath      string                     `json:"filePath"`
	TaskType      string                     `json:"taskType"`
	Messages      []json.RawMessage          `json:"messages"`
	Content       string                     `json:"content"`
	Reasoning     string                     `json:"reasoning_content"`
	ToolCalls     []openCodeReviewToolCall   `json:"tool_calls"`
	Usage         *openCodeReviewUsage       `json:"usage"`
	ToolName      string                     `json:"tool_name"`
	Arguments     json.RawMessage            `json:"arguments"`
	Result        string                     `json:"result"`
	OK            *bool                      `json:"ok"`
	Error         string                     `json:"error"`
	Comments      []json.RawMessage          `json:"comments"`
	SourceSession string                     `json:"sourceSessionId"`
	ParentRunID   string                     `json:"parent_run_id"`
	RunManifest   *openCodeReviewRunManifest `json:"run_manifest"`
}

type openCodeReviewToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type openCodeReviewUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	CacheReadTokens  *int `json:"cache_read_tokens"`
	CacheWriteTokens *int `json:"cache_write_tokens"`
}

type openCodeReviewRunManifest struct {
	Execution struct {
		OCRVersion string `json:"ocr_version"`
	} `json:"execution"`
}

type openCodeReviewStream struct {
	FilePath string
	TaskType string
}

type openCodeReviewRequestHistory struct {
	Count int
	Tail  [sha256.Size]byte
}

type openCodeReviewPendingCall struct {
	Message int
	Tool    int
}

type openCodeReviewParserState struct {
	started       bool
	ended         bool
	truncated     bool
	malformed     int
	sessionID     string
	project       string
	cwd           string
	gitBranch     string
	model         string
	reviewMode    string
	diffFrom      string
	diffTo        string
	diffCommit    string
	resumedFrom   string
	lineageParent string
	sourceVersion string
	startedAt     time.Time
	endedAt       time.Time
	messages      []ParsedMessage
	history       map[openCodeReviewStream]openCodeReviewRequestHistory
	pending       map[openCodeReviewStream]map[string][]openCodeReviewPendingCall
}

func parseOpenCodeReviewFile(
	ctx context.Context,
	path, pathProject, machine string,
	fingerprint SourceFingerprint,
) (*ParseResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Open Code Review session %s: %w", path, err)
	}
	defer file.Close()

	state := openCodeReviewParserState{
		project: pathProject,
		history: make(map[openCodeReviewStream]openCodeReviewRequestHistory),
		pending: make(map[openCodeReviewStream]map[string][]openCodeReviewPendingCall),
	}
	reader := newLineReaderContext(ctx, file, maxLineSize)
	defer releaseLineReader(reader)
	lastMalformed := false
	for {
		line, ok := reader.nextBytes()
		if !ok {
			break
		}
		var record openCodeReviewRecord
		lastMalformed = json.Unmarshal(line, &record) != nil
		if lastMalformed {
			state.malformed++
			continue
		}
		openCodeReviewConsumeRecord(&state, record)
	}
	if err := reader.Err(); err != nil {
		return nil, fmt.Errorf("read Open Code Review session %s: %w", path, err)
	}
	if lastMalformed && !fileEndsWithNewline(file, reader.bytesRead) {
		state.truncated = true
		state.malformed--
	}
	if !state.started {
		return nil, nil
	}

	project := state.project
	if fromCwd := ExtractProjectFromCwdWithBranchContext(ctx, state.cwd, state.gitBranch); fromCwd != "" {
		project = fromCwd
	}
	if project == "" {
		project = "unknown"
	}
	title := openCodeReviewSessionName(state.diffCommit, state.diffFrom, state.diffTo, state.reviewMode)
	if state.endedAt.IsZero() {
		state.endedAt = lastOpenCodeReviewTimestamp(state.messages, state.startedAt)
	}
	status := TerminationStatus("")
	if state.truncated {
		status = TerminationTruncated
	} else if state.ended {
		status = TerminationClean
	} else {
		status = classifyOpenCodeReviewTermination(state.messages)
	}
	firstMessage, userCount := firstMessageAndUserCount(state.messages)

	sessionID := state.sessionID
	parentID := state.resumedFrom
	if parentID == "" {
		parentID = state.lineageParent
	}
	if parentID != "" {
		parentID = string(AgentOpenCodeReview) + ":" + parentID
	}
	session := ParsedSession{
		ID:                  string(AgentOpenCodeReview) + ":" + sessionID,
		SourceSessionID:     sessionID,
		Project:             project,
		Machine:             machine,
		Agent:               AgentOpenCodeReview,
		AgentLabel:          "Open Code Review",
		ParentSessionID:     parentID,
		Cwd:                 state.cwd,
		GitBranch:           state.gitBranch,
		SourceVersion:       state.sourceVersion,
		SessionKind:         state.reviewMode,
		SessionName:         title,
		SessionNamePresent:  true,
		FirstMessage:        firstMessage,
		StartedAt:           state.startedAt,
		EndedAt:             state.endedAt,
		MessageCount:        len(state.messages),
		UserMessageCount:    userCount,
		MalformedLines:      state.malformed,
		IsTruncated:         state.truncated,
		TerminationStatus:   status,
		CountsAuthoritative: true,
		File: FileInfo{
			Path:   path,
			Size:   fingerprint.Size,
			Mtime:  fingerprint.MTimeNS,
			Inode:  int64(fingerprint.Inode),
			Device: int64(fingerprint.Device),
			Hash:   fingerprint.Hash,
		},
	}
	accumulateMessageTokenUsage(&session, state.messages)
	result := ParseResult{Session: session, Messages: state.messages}
	results := []ParseResult{result}
	InferRelationshipTypes(results)
	return &results[0], nil
}

func openCodeReviewConsumeRecord(state *openCodeReviewParserState, record openCodeReviewRecord) {
	if !state.started {
		if record.Type != "session_start" || record.SessionID == "" {
			return
		}
		state.started = true
		state.sessionID = record.SessionID
		state.cwd = record.Cwd
		state.gitBranch = record.GitBranch
		state.model = record.Model
		state.reviewMode = record.ReviewMode
		state.diffFrom = record.DiffFrom
		state.diffTo = record.DiffTo
		state.diffCommit = record.DiffCommit
		state.resumedFrom = record.ResumedFrom
		state.startedAt = parseTimestamp(record.Timestamp)
		return
	}
	if record.RunManifest != nil && record.RunManifest.Execution.OCRVersion != "" {
		state.sourceVersion = record.RunManifest.Execution.OCRVersion
	}

	switch record.Type {
	case "session_start":
		return
	case "llm_request":
		openCodeReviewRequest(state, record)
	case "llm_response":
		openCodeReviewResponse(state, record)
	case "llm_error":
		openCodeReviewDiagnostic(state, record.UUID, record.ParentUUID, parseTimestamp(record.Timestamp), record.Error, "llm_error")
	case "tool_call":
		openCodeReviewToolResult(state, record)
	case "review_item_done", "review_item_reused":
		if len(record.Comments) > 0 {
			content, err := json.MarshalIndent(struct {
				SourceSession string            `json:"sourceSessionId,omitempty"`
				Comments      []json.RawMessage `json:"comments"`
			}{record.SourceSession, record.Comments}, "", "  ")
			if err == nil {
				openCodeReviewDiagnostic(state, record.UUID, record.ParentUUID, parseTimestamp(record.Timestamp), string(content), record.Type)
			}
		}
	case "review_item_failed":
		if record.Error != "" {
			openCodeReviewDiagnostic(state, record.UUID, record.ParentUUID, parseTimestamp(record.Timestamp), record.Error, record.Type)
		}
	case "resume_lineage":
		state.lineageParent = record.ParentRunID
	case "session_end":
		state.ended = true
		state.endedAt = parseTimestamp(record.Timestamp)
	}
}

func openCodeReviewRequest(state *openCodeReviewParserState, record openCodeReviewRecord) {
	if (record.TaskType != "main_task" && record.TaskType != "plan_task") || len(record.Messages) == 0 {
		return
	}
	stream := openCodeReviewStream{FilePath: record.FilePath, TaskType: record.TaskType}
	// Compression rewrites the initial prompt and shrinks the history. Only
	// newly appended messages are user turns; keep the current length so later
	// requests can append turns after a compression.
	previous := state.history[stream]
	count := len(record.Messages)
	tail := sha256.Sum256(record.Messages[count-1])
	start := min(previous.Count, count)
	// A grace prompt can be appended immediately after compression, before
	// a shorter request is recorded. Inspect its new tail while excluding the
	// frozen system/user pair. The digest also keeps request replays silent.
	if start == count && count > 2 && tail != previous.Tail {
		start = count - 1
	}
	state.history[stream] = openCodeReviewRequestHistory{Count: count, Tail: tail}
	for _, raw := range record.Messages[start:] {
		var message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if json.Unmarshal(raw, &message) != nil || message.Role != "user" {
			continue
		}
		content := message.Content
		if content == "" {
			continue
		}
		openCodeReviewAppendMessage(state, ParsedMessage{
			Role:             RoleUser,
			Content:          content,
			Timestamp:        parseTimestamp(record.Timestamp),
			SourceType:       "user",
			SourceUUID:       record.UUID,
			SourceParentUUID: record.ParentUUID,
		})
	}
}

func classifyOpenCodeReviewTermination(messages []ParsedMessage) TerminationStatus {
	for _, message := range slices.Backward(messages) {
		if message.Role != RoleAssistant || message.IsSystem {
			continue
		}
		// task_done is a control signal with no execution result. Keep it in
		// the transcript, but exclude it from pending-call classification.
		// Open Code Review attaches all real results to their calls.
		message.ToolCalls = slices.DeleteFunc(slices.Clone(message.ToolCalls), func(call ParsedToolCall) bool {
			return call.ToolName == "task_done"
		})
		return Classify([]ParsedMessage{message}, "", false)
	}
	return Classify(messages, "", false)
}

func openCodeReviewResponse(state *openCodeReviewParserState, record openCodeReviewRecord) {
	message := ParsedMessage{
		Role:             RoleAssistant,
		Content:          record.Content,
		ThinkingText:     record.Reasoning,
		HasThinking:      record.Reasoning != "",
		Timestamp:        parseTimestamp(record.Timestamp),
		Model:            firstNonEmptyJSONLString(record.Model, state.model),
		SourceType:       "assistant",
		SourceUUID:       record.UUID,
		SourceParentUUID: record.ParentUUID,
	}
	if state.model == "" && record.Model != "" {
		state.model = record.Model
	}
	for _, call := range record.ToolCalls {
		name := call.Name
		if name == "" {
			continue
		}
		message.HasToolUse = true
		message.ToolCalls = append(message.ToolCalls, ParsedToolCall{
			ToolUseID: call.ID,
			ToolName:  name,
			Category:  NormalizeToolCategory(name),
			InputJSON: openCodeReviewArguments(call.Arguments),
		})
	}
	openCodeReviewApplyUsage(&message, record.Usage)
	messageIndex := openCodeReviewAppendMessage(state, message)
	stream := openCodeReviewStream{FilePath: record.FilePath, TaskType: record.TaskType}
	if len(message.ToolCalls) == 0 {
		return
	}
	byName := state.pending[stream]
	if byName == nil {
		byName = make(map[string][]openCodeReviewPendingCall)
		state.pending[stream] = byName
	}
	for index, call := range message.ToolCalls {
		if call.ToolName == "task_done" {
			// Successful completion allows another review round to reuse this
			// stream with a fresh prompt. Invalid task_done calls keep retrying.
			var args map[string]any
			if json.Unmarshal([]byte(call.InputJSON), &args) == nil {
				value, present := args["state"]
				if !present || value == "DONE" {
					delete(state.history, stream)
				}
			}
			continue
		}
		byName[call.ToolName] = append(byName[call.ToolName], openCodeReviewPendingCall{Message: messageIndex, Tool: index})
	}
}

func openCodeReviewToolResult(state *openCodeReviewParserState, record openCodeReviewRecord) {
	stream := openCodeReviewStream{FilePath: record.FilePath, TaskType: record.TaskType}
	byName := state.pending[stream]
	if byName == nil {
		openCodeReviewDiagnostic(state, record.UUID, record.ParentUUID, parseTimestamp(record.Timestamp), openCodeReviewToolResultText(record), "unmatched_tool_result")
		return
	}
	pending := byName[record.ToolName]
	if len(pending) == 0 {
		openCodeReviewDiagnostic(state, record.UUID, record.ParentUUID, parseTimestamp(record.Timestamp), openCodeReviewToolResultText(record), "unmatched_tool_result")
		return
	}
	target := pending[0]
	if len(pending) == 1 {
		delete(byName, record.ToolName)
	} else {
		byName[record.ToolName] = pending[1:]
	}
	status := "completed"
	if (record.OK != nil && !*record.OK) || record.Error != "" {
		status = "errored"
	}
	state.messages[target.Message].ToolCalls[target.Tool].ResultEvents = append(
		state.messages[target.Message].ToolCalls[target.Tool].ResultEvents,
		ParsedToolResultEvent{
			ToolUseID: state.messages[target.Message].ToolCalls[target.Tool].ToolUseID,
			Source:    "tool_execution",
			Status:    status,
			Content:   openCodeReviewToolResultText(record),
			Timestamp: parseTimestamp(record.Timestamp),
		},
	)
}

func openCodeReviewToolResultText(record openCodeReviewRecord) string {
	if record.Result != "" {
		return record.Result
	}
	return record.Error
}

func openCodeReviewDiagnostic(state *openCodeReviewParserState, uuid, parent string, timestamp time.Time, content, subtype string) {
	if content == "" {
		return
	}
	openCodeReviewAppendMessage(state, ParsedMessage{
		Role:             RoleSystem,
		IsSystem:         true,
		Content:          content,
		Timestamp:        timestamp,
		SourceType:       "system",
		SourceSubtype:    subtype,
		SourceUUID:       uuid,
		SourceParentUUID: parent,
	})
}

func openCodeReviewAppendMessage(state *openCodeReviewParserState, message ParsedMessage) int {
	message.Ordinal = len(state.messages)
	message.ContentLength = len(message.Content)
	state.messages = append(state.messages, message)
	return len(state.messages) - 1
}

func openCodeReviewApplyUsage(message *ParsedMessage, usage *openCodeReviewUsage) {
	if usage == nil {
		return
	}
	values := map[string]int{}
	contextTokens := 0
	if usage.CompletionTokens != nil {
		values["output_tokens"] = *usage.CompletionTokens
		message.OutputTokens = *usage.CompletionTokens
		message.HasOutputTokens = true
	}
	if usage.CacheReadTokens != nil {
		values["cache_read_input_tokens"] = *usage.CacheReadTokens
		contextTokens += *usage.CacheReadTokens
		message.HasContextTokens = true
	}
	if usage.CacheWriteTokens != nil {
		values["cache_creation_input_tokens"] = *usage.CacheWriteTokens
		contextTokens += *usage.CacheWriteTokens
		message.HasContextTokens = true
	}
	if usage.PromptTokens != nil {
		// Prompt totals include cache reads and writes on the OpenAI and
		// native Anthropic paths. Pricing expects the uncached remainder.
		values["input_tokens"] = max(*usage.PromptTokens-values["cache_read_input_tokens"]-values["cache_creation_input_tokens"], 0)
		contextTokens = *usage.PromptTokens
		message.HasContextTokens = true
	}
	if len(values) == 0 {
		return
	}
	message.ContextTokens = contextTokens
	raw, err := json.Marshal(values)
	if err == nil {
		message.TokenUsage = jsontext.Value(raw)
	}
}

func openCodeReviewSessionName(diffCommit, diffFrom, diffTo, reviewMode string) string {
	if diffCommit != "" {
		return "Review commit " + diffCommit
	}
	if diffFrom != "" || diffTo != "" {
		return "Review " + diffFrom + ".." + diffTo
	}
	if reviewMode == "full_scan" {
		return "Full repository review"
	}
	if reviewMode != "" {
		return "Open Code Review " + reviewMode
	}
	return "Open Code Review"
}

func lastOpenCodeReviewTimestamp(messages []ParsedMessage, fallback time.Time) time.Time {
	last := fallback
	for _, message := range messages {
		if message.Timestamp.After(last) {
			last = message.Timestamp
		}
	}
	return last
}

func openCodeReviewArguments(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}
