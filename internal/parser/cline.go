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
	"sort"
	"strconv"
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
	Version   int               `json:"version,omitempty"`
	UpdatedAt string            `json:"updated_at,omitempty"`
	Agent     string            `json:"agent,omitempty"`
	SessionID string            `json:"sessionId,omitempty"`
	TaskType  string            `json:"taskType,omitempty"`
	Origin    *clineOriginObj   `json:"origin,omitempty"`
	Messages  []clineRawMessage `json:"messages"`
}

type clineOriginObj struct {
	Source         string `json:"source,omitempty"`
	Mode           string `json:"mode,omitempty"`
	SessionID      string `json:"sessionId,omitempty"`
	ParentThreadID string `json:"parentThreadId,omitempty"`
	Subagent       string `json:"subagent,omitempty"`
	Version        string `json:"version,omitempty"`
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
	results, err := parseClineSessionWithTeammates(metaPath, projectHint, machine)
	if err != nil {
		return nil, nil, err
	}
	if len(results) == 0 {
		return nil, nil, nil
	}
	return &results[0].Session, results[0].Messages, nil
}

// parseClineSessionWithTeammates parses a Cline session and any sibling teammate
// subagent transcripts into ParseResults.
func parseClineSessionWithTeammates(
	metaPath string,
	projectHint string,
	machine string,
) ([]ParseResult, error) {
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("reading cline metadata: %w", err)
	}

	var meta clineSessionMetadata
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return nil, fmt.Errorf("parsing cline metadata: %w", err)
	}

	cleanMetaPath := filepath.Clean(metaPath)
	sessionDir := filepath.Dir(cleanMetaPath)
	canonicalID := filepath.Base(sessionDir)
	if !ValidClineSessionID(canonicalID) {
		return nil, fmt.Errorf("invalid cline session directory name: %q", canonicalID)
	}
	baseName := strings.TrimSuffix(filepath.Base(cleanMetaPath), ".json")
	if baseName != canonicalID {
		return nil, fmt.Errorf("cline metadata filename %q does not match directory %q", filepath.Base(cleanMetaPath), canonicalID)
	}
	if meta.SessionID != "" && meta.SessionID != canonicalID {
		return nil, fmt.Errorf("cline metadata session_id %q does not match canonical id %q", meta.SessionID, canonicalID)
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
		return nil, fmt.Errorf("parsing cline messages: %w", err)
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
		} else if info, err := os.Lstat(metaPath); err == nil && info.Mode().IsRegular() {
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

	info, err := os.Lstat(metaPath)
	fileInfo := FileInfo{Path: metaPath}
	if err == nil && info.Mode().IsRegular() {
		fileInfo.Size = info.Size()
		fileInfo.Mtime = info.ModTime().UnixNano()
	}

	if msgInfo, err := os.Lstat(messagesPath); err == nil && msgInfo.Mode().IsRegular() {
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
			return nil, fmt.Errorf("parsing Cline total cost: %w", err)
		}
		if cost.Microdollars < 0 {
			return nil, fmt.Errorf("parsing Cline total cost: %w", money.ErrNegative)
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

	teammates, agentMap, err := parseClineTeammates(sessionDir, canonicalID, sess, model, provider)
	if err != nil {
		return nil, fmt.Errorf("parsing cline teammates: %w", err)
	}
	annotateClineSubagentCalls(parsedMessages, agentMap)

	parentResult := ParseResult{
		Session:     *sess,
		Messages:    parsedMessages,
		UsageEvents: sess.UsageEvents,
	}

	results := make([]ParseResult, 0, 1+len(teammates))
	results = append(results, parentResult)
	results = append(results, teammates...)
	return results, nil
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

	parsedMessages, peakCtx, maxTS := parseClineRawMessages(rawFile.Messages, defaultModel, defaultProvider)
	return parsedMessages, peakCtx, maxTS, nil
}

// parseClineRawMessages parses raw Cline messages into ParsedMessages,
// computing peak context tokens and maximum timestamp.
func parseClineRawMessages(
	rawMessages []clineRawMessage,
	defaultModel string,
	defaultProvider string,
) ([]ParsedMessage, int, time.Time) {
	parsedMessages := make([]ParsedMessage, 0, len(rawMessages))
	ordinal := 0
	peakCtx := 0
	var maxTS time.Time

	// Map of tool_use_id -> pointer to ParsedToolCall inside parsedMessages
	pendingToolCalls := make(map[string]*ParsedToolCall)

	for _, rawMsg := range rawMessages {
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

		textContent := strings.TrimSpace(strings.Join(textParts, "\n\n"))
		thinking := strings.TrimSpace(strings.Join(thinkingParts, "\n\n"))

		if ordinal == 0 && rawMsg.Role == "user" && textContent != "" {
			textContent = cleanClinePrompt(textContent)
		}

		// Skip empty messages with no content, thinking, or tool calls/results
		if textContent == "" && thinking == "" && len(toolCalls) == 0 && len(toolResults) == 0 {
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

		// If an assistant message contains thinking along with tool calls,
		// emit thinking as its own message first so tool grouping does not
		// hide it in the UI (matching RooCode and Codebuff).
		if role == RoleAssistant && thinking != "" && len(toolCalls) > 0 {
			thinkingUUID := rawMsg.ID
			if thinkingUUID != "" {
				thinkingUUID += ":thinking"
			}
			parsedMessages = append(parsedMessages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       "[Thinking]\n" + thinking + "\n[/Thinking]",
				ThinkingText:  thinking,
				Timestamp:     ts,
				HasThinking:   true,
				ContentLength: len(thinking),
				Model:         model,
				ProviderID:    msgProvider,
				SourceUUID:    thinkingUUID,
			})
			ordinal++
			thinking = ""
		}

		content := textContent
		if thinking != "" {
			if content != "" {
				content = "[Thinking]\n" + thinking + "\n[/Thinking]\n\n" + content
			} else {
				content = "[Thinking]\n" + thinking + "\n[/Thinking]"
			}
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

	return parsedMessages, peakCtx, maxTS
}

// isValidClineTeammateSubagentName reports whether name is a safe, valid subagent
// identifier. It must satisfy ValidClineSessionID and must not contain double
// underscores ("__") which would collide with session ID delimiters.
func isValidClineTeammateSubagentName(name string) bool {
	if name == "" {
		return false
	}
	if !ValidClineSessionID(name) {
		return false
	}
	if strings.Contains(name, "__") {
		return false
	}
	return true
}

// isMessagePrefixSuperset reports whether longer has shorter as an exact prefix of its message IDs.
// Both slices must be non-empty, and len(longer) >= len(shorter).
func isMessagePrefixSuperset(shorter, longer []string) bool {
	if len(shorter) == 0 || len(longer) < len(shorter) {
		return false
	}
	for i := range shorter {
		if shorter[i] != longer[i] {
			return false
		}
	}
	return true
}

type clineTeammateCandidate struct {
	filename     string
	path         string
	info         os.FileInfo
	rawFile      clineMessagesFile
	rawSessionID string
	subagent     string
	msgIDs       []string
	maxTS        time.Time
	updatedAt    time.Time
	mtime        int64
}

// isBetterTeammateCandidate returns true if candidate a is strictly more authoritative
// than candidate b according to the tiebreak hierarchy:
// 1. More message IDs (longer transcript)
// 2. Later max message timestamp (monotonically advances across continuations)
// 3. Later updated_at timestamp
// 4. Later file mtime
// 5. Lexicographical filename tiebreaker (deterministic)
func isBetterTeammateCandidate(a, b *clineTeammateCandidate) bool {
	if len(a.msgIDs) != len(b.msgIDs) {
		return len(a.msgIDs) > len(b.msgIDs)
	}
	if !a.maxTS.Equal(b.maxTS) {
		return a.maxTS.After(b.maxTS)
	}
	if !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.After(b.updatedAt)
	}
	if a.mtime != b.mtime {
		return a.mtime > b.mtime
	}
	return a.filename > b.filename
}

// parseClineTeammates discovers and parses teammate subagent transcripts under sessionDir.
// Continuations of the same subagent (sharing an exact message ID prefix) are coalesced
// into a single authoritative session under a stable ID (cline:<parent>__teammate__<subagent>),
// while distinct runs (e.g. non-continuation restarts) remain separate sessions.
func parseClineTeammates(
	sessionDir string,
	parentSessionID string,
	parentSess *ParsedSession,
	parentModel string,
	parentProvider string,
) ([]ParseResult, map[string]string, error) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading cline session directory %s: %w", sessionDir, err)
	}

	bySubagent := make(map[string][]*clineTeammateCandidate)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		if !IsClineTeammateMessagesFile(parentSessionID, filename) {
			continue
		}
		teammatePath := filepath.Join(sessionDir, filename)
		info, err := os.Lstat(teammatePath)
		if err != nil {
			// Fail-closed on unreadable teammate files: unlike clineEffectiveStat
			// (which performs best-effort stat skips to avoid stalling background
			// sync loops), parseClineTeammates must fail closed on unexpected I/O
			// errors so that transient read errors do not cause active subagents,
			// their token usage, or parent tool bindings to be silently dropped.
			return nil, nil, fmt.Errorf("stat cline teammate %s: %w", teammatePath, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}

		data, err := os.ReadFile(teammatePath)
		if err != nil {
			// Fail-closed on unreadable teammate transcript: ensures transient
			// read errors do not silently drop active subagent sessions.
			return nil, nil, fmt.Errorf("reading cline teammate %s: %w", teammatePath, err)
		}

		var rawFile clineMessagesFile
		if err := json.Unmarshal(data, &rawFile); err != nil {
			return nil, nil, fmt.Errorf("parsing cline teammate %s: %w", teammatePath, err)
		}

		rawSessionID := rawFile.SessionID
		if rawSessionID == "" && rawFile.Origin != nil {
			rawSessionID = rawFile.Origin.SessionID
		}
		if rawSessionID == "" {
			taskSuffix := strings.TrimSuffix(filename, ".messages.json")
			rawSessionID = parentSessionID + "__teamtask__" + taskSuffix
		}

		subagent := ""
		if rawFile.Origin != nil && rawFile.Origin.Subagent != "" {
			if !isValidClineTeammateSubagentName(rawFile.Origin.Subagent) {
				continue
			}
			subagent = rawFile.Origin.Subagent
		} else {
			parts := strings.Split(strings.TrimSuffix(filename, ".messages.json"), "__")
			if len(parts) == 0 || !isValidClineTeammateSubagentName(parts[0]) {
				continue
			}
			subagent = parts[0]
		}

		msgIDs := make([]string, 0, len(rawFile.Messages))
		var maxTS time.Time
		for _, m := range rawFile.Messages {
			if m.ID != "" {
				msgIDs = append(msgIDs, m.ID)
			}
			if m.Timestamp > 0 {
				t := time.UnixMilli(m.Timestamp)
				if t.After(maxTS) {
					maxTS = t
				}
			}
		}

		var updatedAt time.Time
		if rawFile.UpdatedAt != "" {
			if t, ok := parseClineTimestamp(rawFile.UpdatedAt); ok {
				updatedAt = t
			}
		}

		cand := &clineTeammateCandidate{
			filename:     filename,
			path:         teammatePath,
			info:         info,
			rawFile:      rawFile,
			rawSessionID: rawSessionID,
			subagent:     subagent,
			msgIDs:       msgIDs,
			maxTS:        maxTS,
			updatedAt:    updatedAt,
			mtime:        info.ModTime().UnixNano(),
		}
		bySubagent[subagent] = append(bySubagent[subagent], cand)
	}

	var teammates []ParseResult
	agentMap := make(map[string]string)

	subagents := make([]string, 0, len(bySubagent))
	for sa := range bySubagent {
		subagents = append(subagents, sa)
	}
	sort.Strings(subagents)

	for _, sa := range subagents {
		cands := bySubagent[sa]
		sort.SliceStable(cands, func(i, j int) bool {
			return isBetterTeammateCandidate(cands[i], cands[j])
		})

		var winners []*clineTeammateCandidate
		superseded := make(map[*clineTeammateCandidate]bool)

		for _, cand := range cands {
			if superseded[cand] {
				continue
			}
			winners = append(winners, cand)

			for _, other := range cands {
				if other == cand || superseded[other] {
					continue
				}
				if len(other.msgIDs) == 0 {
					superseded[other] = true
				} else if isMessagePrefixSuperset(other.msgIDs, cand.msgIDs) {
					superseded[other] = true
				}
			}
		}

		for runIdx, winner := range winners {
			stableID := parentSessionID + "__teammate__" + sa
			if runIdx > 0 {
				stableID = parentSessionID + "__teammate__" + sa + "__run" + strconv.Itoa(runIdx+1)
			}
			fullSessionID := string(AgentCline) + ":" + stableID

			parsedMessages, peakCtx, maxTS := parseClineRawMessages(winner.rawFile.Messages, parentModel, parentProvider)

			var startedAt time.Time
			if len(parsedMessages) > 0 && !parsedMessages[0].Timestamp.IsZero() {
				startedAt = parsedMessages[0].Timestamp
			} else {
				startedAt = winner.info.ModTime()
			}

			endedAt := maxTS
			if !winner.updatedAt.IsZero() && (endedAt.IsZero() || winner.updatedAt.After(endedAt)) {
				endedAt = winner.updatedAt
			}
			for _, msg := range parsedMessages {
				if msg.Timestamp.After(endedAt) {
					endedAt = msg.Timestamp
				}
			}
			if endedAt.IsZero() {
				endedAt = startedAt
			}

			firstMsg := ""
			userCount := 0
			for _, msg := range parsedMessages {
				if msg.Role == RoleUser && !msg.IsSystem && strings.TrimSpace(msg.Content) != "" {
					userCount++
					if firstMsg == "" {
						firstMsg = truncate(strings.ReplaceAll(msg.Content, "\n", " "), 300)
					}
				}
			}

			sessionName := "Teammate: " + sa

			parentFullID := string(AgentCline) + ":" + parentSessionID
			if winner.rawFile.Origin != nil && winner.rawFile.Origin.ParentThreadID != "" {
				parentFullID = string(AgentCline) + ":" + winner.rawFile.Origin.ParentThreadID
			}

			fileInfo := FileInfo{
				Path:  winner.path,
				Size:  winner.info.Size(),
				Mtime: winner.info.ModTime().UnixNano(),
			}

			subSess := &ParsedSession{
				ID:                fullSessionID,
				Project:           parentSess.Project,
				Machine:           parentSess.Machine,
				Agent:             AgentCline,
				Cwd:               parentSess.Cwd,
				GitBranch:         parentSess.GitBranch,
				ParentSessionID:   parentFullID,
				RelationshipType:  RelSubagent,
				FirstMessage:      firstMsg,
				SessionName:       sessionName,
				StartedAt:         startedAt,
				EndedAt:           endedAt,
				MessageCount:      len(parsedMessages),
				UserMessageCount:  userCount,
				SourceSessionID:   winner.rawSessionID,
				SourceVersion:     "cline-session-v1",
				File:              fileInfo,
				TerminationStatus: classifyClineTermination("", parsedMessages),
			}

			hasMessageUsage := false
			for _, m := range parsedMessages {
				if len(m.TokenUsage) > 0 {
					hasMessageUsage = true
					break
				}
			}
			if hasMessageUsage {
				accumulateMessageTokenUsage(subSess, parsedMessages)
			} else if peakCtx > 0 {
				subSess.PeakContextTokens = peakCtx
				subSess.HasPeakContextTokens = true
				subSess.aggregateTokenPresenceKnown = true
			}

			if runIdx == 0 {
				agentMap[sa] = fullSessionID
			}

			teammates = append(teammates, ParseResult{
				Session:     *subSess,
				Messages:    parsedMessages,
				UsageEvents: subSess.UsageEvents,
			})
		}
	}

	return teammates, agentMap, nil
}

// extractClineAgentID extracts the agentId from a Cline tool call input JSON.
func extractClineAgentID(inputJSON string) string {
	if inputJSON == "" {
		return ""
	}
	var input struct {
		AgentID string `json:"agentId"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err == nil && input.AgentID != "" {
		return input.AgentID
	}
	return ""
}

// annotateClineSubagentCalls annotates tool calls representing teammate
// invocations with the child subagent's session ID. Only task execution
// tool calls (team_run_task) are linked to the subagent session; teammate
// spawn/definition calls (team_spawn_teammate) register the teammate record
// but do not represent an execution run.
func annotateClineSubagentCalls(msgs []ParsedMessage, agentMap map[string]string) {
	if len(agentMap) == 0 {
		return
	}
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			tc := &msgs[i].ToolCalls[j]
			if tc.ToolName == "team_run_task" {
				agentID := extractClineAgentID(tc.InputJSON)
				if agentID != "" {
					if sid, ok := agentMap[agentID]; ok {
						tc.SubagentSessionID = sid
						for k := range tc.ResultEvents {
							tc.ResultEvents[k].SubagentSessionID = sid
							tc.ResultEvents[k].AgentID = agentID
						}
					}
				}
			}
		}
	}
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

// hasClineOrphanedToolCall reports whether the last assistant message has
// any tool_use blocks that lack a matching tool_result or completion event,
// special-casing terminal tools (such as attempt_completion) as resolved.
func hasClineOrphanedToolCall(messages []ParsedMessage) bool {
	if len(messages) == 0 {
		return false
	}
	lastAssistantIdx := -1
	for i, v := range slices.Backward(messages) {
		if v.IsSystem {
			continue
		}
		if v.Role == RoleAssistant {
			lastAssistantIdx = i
			break
		}
	}
	if lastAssistantIdx == -1 {
		return false
	}
	last := messages[lastAssistantIdx]
	if len(last.ToolCalls) == 0 {
		return false
	}

	resolved := make(map[string]bool)
	for _, m := range messages[lastAssistantIdx+1:] {
		for _, tr := range m.ToolResults {
			if tr.ToolUseID != "" {
				resolved[tr.ToolUseID] = true
			}
		}
	}
	for i := range last.ToolCalls {
		tc := &last.ToolCalls[i]
		if tc.ToolUseID == "" {
			continue
		}
		// Special-case Cline terminal tools (e.g. attempt_completion)
		// as resolved since they signal normal completion without
		// requiring a follow-up user tool_result.
		if clineTerminalTools[tc.ToolName] {
			resolved[tc.ToolUseID] = true
			continue
		}
		for _, ev := range tc.ResultEvents {
			if ev.Status != "running" {
				resolved[tc.ToolUseID] = true
				break
			}
		}
	}

	for _, tc := range last.ToolCalls {
		if tc.ToolUseID != "" && !resolved[tc.ToolUseID] {
			return true
		}
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
	if hasClineOrphanedToolCall(messages) {
		return TerminationToolCallPending
	}
	if clineLastAssistantEndsWithTerminalTool(messages) {
		if status == "failed" || status == "error" {
			return TerminationTruncated
		}
		return TerminationClean
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

// clineFingerprintSource computes a composite fingerprint from <sessionId>.json,
// <sessionId>.messages.json, and any teammate *.messages.json files for freshness detection.
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

	entries, err := os.ReadDir(sessionDir)
	if err != nil && !os.IsNotExist(err) {
		return SourceFingerprint{}, fmt.Errorf("read cline session dir %s: %w", sessionDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !IsClineTeammateMessagesFile(sessionID, name) {
			continue
		}
		teammatePath := filepath.Join(sessionDir, name)
		teammateInfo, err := siblingMetadataFileInfo(teammatePath)
		if err != nil {
			return SourceFingerprint{}, err
		}
		if teammateInfo != nil {
			fp.Size += teammateInfo.Size()
			if ts := teammateInfo.ModTime().UnixNano(); ts > fp.MTimeNS {
				fp.MTimeNS = ts
			}
			if err := addSiblingMetadataFingerprintPart(h, "teammate:"+name, teammatePath, teammateInfo); err != nil {
				return SourceFingerprint{}, err
			}
		}
	}

	fp.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fp, nil
}
