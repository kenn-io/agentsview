package parser

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// VibeMessage represents a single message in a Mistral Vibe session
type VibeMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []VibeToolCall `json:"tool_calls,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	MessageID        string         `json:"message_id,omitempty"`
	Injected         bool           `json:"injected,omitempty"`
	Model            string         `json:"model,omitempty"`
}

// VibeToolCall represents a tool invocation in Vibe
type VibeToolCall struct {
	ID       string               `json:"id"`
	Index    int                  `json:"index,omitempty"`
	Function VibeToolCallFunction `json:"function"`
}

// VibeToolCallFunction represents the function details of a tool call
type VibeToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// VibeSessionMetadata represents session-level metadata from meta.json
type VibeSessionMetadata struct {
	SessionID       string    `json:"session_id"`
	ParentSessionID *string   `json:"parent_session_id"`
	StartTime       time.Time `json:"start_time"`
	EndTime         time.Time `json:"end_time"`
	GitCommit       string    `json:"git_commit,omitempty"`
	GitBranch       string    `json:"git_branch,omitempty"`
	Model           string    `json:"model,omitempty"`
	WorkingDir      string    `json:"working_dir,omitempty"`
	Title           string    `json:"title,omitempty"`
	Stats           VibeStats `json:"stats,omitempty"`
	Environment     struct {
		WorkingDirectory string `json:"working_directory,omitempty"`
	} `json:"environment,omitempty"`
	Config struct {
		ActiveModel string `json:"active_model,omitempty"`
	} `json:"config,omitempty"`
}

// VibeStats represents token usage statistics from meta.json
type VibeStats struct {
	Steps                    int   `json:"steps"`
	SessionPromptTokens      int   `json:"session_prompt_tokens"`
	SessionCompletionTokens  int   `json:"session_completion_tokens"`
	ContextTokens            int   `json:"context_tokens"`
	LastTurnPromptTokens     int   `json:"last_turn_prompt_tokens"`
	LastTurnCompletionTokens int   `json:"last_turn_completion_tokens"`
	SessionTotalLLMTokens    int64 `json:"session_total_llm_tokens"`
	LastTurnTotalTokens      int   `json:"last_turn_total_tokens"`
}

// parseVibeResult parses a Mistral Vibe messages.jsonl file into a ParseResult.
// It owns the on-disk shape (messages.jsonl plus the sibling meta.json) for the
// Vibe provider; the package-level entrypoint was folded onto the provider.
func parseVibeResultFile(path string, fileInfo FileInfo) (ParseResult, error) {
	result := ParseResult{
		Session: ParsedSession{
			Agent:     AgentVibe,
			File:      fileInfo,
			StartedAt: time.Unix(0, fileInfo.Mtime),
			EndedAt:   time.Unix(0, fileInfo.Mtime),
		},
	}

	// Extract session ID from directory name as a fallback. The "vibe:"
	// prefix is always applied so prefix-based routing (AgentByPrefix,
	// FindSourceFile) works even when meta.json is missing.
	// Path format: .../session/session_YYYYMMDD_HHMMSS_uuid/messages.jsonl
	dir := filepath.Dir(path)
	result.Session.ID = "vibe:" + filepath.Base(dir)
	result.Session.Project = "vibe"

	// Try to parse meta.json for additional metadata
	var sessionModel string
	var sessionStats VibeStats
	var hasMetaData bool
	metaPath := filepath.Join(dir, "meta.json")
	metaData, metaErr := parseVibeMetadata(metaPath)
	switch {
	case metaErr != nil && errors.Is(metaErr, os.ErrNotExist):
		// meta.json has not been written yet (a freshly created session):
		// keep the directory-name fallback ID set above.
	case metaErr != nil:
		// meta.json exists but the full parse failed: a partial write or a
		// single malformed optional field. Recover the canonical session_id
		// from a tolerant minimal parse so a transient error does not abandon
		// the canonical session row for the directory-name fallback (which
		// would exclude and re-create the row, dropping pins and usage). If
		// even the minimal parse fails, surface the error so the sync retries
		// and leaves the existing row untouched rather than replacing it.
		id, idErr := parseVibeSessionID(metaPath)
		if idErr != nil {
			return result, fmt.Errorf(
				"parsing Vibe meta.json %s: %w", metaPath, metaErr,
			)
		}
		if id != "" {
			result.Session.ID = "vibe:" + id
			result.Session.SourceSessionID = id
		}
	default:
		hasMetaData = true
		sessionStats = metaData.Stats
		// Use the actual session_id from meta.json as the canonical ID
		if metaData.SessionID != "" {
			result.Session.ID = "vibe:" + metaData.SessionID
			result.Session.SourceSessionID = metaData.SessionID
		}
		if metaData.Title != "" {
			result.Session.SessionName = metaData.Title
		}
		if !metaData.StartTime.IsZero() {
			result.Session.StartedAt = metaData.StartTime
		}
		if !metaData.EndTime.IsZero() {
			result.Session.EndedAt = metaData.EndTime
		}
		if metaData.Model != "" {
			sessionModel = metaData.Model
		}
		if metaData.WorkingDir != "" {
			result.Session.Cwd = metaData.WorkingDir
			// Derive a human-readable project name from the working
			// directory (the enclosing git repo, or its basename),
			// matching how Claude sessions are grouped. Falls back to
			// "vibe" when no working directory is recorded.
			if p := ExtractProjectFromCwdWithBranch(
				metaData.WorkingDir, metaData.GitBranch,
			); p != "" {
				result.Session.Project = p
			}
		}
		if metaData.GitBranch != "" {
			result.Session.GitBranch = metaData.GitBranch
		}
		if metaData.GitCommit != "" {
			result.Session.SourceVersion = metaData.GitCommit
		}
		// Extract token usage from stats
		if metaData.Stats.SessionCompletionTokens > 0 {
			result.Session.HasTotalOutputTokens = true
			result.Session.TotalOutputTokens = metaData.Stats.SessionCompletionTokens
		}
		if metaData.Stats.ContextTokens > 0 {
			result.Session.HasPeakContextTokens = true
			result.Session.PeakContextTokens = metaData.Stats.ContextTokens
		} else if metaData.Stats.SessionPromptTokens > 0 {
			result.Session.HasPeakContextTokens = true
			result.Session.PeakContextTokens = metaData.Stats.SessionPromptTokens
		}

		// Handle parent session relationship. The parent reference is a
		// bare session_id, so prefix it to match the canonical ID scheme.
		if metaData.ParentSessionID != nil && *metaData.ParentSessionID != "" {
			result.Session.ParentSessionID = "vibe:" + *metaData.ParentSessionID
			result.Session.RelationshipType = RelContinuation
		}
	}

	// Parse messages.jsonl
	file, err := os.Open(path)
	if err != nil {
		return result, fmt.Errorf("failed to open Vibe session file: %w", err)
	}
	defer file.Close()

	lr := newLineReader(file, maxLineSize)
	messageOrdinal := 0
	var firstUserContent string

	for {
		line, ok := lr.next()
		if !ok {
			break
		}

		var vibeMsg VibeMessage
		if err := json.Unmarshal([]byte(line), &vibeMsg); err != nil {
			result.Session.MalformedLines++
			continue
		}

		// Try to extract session model from first assistant message if not set yet
		if sessionModel == "" && vibeMsg.Role == "assistant" && vibeMsg.Model != "" {
			sessionModel = vibeMsg.Model
		}

		// Tool results are separate "tool" records linked back to the
		// assistant's tool call via tool_call_id. Emit them as an empty
		// RoleUser carrier message (matching the Hermes/QClaw/OpenClaw
		// convention) so the sync engine's pairToolResults can attach the
		// result to the originating tool call by ID; the carrier message
		// itself is filtered out of the visible transcript afterward.
		if vibeMsg.Role == "tool" {
			if vibeMsg.ToolCallID == "" {
				continue
			}
			quoted, err := json.Marshal(vibeMsg.Content)
			if err != nil {
				continue
			}
			result.Messages = append(result.Messages, ParsedMessage{
				Ordinal:       messageOrdinal,
				Role:          RoleUser,
				Content:       "",
				ContentLength: len(vibeMsg.Content),
				ToolResults: []ParsedToolResult{{
					ToolUseID:     vibeMsg.ToolCallID,
					ContentRaw:    string(quoted),
					ContentLength: len(vibeMsg.Content),
				}},
			})
			messageOrdinal++
			continue
		}

		// Convert Vibe message to AgentsView ParsedMessage
		msg, _ := convertVibeMessage(vibeMsg, messageOrdinal, sessionModel)
		result.Messages = append(result.Messages, msg)

		// Track first user message content for session metadata. Skip
		// system/injected context so it never becomes the session's first
		// message.
		if firstUserContent == "" && msg.Role == RoleUser &&
			!msg.IsSystem && msg.Content != "" {
			firstUserContent = msg.Content
		}

		messageOrdinal++
	}

	if err := lr.Err(); err != nil {
		return result, fmt.Errorf("failed to read Vibe session file: %w", err)
	}

	// Set session metadata from messages
	if len(result.Messages) > 0 {
		result.Session.MessageCount = len(result.Messages)
		result.Session.FirstMessage = firstUserContent
	}

	// Count real user messages, excluding system/injected context and the
	// empty tool-result carrier messages emitted for "tool" records (which
	// carry RoleUser to satisfy pairToolResults).
	for _, msg := range result.Messages {
		if msg.Role == RoleUser && !msg.IsSystem && len(msg.ToolResults) == 0 {
			result.Session.UserMessageCount++
		}
	}

	// Create usage events from session stats if we have stats, a model, and any token data
	if hasMetaData && sessionModel != "" {
		if usageEvents := vibeUsageEvents(
			sessionStats, sessionModel, result.Session.ID,
			result.Session.StartedAt, result.Session.EndedAt,
		); len(usageEvents) > 0 {
			result.UsageEvents = usageEvents
		}
	}

	return result, nil
}

// parseVibeMetadata parses the meta.json file for session-level metadata
func parseVibeMetadata(path string) (VibeSessionMetadata, error) {
	var meta VibeSessionMetadata
	data, err := os.ReadFile(path)
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}

	// Extract working directory from environment if not at top level
	if meta.WorkingDir == "" && meta.Environment.WorkingDirectory != "" {
		meta.WorkingDir = meta.Environment.WorkingDirectory
	}

	// Vibe records the model under config.active_model rather than a
	// top-level "model" field, so fall back to it. Without this the
	// session has no model and no usage event is emitted.
	if meta.Model == "" && meta.Config.ActiveModel != "" {
		meta.Model = meta.Config.ActiveModel
	}

	return meta, nil
}

// parseVibeSessionID extracts only the canonical session_id from meta.json
// using a minimal tolerant struct. A malformed optional field (for example a
// bad timestamp) fails the full VibeSessionMetadata parse but must not cost the
// session its stable identity, so the id is recovered independently. Returns an
// error when the file cannot be read or is not even minimally valid JSON.
func parseVibeSessionID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var meta struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", err
	}
	return meta.SessionID, nil
}

// convertVibeMessage converts a VibeMessage to a ParsedMessage
func convertVibeMessage(vibeMsg VibeMessage, ordinal int, defaultModel string) (ParsedMessage, []ParsedToolCall) {
	msg := ParsedMessage{
		Ordinal:       ordinal,
		Role:          RoleType(vibeMsg.Role),
		Content:       vibeMsg.Content,
		ContentLength: len(vibeMsg.Content),
		Model:         vibeMsg.Model,
		SourceUUID:    vibeMsg.MessageID,
	}

	// Use default model if message doesn't have one
	if msg.Model == "" && defaultModel != "" {
		msg.Model = defaultModel
	}

	// Handle reasoning content as thinking
	if vibeMsg.ReasoningContent != "" {
		msg.ThinkingText = vibeMsg.ReasoningContent
		msg.HasThinking = true
	}

	// Handle system messages. Injected records carry system context (often
	// under a user role), so mark them system too; they are then excluded from
	// first-message and user-message-count derivation.
	if vibeMsg.Role == "system" || vibeMsg.Injected {
		msg.IsSystem = true
	}

	var toolCalls []ParsedToolCall

	// Convert tool calls
	for _, tc := range vibeMsg.ToolCalls {
		toolCall := ParsedToolCall{
			ToolUseID: tc.ID,
			ToolName:  tc.Function.Name,
			Category:  NormalizeToolCategory(tc.Function.Name),
			InputJSON: vibeToolArguments(tc.Function.Arguments),
		}
		toolCalls = append(toolCalls, toolCall)
	}

	if len(toolCalls) > 0 {
		msg.HasToolUse = true
		msg.ToolCalls = toolCalls
	}

	return msg, toolCalls
}

// vibeToolArguments normalizes a tool call's arguments to a raw JSON object.
// Vibe (like the OpenAI/Mistral wire format) may encode arguments either as a
// nested JSON object or as a JSON-encoded string; the latter is unwrapped so
// InputJSON is always the underlying object.
func vibeToolArguments(args json.RawMessage) string {
	if len(args) > 0 && args[0] == '"' {
		var s string
		if err := json.Unmarshal(args, &s); err == nil {
			return s
		}
	}
	return string(args)
}

// parseSession parses a Vibe session at path and returns the session, messages,
// and usage events in the shape the provider consumes: (*ParsedSession,
// []ParsedMessage, []ParsedUsageEvent, error). It stats the file to build
// FileInfo and optionally overrides the project and machine.
func parseVibeSession(path, project, machine string) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	fileInfo := FileInfo{
		Path:  path,
		Size:  info.Size(),
		Mtime: info.ModTime().UnixNano(),
	}

	result, err := parseVibeResultFile(path, fileInfo)
	if err != nil {
		return nil, nil, nil, err
	}

	// Override project if provided
	if project != "" {
		result.Session.Project = project
	}

	// Override machine if provided
	if machine != "" {
		result.Session.Machine = machine
	}

	return &result.Session, result.Messages, result.UsageEvents, nil
}

// vibeUsageEvents creates usage events from Vibe session stats.
// Vibe stores aggregate token usage in meta.json stats, so we emit a single
// session-level usage event similar to Hermes.
func vibeUsageEvents(
	stats VibeStats, model, sessionID string, startedAt, endedAt time.Time,
) []ParsedUsageEvent {
	// Only emit an event if we have a model and at least some token usage data
	if model == "" {
		return nil
	}
	if stats.SessionPromptTokens == 0 && stats.SessionCompletionTokens == 0 &&
		stats.ContextTokens == 0 && stats.SessionTotalLLMTokens == 0 {
		return nil
	}

	// Use SessionPromptTokens for input tokens and SessionCompletionTokens for output tokens.
	// SessionTotalLLMTokens appears to be the sum of input + output tokens,
	// so we don't use it as a direct source but it can serve as validation.
	inputTokens := stats.SessionPromptTokens
	outputTokens := stats.SessionCompletionTokens

	return []ParsedUsageEvent{{
		SessionID:    sessionID,
		Source:       "session",
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		// Vibe doesn't currently expose cache token breakdown in meta.json
		CacheCreationInputTokens: 0,
		CacheReadInputTokens:     0,
		ReasoningTokens:          0,
		// Vibe doesn't currently expose cost information
		CostUSD:    nil,
		CostStatus: "",
		CostSource: "",
		OccurredAt: vibeTimeString(endedAt, startedAt),
		DedupKey:   "session:" + sessionID,
	}}
}

// vibeTimeString returns the RFC3339Nano formatted string for primary time,
// falling back to fallback if primary is zero.
func vibeTimeString(primary, fallback time.Time) string {
	if !primary.IsZero() {
		return primary.Format(time.RFC3339Nano)
	}
	if !fallback.IsZero() {
		return fallback.Format(time.RFC3339Nano)
	}
	return ""
}
