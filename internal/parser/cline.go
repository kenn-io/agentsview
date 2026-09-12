// ABOUTME: Parses Cline CLI (cline) session files from the ~/.cline/data/sessions/
// ABOUTME: directory. Each session is a directory containing <sessionId>.json
// ABOUTME: (metadata and usage) and <sessionId>.messages.json (transcript).
package parser

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

type clineSessionMetadata struct {
	Version       int              `json:"version,omitempty"`
	SessionID     string           `json:"session_id"`
	Source        string           `json:"source,omitempty"`
	StartedAt     string           `json:"started_at,omitempty"`
	EndedAt       string           `json:"ended_at,omitempty"`
	ExitCode      *int             `json:"exit_code,omitempty"`
	Status        string           `json:"status,omitempty"`
	Interactive   bool             `json:"interactive,omitempty"`
	Provider      string           `json:"provider,omitempty"`
	Model         string           `json:"model,omitempty"`
	Cwd           string           `json:"cwd,omitempty"`
	WorkspaceRoot string           `json:"workspace_root,omitempty"`
	Prompt        string           `json:"prompt,omitempty"`
	Metadata      clineMetadataObj `json:"metadata,omitempty"`
}

type clineMetadataObj struct {
	Title          string          `json:"title,omitempty"`
	TotalCost      *jsontext.Value `json:"totalCost,omitempty"`
	Usage          *clineUsageObj  `json:"usage,omitempty"`
	AggregateUsage *clineUsageObj  `json:"aggregateUsage,omitempty"`
	Git            *clineGitObj    `json:"git,omitempty"`
}

type clineGitObj struct {
	URL    string `json:"url,omitempty"`
	Branch string `json:"branch,omitempty"`
}

type clineUsageObj struct {
	InputTokens      int             `json:"inputTokens,omitempty"`
	OutputTokens     int             `json:"outputTokens,omitempty"`
	CacheReadTokens  int             `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int             `json:"cacheWriteTokens,omitempty"`
	TotalCost        *jsontext.Value `json:"totalCost,omitempty"`
}

type clineMessagesFile struct {
	Messages []clineRawMessage `json:"messages"`
}

type clineRawMessage struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"` // "user" or "assistant"
	Content   []clineRawBlock `json:"content"`
	Timestamp int64           `json:"ts"` // epoch ms
	ModelInfo *clineModelInfo `json:"modelInfo,omitempty"`
	Metrics   *clineMetrics   `json:"metrics,omitempty"`
}

type clineModelInfo struct {
	ID       string `json:"id,omitempty"`
	Provider string `json:"provider,omitempty"`
	Family   string `json:"family,omitempty"`
}

type clineMetrics struct {
	InputTokens      int `json:"inputTokens,omitempty"`
	OutputTokens     int `json:"outputTokens,omitempty"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
}

type clineRawBlock struct {
	Type      string         `json:"type"` // "text", "thinking", "tool_use", "tool_result"
	Text      string         `json:"text,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     jsontext.Value `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   jsontext.Value `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
}

var clineUserInputRe = regexp.MustCompile(`(?s)^\s*<user_input(?:\s+mode="[^"]*")?\s*>(.*?)</user_input>\s*$`)

// cleanClinePrompt strips any outer <user_input mode="...">...</user_input> tags
// to make session names and prompts clean for display.
func cleanClinePrompt(p string) string {
	trimmed := strings.TrimSpace(p)
	if m := clineUserInputRe.FindStringSubmatch(trimmed); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return trimmed
}

// parseClineTimestamp parses ISO 8601 / RFC 3339 timestamps from Cline metadata.
func parseClineTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// parseClineSession parses a single Cline session from its metadata file
// (<sessionId>.json) and transcript file (<sessionId>.messages.json).
func parseClineSession(
	metaPath string,
	projectHint string,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading cline metadata: %w", err)
	}

	var meta clineSessionMetadata
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return nil, nil, fmt.Errorf("parsing cline metadata: %w", err)
	}

	cleanMetaPath := filepath.Clean(metaPath)
	sessionDir := filepath.Dir(cleanMetaPath)
	canonicalID := filepath.Base(sessionDir)
	if !ValidClineSessionID(canonicalID) {
		return nil, nil, fmt.Errorf("invalid cline session directory name: %q", canonicalID)
	}
	baseName := strings.TrimSuffix(filepath.Base(cleanMetaPath), ".json")
	if baseName != canonicalID {
		return nil, nil, fmt.Errorf("cline metadata filename %q does not match directory %q", filepath.Base(cleanMetaPath), canonicalID)
	}
	if meta.SessionID != "" && meta.SessionID != canonicalID {
		return nil, nil, fmt.Errorf("cline metadata session_id %q does not match canonical id %q", meta.SessionID, canonicalID)
	}

	sessionID := string(AgentCline) + ":" + canonicalID
	model := meta.Model
	provider := meta.Provider

	cwd := meta.Cwd
	if cwd == "" {
		cwd = meta.WorkspaceRoot
	}

	project := projectHint
	if cwd != "" {
		if p := ExtractProjectFromCwd(cwd); p != "" {
			project = p
		}
	}

	var startedAt time.Time
	if t, ok := parseClineTimestamp(meta.StartedAt); ok {
		startedAt = t
	}

	messagesPath := filepath.Join(sessionDir, canonicalID+".messages.json")

	parsedMessages, peakCtx, maxTS, err := parseClineMessages(messagesPath, model, provider)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("parsing cline messages: %w", err)
	}

	endedAt := maxTS
	if t, ok := parseClineTimestamp(meta.EndedAt); ok {
		if endedAt.IsZero() || t.After(endedAt) {
			endedAt = t
		}
	}
	for _, msg := range parsedMessages {
		if msg.Timestamp.After(endedAt) {
			endedAt = msg.Timestamp
		}
	}
	if startedAt.IsZero() {
		if len(parsedMessages) > 0 {
			startedAt = parsedMessages[0].Timestamp
		} else if info, err := os.Stat(metaPath); err == nil {
			startedAt = info.ModTime()
		}
	}
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	userMsgCount := 0
	firstMsg := ""
	for _, msg := range parsedMessages {
		if msg.Role == RoleUser && !msg.IsSystem && strings.TrimSpace(msg.Content) != "" {
			userMsgCount++
			if firstMsg == "" {
				firstMsg = truncate(strings.ReplaceAll(msg.Content, "\n", " "), 300)
			}
		}
	}

	cleanedPrompt := cleanClinePrompt(meta.Prompt)
	if firstMsg == "" {
		firstMsg = truncate(strings.ReplaceAll(cleanedPrompt, "\n", " "), 300)
	}

	sessionName := cleanClinePrompt(meta.Metadata.Title)
	if sessionName == "" {
		sessionName = cleanedPrompt
	}
	if sessionName == "" {
		sessionName = firstMsg
	}
	if len(sessionName) > 80 {
		sessionName = truncate(sessionName, 77)
	}
	if sessionName == "" {
		sessionName = projectHint
	}

	info, err := os.Stat(metaPath)
	fileInfo := FileInfo{Path: metaPath}
	if err == nil {
		fileInfo.Size = info.Size()
		fileInfo.Mtime = info.ModTime().UnixNano()
	}

	if msgInfo, err := os.Stat(messagesPath); err == nil {
		fileInfo.Size += msgInfo.Size()
		if msgMtime := msgInfo.ModTime().UnixNano(); msgMtime > fileInfo.Mtime {
			fileInfo.Mtime = msgMtime
		}
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentCline,
		Cwd:              cwd,
		FirstMessage:     firstMsg,
		SessionName:      sessionName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(parsedMessages),
		UserMessageCount: userMsgCount,
		SourceSessionID:  meta.SessionID,
		SourceVersion:    "cline-session-v1",
		File:             fileInfo,
	}

	if meta.Metadata.Git != nil && meta.Metadata.Git.Branch != "" {
		sess.GitBranch = meta.Metadata.Git.Branch
	}

	usage := meta.Metadata.Usage
	if usage == nil {
		usage = meta.Metadata.AggregateUsage
	}

	hasMessageUsage := false
	for _, m := range parsedMessages {
		if len(m.TokenUsage) > 0 {
			hasMessageUsage = true
			break
		}
	}

	if hasMessageUsage {
		accumulateMessageTokenUsage(sess, parsedMessages)
		if usage != nil && usage.OutputTokens > sess.TotalOutputTokens {
			sess.TotalOutputTokens = usage.OutputTokens
			sess.HasTotalOutputTokens = true
		}
	} else {
		if peakCtx > 0 {
			sess.PeakContextTokens = peakCtx
			sess.HasPeakContextTokens = true
		}
		if usage != nil && usage.OutputTokens > 0 {
			sess.TotalOutputTokens = usage.OutputTokens
			sess.HasTotalOutputTokens = true
		}
		sess.aggregateTokenPresenceKnown = sess.HasTotalOutputTokens || sess.HasPeakContextTokens
	}

	sess.TerminationStatus = classifyClineTermination(meta.Status, parsedMessages)

	var costVal *jsontext.Value
	if usage != nil && usage.TotalCost != nil {
		costVal = usage.TotalCost
	} else if meta.Metadata.TotalCost != nil {
		costVal = meta.Metadata.TotalCost
	}

	var parsedCost *money.Money
	if costVal != nil {
		cost, err := money.ParseDollars(string(*costVal))
		if err != nil {
			return nil, nil, fmt.Errorf("parsing Cline total cost: %w", err)
		}
		if cost.Microdollars < 0 {
			return nil, nil, fmt.Errorf("parsing Cline total cost: %w", money.ErrNegative)
		}
		parsedCost = &cost
	}

	// Emit aggregate usage event for pricing:
	// - When per-message token metrics exist, emit only if an authoritative total cost is reported,
	//   omitting token counts so message tokens are not double-counted.
	// - When no per-message token metrics exist, emit whenever aggregate token usage or total cost exists.
	if (!hasMessageUsage && usage != nil) || parsedCost != nil {
		event := ParsedUsageEvent{
			SessionID:  sessionID,
			Source:     "session",
			Model:      model,
			ProviderID: provider,
			OccurredAt: func() string {
				if !endedAt.IsZero() {
					return endedAt.Format(time.RFC3339Nano)
				}
				return startedAt.Format(time.RFC3339Nano)
			}(),
			DedupKey: "session:" + sessionID,
			Cost:     parsedCost,
		}
		if !hasMessageUsage && usage != nil {
			if usage.InputTokens > 0 {
				event.InputTokens = usage.InputTokens
			}
			if usage.OutputTokens > 0 {
				event.OutputTokens = usage.OutputTokens
			}
			if usage.CacheReadTokens > 0 {
				event.CacheReadInputTokens = usage.CacheReadTokens
			}
			if usage.CacheWriteTokens > 0 {
				event.CacheCreationInputTokens = usage.CacheWriteTokens
			}
		}
		sess.UsageEvents = []ParsedUsageEvent{event}
	}

	return sess, parsedMessages, nil
}

// parseClineMessages reads and parses <sessionId>.messages.json into ParsedMessages.
func parseClineMessages(
	path string,
	defaultModel string,
	defaultProvider string,
) ([]ParsedMessage, int, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, time.Time{}, err
	}

	var rawFile clineMessagesFile
	if err := json.Unmarshal(data, &rawFile); err != nil {
		// Fallback: try unmarshaling as raw slice of messages
		var slice []clineRawMessage
		if err2 := json.Unmarshal(data, &slice); err2 != nil {
			return nil, 0, time.Time{}, fmt.Errorf("parsing cline messages: %w", err)
		}
		rawFile.Messages = slice
	}

	parsedMessages := make([]ParsedMessage, 0, len(rawFile.Messages))
	ordinal := 0
	peakCtx := 0
	var maxTS time.Time

	// Map of tool_use_id -> pointer to ParsedToolCall inside parsedMessages
	pendingToolCalls := make(map[string]*ParsedToolCall)

	for _, rawMsg := range rawFile.Messages {
		ts := time.UnixMilli(rawMsg.Timestamp)
		if ts.After(maxTS) {
			maxTS = ts
		}

		model := defaultModel
		provider := defaultProvider
		if rawMsg.ModelInfo != nil {
			if rawMsg.ModelInfo.ID != "" {
				model = rawMsg.ModelInfo.ID
			}
			if rawMsg.ModelInfo.Provider != "" {
				provider = rawMsg.ModelInfo.Provider
			}
		}

		var textParts []string
		var thinkingParts []string
		var toolCalls []ParsedToolCall
		var toolResults []ParsedToolResult

		for _, block := range rawMsg.Content {
			switch block.Type {
			case "text":
				t := strings.TrimSpace(block.Text)
				if t != "" {
					textParts = append(textParts, t)
				}
			case "thinking":
				th := strings.TrimSpace(block.Thinking)
				if th != "" {
					thinkingParts = append(thinkingParts, th)
				}
			case "tool_use":
				tc := ParsedToolCall{
					ToolUseID: block.ID,
					ToolName:  block.Name,
					Category:  NormalizeToolCategory(block.Name),
					InputJSON: string(block.Input),
				}
				if block.Name == "skills" || block.Name == "skill" || block.Name == "Skill" {
					var inputMap map[string]any
					if json.Unmarshal(block.Input, &inputMap) == nil {
						if sk, ok := inputMap["skill"].(string); ok && sk != "" {
							tc.SkillName = sk
						} else if name, ok := inputMap["name"].(string); ok && name != "" {
							tc.SkillName = name
						}
					}
				} else {
					tc.SkillName = inferToolSkillName(context.Background(), block.Name, tc.InputJSON)
				}
				toolCalls = append(toolCalls, tc)
			case "tool_result":
				textContent, hasErr := parseClineToolResultContent(block.Content)
				isErr := block.IsError || hasErr

				tr := ParsedToolResult{
					ToolUseID:     block.ToolUseID,
					ContentLength: len(textContent),
					ContentRaw:    string(block.Content),
				}
				toolResults = append(toolResults, tr)

				// Pair with preceding tool call
				if target, ok := pendingToolCalls[block.ToolUseID]; ok {
					status := "completed"
					if isErr {
						status = "errored"
					}
					target.ResultEvents = append(target.ResultEvents, ParsedToolResultEvent{
						Status:    status,
						Content:   textContent,
						Timestamp: ts,
					})
				}
			}
		}

		content := strings.TrimSpace(strings.Join(textParts, "\n\n"))
		thinking := strings.TrimSpace(strings.Join(thinkingParts, "\n\n"))

		if ordinal == 0 && rawMsg.Role == "user" && content != "" {
			content = cleanClinePrompt(content)
		}

		// Skip empty messages with no content, thinking, or tool calls/results
		if content == "" && thinking == "" && len(toolCalls) == 0 && len(toolResults) == 0 {
			continue
		}

		role := RoleUser
		if rawMsg.Role == "assistant" {
			role = RoleAssistant
		}

		msgProvider := ""
		if role == RoleAssistant {
			msgProvider = provider
		}

		msg := ParsedMessage{
			Ordinal:       ordinal,
			Role:          role,
			Content:       content,
			ThinkingText:  thinking,
			Timestamp:     ts,
			HasThinking:   thinking != "",
			HasToolUse:    len(toolCalls) > 0,
			ContentLength: len(content),
			ToolCalls:     toolCalls,
			ToolResults:   toolResults,
			Model:         model,
			ProviderID:    msgProvider,
			SourceUUID:    rawMsg.ID,
		}

		if role == RoleUser && len(toolResults) > 0 && content == "" {
			msg.IsSystem = true
			msg.SourceSubtype = SourceSubtypeToolResult
		}

		if role == RoleAssistant && rawMsg.Metrics != nil {
			hasTokens := rawMsg.Metrics.InputTokens > 0 ||
				rawMsg.Metrics.OutputTokens > 0 ||
				rawMsg.Metrics.CacheReadTokens > 0 ||
				rawMsg.Metrics.CacheWriteTokens > 0

			ctx := rawMsg.Metrics.InputTokens + rawMsg.Metrics.CacheReadTokens + rawMsg.Metrics.CacheWriteTokens
			if ctx > 0 {
				msg.ContextTokens = ctx
				msg.HasContextTokens = true
				if ctx > peakCtx {
					peakCtx = ctx
				}
			}
			if rawMsg.Metrics.OutputTokens > 0 {
				msg.OutputTokens = rawMsg.Metrics.OutputTokens
				msg.HasOutputTokens = true
			}

			if hasTokens {
				normalized := map[string]int{
					"input_tokens":                rawMsg.Metrics.InputTokens,
					"output_tokens":               rawMsg.Metrics.OutputTokens,
					"cache_read_input_tokens":     rawMsg.Metrics.CacheReadTokens,
					"cache_creation_input_tokens": rawMsg.Metrics.CacheWriteTokens,
				}
				if j, err := json.Marshal(normalized, json.Deterministic(true)); err == nil {
					msg.TokenUsage = j
				}
			}
		}

		parsedMessages = append(parsedMessages, msg)

		// Register tool calls in pending map for later pairing
		if len(toolCalls) > 0 {
			targetMsg := &parsedMessages[len(parsedMessages)-1]
			for ci := range targetMsg.ToolCalls {
				tc := &targetMsg.ToolCalls[ci]
				if tc.ToolUseID != "" {
					pendingToolCalls[tc.ToolUseID] = tc
				}
			}
		}

		ordinal++
	}

	return parsedMessages, peakCtx, maxTS, nil
}

// parseClineToolResultContent extracts the text content and error indication from
// a Cline tool_result content field (which can be a JSON string, an array of content/result
// blocks, or a single object).
func parseClineToolResultContent(raw jsontext.Value) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return strings.TrimSpace(str), false
	}

	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err == nil {
		var parts []string
		hasError := false
		for _, item := range items {
			if s, ok := item["success"].(bool); ok && !s {
				hasError = true
			}
			if isErr, ok := item["is_error"].(bool); ok && isErr {
				hasError = true
			}
			if t, ok := item["type"].(string); ok && t == "error" {
				hasError = true
			}
			if errVal, ok := item["error"].(string); ok && errVal != "" {
				hasError = true
				parts = append(parts, errVal)
			}
			if res, ok := item["result"].(string); ok && res != "" {
				parts = append(parts, res)
			}
			if txt, ok := item["text"].(string); ok && txt != "" {
				parts = append(parts, txt)
			}
			if cnt, ok := item["content"].(string); ok && cnt != "" {
				parts = append(parts, cnt)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n")), hasError
	}

	var item map[string]any
	if err := json.Unmarshal(raw, &item); err == nil {
		var parts []string
		hasError := false
		if s, ok := item["success"].(bool); ok && !s {
			hasError = true
		}
		if isErr, ok := item["is_error"].(bool); ok && isErr {
			hasError = true
		}
		if t, ok := item["type"].(string); ok && t == "error" {
			hasError = true
		}
		if errVal, ok := item["error"].(string); ok && errVal != "" {
			hasError = true
			parts = append(parts, errVal)
		}
		if res, ok := item["result"].(string); ok && res != "" {
			parts = append(parts, res)
		}
		if txt, ok := item["text"].(string); ok && txt != "" {
			parts = append(parts, txt)
		}
		if cnt, ok := item["content"].(string); ok && cnt != "" {
			parts = append(parts, cnt)
		}
		return strings.TrimSpace(strings.Join(parts, "\n")), hasError
	}

	return strings.TrimSpace(string(raw)), false
}

var clineTerminalTools = map[string]bool{
	"attempt_completion": true,
}

// clineLastAssistantEndsWithTerminalTool reports whether the final
// non-system assistant message ends on a terminal tool call (attempt_completion).
// Such sessions are clean completions: the trailing tool call is the agent's
// explicit completion signal, not an interrupted call awaiting a result.
func clineLastAssistantEndsWithTerminalTool(messages []ParsedMessage) bool {
	for _, v := range slices.Backward(messages) {
		m := v
		if m.IsSystem {
			continue
		}
		if m.Role != RoleAssistant {
			return false
		}
		if len(m.ToolCalls) == 0 {
			return false
		}
		return clineTerminalTools[m.ToolCalls[len(m.ToolCalls)-1].ToolName]
	}
	return false
}

// classifyClineTermination classifies session termination from the metadata status
// and transcript messages.
func classifyClineTermination(
	status string,
	messages []ParsedMessage,
) TerminationStatus {
	if len(messages) == 0 {
		return ""
	}
	if clineLastAssistantEndsWithTerminalTool(messages) {
		if status == "failed" || status == "error" {
			return TerminationTruncated
		}
		return TerminationClean
	}
	if hasOrphanedToolCall(messages) {
		return TerminationToolCallPending
	}
	if clineLastMessageIsThinkingOnly(messages) {
		return TerminationToolCallPending
	}
	switch status {
	case "failed", "error":
		return TerminationTruncated
	case "completed":
		return TerminationClean
	}
	return ""
}

// clineLastMessageIsThinkingOnly checks if the last assistant message contains
// only thinking text without any content or tool calls.
func clineLastMessageIsThinkingOnly(messages []ParsedMessage) bool {
	for _, v := range slices.Backward(messages) {
		m := v
		if m.IsSystem {
			continue
		}
		if m.Role != RoleAssistant {
			return false
		}
		if !m.HasThinking {
			return false
		}
		if len(m.ToolCalls) > 0 {
			return false
		}
		return strings.TrimSpace(m.Content) == "" || IsThinkingOnlyContent(m.Content)
	}
	return false
}

// clineFingerprintSource computes a composite fingerprint from <sessionId>.json
// and <sessionId>.messages.json for freshness detection.
func clineFingerprintSource(path string) (SourceFingerprint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}

	fp := SourceFingerprint{
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}

	h := sha256.New()
	if err := addSiblingMetadataFingerprintPart(h, "metadata", path, info); err != nil {
		return SourceFingerprint{}, err
	}

	sessionDir := filepath.Dir(path)
	sessionID := filepath.Base(sessionDir)
	msgPath := filepath.Join(sessionDir, sessionID+".messages.json")
	msgInfo, err := siblingMetadataFileInfo(msgPath)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if msgInfo != nil {
		fp.Size += msgInfo.Size()
		if ts := msgInfo.ModTime().UnixNano(); ts > fp.MTimeNS {
			fp.MTimeNS = ts
		}
		if err := addSiblingMetadataFingerprintPart(h, "messages", msgPath, msgInfo); err != nil {
			return SourceFingerprint{}, err
		}
	}

	fp.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fp, nil
}
