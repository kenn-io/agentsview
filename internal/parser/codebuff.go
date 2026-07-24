// ABOUTME: Parses Codebuff/Freebuff chat-messages.json session files into
// ABOUTME: structured session data. Both agents share the same on-disk layout
// ABOUTME: under ~/.config/manicode/projects/<project>/chats/<timestamp>/.
// ABOUTME: The agent type (codebuff vs freebuff) is determined from the
// ABOUTME: agentType field in run-state.json.
package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// codebuffSessionDir contains the session timestamp directory path and
// the project hint derived from the parent directory name.
type codebuffSessionDir struct {
	Path        string
	ProjectHint string
}

// parseCodebuffSession parses a single codebuff/freebuff session directory
// and returns the parsed session with messages.
func parseCodebuffSession(
	dir string,
	projectHint string,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	chatMessagesPath := filepath.Join(dir, "chat-messages.json")
	runStatePath := filepath.Join(dir, "run-state.json")
	chatMetaPath := filepath.Join(dir, "chat-meta.json")

	// Read run-state.json for model, token, agent-type, and skills data.
	rs, err := readCodebuffRunState(runStatePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("read run-state %s: %w", runStatePath, err)
	}

	// Read chat-meta.json for session name and timing hints.
	meta := readCodebuffChatMeta(chatMetaPath)

	// Model name is the raw agentType from run-state.json
	// (e.g. "base2-free-deepseek", "base2-free-mimo").
	model := rs.AgentType

	// Read and parse the chat messages.
	data, err := os.ReadFile(chatMessagesPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read chat-messages %s: %w", chatMessagesPath, err)
	}
	if !gjson.ValidBytes(data) {
		return nil, nil, fmt.Errorf("decode %s: invalid json", chatMessagesPath)
	}

	// Session ID is the timestamp directory name (ISO 8601).
	sessionID := filepath.Base(dir)
	sessionDate := parseCodebuffSessionDate(sessionID)

	msgs, startedAt, endedAt := parseCodebuffMessages(data, sessionDate, model)

	// Enrich tool calls with skill names by matching against the skills
	// catalog available to this session (run-state.json.fileContext.skills).
	// Codebuff/Freebuff invoke skills through generic tool calls (e.g.
	// run_terminal_command) rather than a dedicated Skill tool, so a tool
	// call is attributed to a skill when its name or input references a
	// known skill from the catalog.
	codebuffAttachSkillNames(msgs, rs.Skills)

	// Build session name from first user prompt.
	firstMsg := ""
	for _, msg := range msgs {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			firstMsg = truncate(
				strings.ReplaceAll(msg.Content, "\n", " "),
				300,
			)
			break
		}
	}
	if firstMsg == "" && meta.FirstPrompt != "" {
		firstMsg = truncate(
			strings.ReplaceAll(meta.FirstPrompt, "\n", " "),
			300,
		)
	}

	// Session name from first prompt (better than directory name).
	sessionName := firstMsg
	if len(sessionName) > 80 {
		sessionName = sessionName[:77] + "..."
	}
	if sessionName == "" {
		if rs.Cwd != "" {
			sessionName = filepath.Base(rs.Cwd)
		} else {
			sessionName = projectHint
		}
	}

	// Determine agent type from run-state agentType field.
	// Sessions with "free" in the agentType are Freebuff, others are Codebuff.
	// Both share the same on-disk layout; the parser splits them by type
	// so the UI can filter each agent independently.
	agent := AgentCodebuff
	agentLabel := "Codebuff"
	if strings.Contains(strings.ToLower(rs.AgentType), "free") {
		agent = AgentFreebuff
		agentLabel = "Freebuff"
	}

	// Count user messages.
	userMsgCount := 0
	for _, msg := range msgs {
		if msg.Role == RoleUser && !msg.IsSystem &&
			strings.TrimSpace(msg.Content) != "" {
			userMsgCount++
		}
	}
	messageCount := len(msgs)

	// If no messages from the transcript, use meta counts.
	if messageCount == 0 {
		messageCount = meta.MessageCount
		if meta.MessageCount > 0 {
			userMsgCount = 1 // at least one user prompt
		}
	}

	// Model extracted earlier (passed into message parser above).

	// Source file identity: use chat-messages.json as the primary source.
	info, err := os.Stat(chatMessagesPath)
	fileInfo := FileInfo{
		Path: chatMessagesPath,
	}
	if err == nil {
		fileInfo.Size = info.Size()
		fileInfo.Mtime = info.ModTime().UnixNano()
	}

	fullID := string(agent) + ":" + sessionID

	// Derive project from run-state cwd or project hint.
	// Use ExtractProjectFromCwd (git-root aware) rather than
	// GetProjectName because rs.Cwd is a full absolute path, not
	// a Claude-style encoded project name.
	project := projectHint
	if rs.Cwd != "" {
		if p := ExtractProjectFromCwd(rs.Cwd); p != "" {
			project = p
		}
	}

	sess := &ParsedSession{
		ID:               fullID,
		Project:          project,
		Machine:          machine,
		Agent:            agent,
		AgentLabel:       agentLabel,
		Cwd:              rs.Cwd,
		FirstMessage:     firstMsg,
		SessionName:      sessionName,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     messageCount,
		UserMessageCount: userMsgCount,
		SourceSessionID:  sessionID,
		SourceVersion:    "codebuff-chat-v1",
		File:             fileInfo,
	}

	// Token counts from run-state.
	if rs.ContextTokenCount > 0 {
		sess.PeakContextTokens = rs.ContextTokenCount
		sess.HasPeakContextTokens = true
	}

	sess.aggregateTokenPresenceKnown =
		sess.HasTotalOutputTokens || sess.HasPeakContextTokens

	// Emit usage event with clean model name for catalog pricing.
	// Credits are billing units (1 credit = $0.01), mapped to CostUSD.
	if model != "" {
		evt := ParsedUsageEvent{
			SessionID: fullID,
			Source:    "session",
			Model:     model,
			OccurredAt: func() string {
				if !endedAt.IsZero() {
					return endedAt.Format(time.RFC3339Nano)
				}
				return startedAt.Format(time.RFC3339Nano)
			}(),
			DedupKey: "session:" + fullID,
		}
		if rs.CreditsUsed > 0 {
			cost := rs.CreditsUsed * 0.01
			evt.CostUSD = &cost
			evt.CostStatus = "reported"
			evt.CostSource = "session"
		}
		sess.UsageEvents = []ParsedUsageEvent{evt}
	}

	return sess, msgs, nil
}

// codebuffRunState holds extracted fields from run-state.json.
type codebuffRunState struct {
	AgentType         string
	ContextTokenCount int
	CreditsUsed       float64
	DirectCreditsUsed float64
	Cwd               string
	Skills            []codebuffSkill
}

// codebuffSkill is a single skill entry from the session's skill catalog
// (run-state.json sessionState.fileContext.skills). The catalog lists the
// skills available to the agent during the session.
type codebuffSkill struct {
	Name        string
	Description string
	FilePath    string
	Content     string
}

func readCodebuffRunState(path string) (codebuffRunState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return codebuffRunState{}, err
	}
	if !gjson.ValidBytes(data) {
		return codebuffRunState{}, fmt.Errorf("invalid json in %s", path)
	}

	mas := gjson.GetBytes(data, "sessionState.mainAgentState")
	rs := codebuffRunState{
		AgentType:         mas.Get("agentType").Str,
		ContextTokenCount: int(mas.Get("contextTokenCount").Int()),
		CreditsUsed:       mas.Get("creditsUsed").Float(),
		DirectCreditsUsed: mas.Get("directCreditsUsed").Float(),
		Cwd: gjson.GetBytes(data,
			"sessionState.fileContext.cwd").Str,
	}
	rs.Skills = parseCodebuffSkills(data)
	return rs, nil
}

// parseCodebuffSkills extracts the skill catalog from run-state.json
// (sessionState.fileContext.skills). The field is a JSON object keyed by
// skill name; each value carries name, description, optional content, and
// filePath. Returns an empty slice when no skills are present.
func parseCodebuffSkills(data []byte) []codebuffSkill {
	skills := gjson.GetBytes(data, "sessionState.fileContext.skills")
	if !skills.Exists() || !skills.IsObject() {
		return nil
	}
	var out []codebuffSkill
	skills.ForEach(func(key, val gjson.Result) bool {
		name := val.Get("name").Str
		if name == "" {
			name = key.Str
		}
		out = append(out, codebuffSkill{
			Name:        name,
			Description: val.Get("description").Str,
			FilePath:    val.Get("filePath").Str,
			Content:     val.Get("content").Str,
		})
		return true
	})
	return out
}

// codebuffAttachSkillNames attributes tool calls to skills from the
// session's skill catalog. Codebuff/Freebuff do not emit a dedicated Skill
// tool; skills are invoked through generic tools (e.g. run_terminal_command)
// whose input names the skill, or through a tool literally named "Skill".
// A tool call is attributed when its tool name matches a skill, or its input
// JSON references a known skill name.
func codebuffAttachSkillNames(msgs []ParsedMessage, skills []codebuffSkill) {
	if len(skills) == 0 {
		return
	}
	byName := make(map[string]struct{}, len(skills))
	for _, s := range skills {
		byName[strings.ToLower(s.Name)] = struct{}{}
	}
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			tc := &msgs[i].ToolCalls[j]
			if tc.SkillName != "" {
				continue
			}
			// Explicit Skill tool.
			if strings.EqualFold(tc.ToolName, "Skill") ||
				strings.EqualFold(tc.ToolName, "skill") {
				tc.SkillName = gjson.Get(tc.InputJSON, "skill").Str
				if tc.SkillName == "" {
					tc.SkillName = gjson.Get(tc.InputJSON, "name").Str
				}
				if tc.SkillName == "" {
					tc.SkillName = tc.ToolName
				}
				continue
			}
			// Tool name itself is a skill name.
			if _, ok := byName[strings.ToLower(tc.ToolName)]; ok {
				tc.SkillName = tc.ToolName
				continue
			}
			// Input JSON references a known skill name.
			if name := codebuffSkillNameFromInput(tc.InputJSON, byName); name != "" {
				tc.SkillName = name
			}
		}
	}
}

// codebuffSkillNameFromInput scans raw tool input JSON for a reference to a
// known skill name. It matches the skill name as a quoted JSON string value
// or as a standalone token (e.g. inside a shell command). Returns "" when no
// known skill is referenced.
func codebuffSkillNameFromInput(inputJSON string, byName map[string]struct{}) string {
	if inputJSON == "" {
		return ""
	}
	// Direct JSON string/object match on common skill-carrying keys.
	for _, key := range []string{"skill", "name", "skill_name", "command", "prompt"} {
		v := gjson.Get(inputJSON, key).Str
		if v != "" {
			if _, ok := byName[strings.ToLower(v)]; ok {
				return v
			}
		}
	}
	// Fall back to scanning for any known skill name as a whole token.
	lower := strings.ToLower(inputJSON)
	for name := range byName {
		if containsSkillToken(lower, name) {
			return name
		}
	}
	return ""
}

// containsSkillToken reports whether lower contains name as a whole
// alphanumeric token (word-boundary match). It splits lower on
// non-alphanumeric runes and compares each token against name.
// This avoids false positives from substring matching (e.g. "go"
// matching "going" or "cargo").
func containsSkillToken(lower, name string) bool {
	if name == "" {
		return false
	}
	var buf []byte
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			buf = append(buf, c)
		} else {
			if string(buf) == name {
				return true
			}
			buf = buf[:0]
		}
	}
	return string(buf) == name
}

// codebuffChatMeta holds extracted fields from chat-meta.json.
type codebuffChatMeta struct {
	MessageCount int
	FirstPrompt  string
	MessagesSize int64
}

func readCodebuffChatMeta(path string) codebuffChatMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return codebuffChatMeta{}
	}
	if !gjson.ValidBytes(data) {
		return codebuffChatMeta{}
	}
	return codebuffChatMeta{
		MessageCount: int(gjson.GetBytes(data, "messageCount").Int()),
		FirstPrompt:  gjson.GetBytes(data, "firstPrompt").Str,
		MessagesSize: gjson.GetBytes(data, "messagesSize").Int(),
	}
}

// parseCodebuffSessionDate parses the session directory name as an ISO 8601
// timestamp. The directory name format is "2026-07-16T00-09-00.236Z".
func parseCodebuffSessionDate(sessionID string) time.Time {
	// Try full ISO format with milliseconds and Z suffix.
	if ts, err := time.Parse("2006-01-02T15-04-05.999Z", sessionID); err == nil {
		return ts
	}
	// Try without milliseconds.
	if ts, err := time.Parse("2006-01-02T15-04-05Z", sessionID); err == nil {
		return ts
	}
	// Try with milliseconds, no Z. Interpret as local time since
	// codebuff records wall-clock timestamps without a UTC offset.
	if ts, err := time.ParseInLocation("2006-01-02T15-04-05.999", sessionID, time.Local); err == nil {
		return ts
	}
	// Try basic ISO date only.
	if ts, err := time.ParseInLocation("2006-01-02", sessionID, time.Local); err == nil {
		return ts
	}
	return time.Time{}
}

// parseCodebuffMessages parses chat-messages.json data into ParsedMessages.
// sessionDate provides the date context for time-only timestamps.
// model is set on every ParsedMessage so the UI can identify the model used.
func parseCodebuffMessages(
	data []byte, sessionDate time.Time, model string,
) ([]ParsedMessage, time.Time, time.Time) {
	root := gjson.ParseBytes(data)
	if !root.IsArray() {
		return nil, time.Time{}, time.Time{}
	}

	var (
		messages  []ParsedMessage
		startedAt time.Time
		endedAt   time.Time
		ordinal   int
	)

	root.ForEach(func(_, msg gjson.Result) bool {
		variant := msg.Get("variant").Str
		ts := parseCodebuffTimestamp(
			msg.Get("timestamp").Str, sessionDate,
		)

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
			parsed := parseCodebuffAIMessage(msg, ts)
			if len(parsed) == 0 {
				return true
			}
			for i := range parsed {
				parsed[i].Ordinal = ordinal
				parsed[i].Model = model
				ordinal++
			}
			messages = append(messages, parsed...)

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

	return messages, startedAt, endedAt
}

// parseCodebuffAIMessage parses an AI-variant message into one or more
// ParsedMessages. AI messages contain blocks: text (reasoning or regular),
// tool calls, and subagent invocations.
func parseCodebuffAIMessage(
	msg gjson.Result,
	ts time.Time,
) []ParsedMessage {
	blocks := msg.Get("blocks")
	if !blocks.IsArray() {
		return nil
	}

	var (
		out           []ParsedMessage
		thinkingParts []string
		textParts     []string
		toolCalls     []ParsedToolCall
		toolResults   []ParsedToolResult
		systemParts   []string
	)

	blocks.ForEach(func(_, block gjson.Result) bool {
		blockType := block.Get("type").Str

		switch blockType {
		case "text":
			textType := block.Get("textType").Str
			content := block.Get("content").Str
			switch textType {
			case "reasoning":
				if strings.TrimSpace(content) != "" {
					thinkingParts = append(thinkingParts, content)
				}
			case "text":
				if strings.TrimSpace(content) != "" {
					textParts = append(textParts, content)
				}
			default:
				if strings.TrimSpace(content) != "" {
					textParts = append(textParts, content)
				}
			}

		case "tool":
			tc := parseCodebuffToolCall(block)
			if tc != nil {
				toolCalls = append(toolCalls, *tc)
				if output := block.Get("output"); output.Exists() {
					quoted, _ := json.Marshal(output.Raw)
					toolResults = append(toolResults, ParsedToolResult{
						ToolUseID:     tc.ToolUseID,
						ContentRaw:    string(quoted),
						ContentLength: len(output.Raw),
					})
				}
			}

		case "agent":
			// Subagent invocations: capture agent type, name, and
			// spawn params as a tool call for UI display.
			agentType := block.Get("agentType").Str
			agentName := block.Get("agentName").Str
			agentID := block.Get("agentId").Str
			agentStatus := block.Get("status").Str

			// Capture spawn params as input JSON.
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
			inputJSON, _ := json.Marshal(inputParts)

			status := agentStatus
			if status == "" {
				status = "spawned"
			}

			tc := ParsedToolCall{
				ToolUseID: agentID,
				ToolName:  agentType,
				Category:  "Task",
				// SubagentSessionID is intentionally unset: codebuff/freebuff
				// subagent invocations (basher, code-searcher, etc.) are not
				// standalone agentsview-tracked sessions.
				InputJSON: string(inputJSON),
			}
			toolCalls = append(toolCalls, tc)

			// Collect subagent output text for inline display.
			if output := block.Get("content"); output.Exists() && output.Str != "" {
				prefix := agentName
				if agentType != "" {
					prefix = agentType + ":" + agentName
				}
				textParts = append(textParts,
					"["+prefix+" ("+status+")]\n"+output.Str,
				)
			}

		case "mode-divider":
			mode := block.Get("mode").Str
			if mode != "" {
				systemParts = append(systemParts, "[Mode: "+mode+"]")
			}

		case "plan":
			// Planning output from the agent. Emit as a system message
			// with the plan content.
			content := block.Get("content").Str
			if strings.TrimSpace(content) != "" {
				systemParts = append(systemParts, "[Plan]\n"+content)
			}

		case "ask-user":
			// The agent asked the user a clarifying question. Capture
			// the question text so the transcript shows what was asked.
			questions := block.Get("questions")
			if questions.IsArray() {
				questions.ForEach(func(_, q gjson.Result) bool {
					questionText := q.Get("question").Str
					if strings.TrimSpace(questionText) != "" {
						systemParts = append(systemParts,
							"[Agent asked] "+questionText)
					}
					return true
				})
			}

		case "image":
			// User-uploaded image. Record that an image was attached
			// without storing the base64 content.
			filename := block.Get("filename").Str
			if filename != "" {
				textParts = append(textParts,
					"[Image: "+filename+"]")
			} else {
				textParts = append(textParts, "[Image attached]")
			}
		}
		return true
	})

	// 1. System notes.
	if len(systemParts) > 0 {
		sysContent := strings.Join(systemParts, "\n")
		out = append(out, ParsedMessage{
			Role:          RoleSystem,
			Content:       sysContent,
			Timestamp:     ts,
			ContentLength: len(sysContent),
			IsSystem:      true,
		})
	}

	// 2. Thinking as a separate message.
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

	// 3. Text + tool calls.
	textContent := strings.Join(textParts, "\n\n")
	hasToolUse := len(toolCalls) > 0

	if textContent != "" || hasToolUse {
		out = append(out, ParsedMessage{
			Role:          RoleAssistant,
			Content:       textContent,
			Timestamp:     ts,
			HasToolUse:    hasToolUse,
			ToolCalls:     toolCalls,
			ContentLength: len(textContent),
		})
	} else if !hasToolUse && len(thinkingParts) == 0 && len(systemParts) == 0 {
		return nil
	}

	// 4. Tool results.
	for _, tr := range toolResults {
		out = append(out, ParsedMessage{
			Role:          RoleUser,
			Timestamp:     ts,
			ToolResults:   []ParsedToolResult{tr},
			ContentLength: tr.ContentLength,
		})
	}

	return out
}

// parseCodebuffToolCall extracts a ParsedToolCall from a tool block.
func parseCodebuffToolCall(block gjson.Result) *ParsedToolCall {
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

// parseCodebuffTimestamp parses a timestamp string. Codebuff/freebuff
// messages use "HH:MM PM" format with the date provided by the session
// directory name. The sessionDate carries the date context.
func parseCodebuffTimestamp(s string, sessionDate time.Time) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}

	// ISO format (used by newer builds or subagent messages).
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	if ts, err := time.Parse("2006-01-02T15:04:05.999Z07:00", s); err == nil {
		return ts
	}

	// "HH:MM PM" format: combine with the session date.
	if ts, err := time.Parse("03:04 PM", s); err == nil {
		if sessionDate.IsZero() {
			return time.Time{}
		}
		// Combine date from session with time from message.
		return time.Date(
			sessionDate.Year(),
			sessionDate.Month(),
			sessionDate.Day(),
			ts.Hour(),
			ts.Minute(),
			0, 0,
			sessionDate.Location(),
		)
	}

	return time.Time{}
}

// discoverCodebuffSessions finds all session directories under a root.
// root is the parent projects directory (~/.config/manicode/projects).
// Sessions live under <root>/<project>/chats/<timestamp>/.
func discoverCodebuffSessions(root string) []codebuffSessionDir {
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var dirs []codebuffSessionDir
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectName := project.Name()
		chatsDir := filepath.Join(root, projectName, "chats")
		entries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(chatsDir, entry.Name())
			if !IsRegularFile(filepath.Join(dir, "chat-messages.json")) {
				continue
			}
			dirs = append(dirs, codebuffSessionDir{
				Path:        dir,
				ProjectHint: projectName,
			})
		}
	}
	return dirs
}

// codebuffProjectFromPath extracts the project name from a session
// file path. The path is rooted under ~/.config/manicode/projects/.
func codebuffProjectFromPath(path string) string {
	// Path is: <root>/<project>/chats/<timestamp>/chat-messages.json
	// We want the <project> component.
	// Walk up from chat-messages.json: dir=/timestamp, parent=chats, grandparent=<project>
	dir := filepath.Dir(path)            // <timestamp>
	chatsDir := filepath.Dir(dir)        // chats
	projectDir := filepath.Dir(chatsDir) // <project>
	return filepath.Base(projectDir)
}
