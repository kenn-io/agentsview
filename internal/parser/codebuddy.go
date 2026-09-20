package parser

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

var codebuddyUserQueryRegex = regexp.MustCompile(`(?s)<user_query>(.*?)</user_query>`)

// codebuddyDefaultDirs returns platform-specific default directories holding
// CodeBuddyExtension data.
func codebuddyDefaultDirs() []string {
	return []string{
		// Windows
		"AppData/Local/CodeBuddyExtension/Data",
		// macOS
		"Library/Application Support/CodeBuddyExtension/Data",
		// Linux
		".config/CodeBuddyExtension/Data",
	}
}

// parseCodeBuddySession parses a CodeBuddy session given its manifest file at
// .../history/<ws_hash>/<session_id>/index.json.
func parseCodeBuddySession(indexPath, projectHint, machine string) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(indexPath)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", indexPath, err)
	}

	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", indexPath, err)
	}

	sessionDir := filepath.Dir(indexPath)
	sessionID := filepath.Base(sessionDir)
	wsDir := filepath.Dir(sessionDir)

	title, wsCreatedAt, modelHint := readCodeBuddyWorkspaceMetadata(wsDir, sessionID)

	indexRoot := gjson.ParseBytes(indexBytes)
	if !gjson.ValidBytes(indexBytes) {
		return nil, nil, errors.New("invalid CodeBuddy session manifest")
	}
	if !indexRoot.IsObject() || !indexRoot.Get("messages").IsArray() {
		return nil, nil, errors.New("invalid CodeBuddy session manifest: messages must be an array")
	}
	msgRefs := indexRoot.Get("messages").Array()

	var (
		messages      []ParsedMessage
		ordinal       int
		startedAt     time.Time
		endedAt       time.Time
		firstMsg      string
		cwd           string
		realUserCount int
		malformed     int
	)

	if !wsCreatedAt.IsZero() {
		startedAt = wsCreatedAt
	}

	messagesDir := filepath.Join(sessionDir, "messages")
	for _, ref := range msgRefs {
		msgID := ref.Get("id").Str
		if !IsValidSessionID(msgID) {
			malformed++
			continue
		}

		msgPath := filepath.Join(messagesDir, msgID+".json")
		msgBytes, err := os.ReadFile(msgPath)
		if err != nil {
			malformed++
			continue
		}

		if !gjson.ValidBytes(msgBytes) {
			malformed++
			continue
		}

		msgRoot := gjson.ParseBytes(msgBytes)
		roleStr := msgRoot.Get("role").Str
		ts := parseCodeBuddyTimestamp(msgRoot.Get("createdAt").Str)
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		extraStr := msgRoot.Get("extra").Str
		extraRoot := gjson.Parse(extraStr)
		if msgRoot.Get("extra").IsObject() {
			extraRoot = msgRoot.Get("extra")
		}
		msgModel := extraRoot.Get("modelId").Str
		if msgModel == "" {
			msgModel = extraRoot.Get("modelName").Str
		}
		if msgModel == "" {
			msgModel = modelHint
		}

		// Inner message JSON
		innerMsg := msgRoot.Get("message")
		if innerMsg.Type == gjson.String {
			innerMsg = gjson.Parse(innerMsg.Str)
		}

		switch roleStr {
		case "user":
			content, extractedCwd := extractCodeBuddyUserContent(innerMsg, extraRoot)
			if cwd == "" && extractedCwd != "" {
				cwd = extractedCwd
			}
			if strings.TrimSpace(content) == "" {
				continue
			}
			if firstMsg == "" {
				firstMsg = truncate(strings.ReplaceAll(content, "\n", " "), 300)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				Timestamp:     ts,
				ContentLength: len(content),
			})
			ordinal++
			realUserCount++

		case "assistant":
			textContent, thinkingText, toolCalls := extractCodeBuddyAssistantContent(innerMsg)

			msg := ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       textContent,
				Timestamp:     ts,
				ContentLength: len(textContent),
				Model:         msgModel,
				ThinkingText:  thinkingText,
				HasThinking:   thinkingText != "",
			}
			msg.ContentLength += len(thinkingText)
			if len(toolCalls) > 0 {
				msg.HasToolUse = true
				msg.ToolCalls = toolCalls
			}
			applyCodeBuddyUsage(&msg, extraRoot)
			if textContent == "" && thinkingText == "" && len(toolCalls) == 0 && len(msg.TokenUsage) == 0 {
				continue
			}
			messages = append(messages, msg)
			ordinal++

		case "tool":
			toolResults := extractCodeBuddyToolResults(innerMsg, extraRoot)
			if len(toolResults) == 0 {
				continue
			}
			contentLen := 0
			for _, tr := range toolResults {
				contentLen += tr.ContentLength
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Timestamp:     ts,
				ContentLength: contentLen,
				ToolResults:   toolResults,
			})
			ordinal++
		}
	}

	project := projectHint
	if cwd != "" {
		project = ExtractProjectFromCwd(cwd)
	}
	if project == "" {
		project = filepath.Base(wsDir)
	}

	sess := &ParsedSession{
		ID:               "codebuddy:" + sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentCodeBuddy,
		SessionName:      title,
		Cwd:              cwd,
		MalformedLines:   malformed,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: realUserCount,
		File: FileInfo{
			Path:  indexPath,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}
	accumulateMessageTokenUsage(sess, messages)
	return sess, messages, nil
}

func readCodeBuddyWorkspaceMetadata(wsDir, sessionID string) (title string, createdAt time.Time, modelID string) {
	idxPath := filepath.Join(wsDir, "index.json")
	data, err := os.ReadFile(idxPath)
	if err != nil {
		return "", time.Time{}, ""
	}
	root := gjson.ParseBytes(data)
	for _, conv := range root.Get("conversations").Array() {
		if conv.Get("id").Str == sessionID {
			title = conv.Get("name").Str
			createdAt = parseCodeBuddyTimestamp(conv.Get("createdAt").Str)
			modelID = conv.Get("selectedModelId").Str
			return title, createdAt, modelID
		}
	}
	return "", time.Time{}, ""
}

func extractCodeBuddyUserContent(innerMsg gjson.Result, extraRoot gjson.Result) (content, cwd string) {
	// Preserve every source text block, but still extract cwd from the envelope.
	var sourceTexts []string
	for _, blk := range extraRoot.Get("sourceContentBlocks").Array() {
		if text := blk.Get("text").Str; strings.TrimSpace(text) != "" {
			sourceTexts = append(sourceTexts, text)
		}
	}

	// Otherwise parse content array from inner message
	var rawTextParts []string
	for _, blk := range innerMsg.Get("content").Array() {
		if blk.Get("type").Str == "text" {
			rawTextParts = append(rawTextParts, blk.Get("text").Str)
		}
	}
	fullRaw := strings.Join(rawTextParts, "\n")

	// Try extracting cwd from <user_info>
	if _, rest, ok := strings.Cut(fullRaw, "Workspace Folder: "); ok {
		if newline := strings.IndexAny(rest, "\r\n"); newline != -1 {
			rest = rest[:newline]
		}
		cwd = strings.TrimSpace(strings.SplitN(rest, "</user_info>", 2)[0])
	}
	if len(sourceTexts) > 0 {
		return strings.TrimSpace(strings.Join(sourceTexts, "\n")), cwd
	}

	// Try extracting user query from <user_query> tags
	if m := codebuddyUserQueryRegex.FindStringSubmatch(fullRaw); len(m) > 1 {
		return strings.TrimSpace(m[1]), cwd
	}

	return strings.TrimSpace(fullRaw), cwd
}

func extractCodeBuddyAssistantContent(innerMsg gjson.Result) (textContent, thinkingText string, toolCalls []ParsedToolCall) {
	var texts, thoughts []string
	for _, blk := range innerMsg.Get("content").Array() {
		bType := blk.Get("type").Str
		switch bType {
		case "reasoning", "thinking":
			text := blk.Get("text").Str
			if text == "" {
				text = blk.Get("thinking").Str
			}
			if text != "" {
				thoughts = append(thoughts, text)
			}
		case "text":
			if t := blk.Get("text").Str; t != "" {
				texts = append(texts, t)
			}
		case "tool-call":
			callID := blk.Get("toolCallId").Str
			toolName := blk.Get("toolName").Str
			if callID == "" || toolName == "" {
				continue
			}
			args := blk.Get("args").Raw
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, ParsedToolCall{
				ToolUseID: callID,
				ToolName:  toolName,
				Category:  NormalizeToolCategory(toolName),
				InputJSON: args,
			})
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n")), strings.TrimSpace(strings.Join(thoughts, "\n")), toolCalls
}

func extractCodeBuddyToolResults(innerMsg gjson.Result, extraRoot gjson.Result) []ParsedToolResult {
	var results []ParsedToolResult

	for _, blk := range innerMsg.Get("content").Array() {
		if blk.Get("type").Str != "tool-result" {
			continue
		}
		callID := blk.Get("toolCallId").Str
		if callID == "" {
			continue
		}

		resObj := blk.Get("result")
		rawOutput := ""
		if stdout := resObj.Get("result.stdout").Str; stdout != "" {
			rawOutput = stdout
		} else if resObj.Exists() {
			rawOutput = resObj.Raw
		}

		contentLen := len(rawOutput)
		q, _ := json.Marshal(rawOutput)
		results = append(results, ParsedToolResult{
			ToolUseID:     callID,
			ContentLength: contentLen,
			ContentRaw:    string(q),
		})
	}

	// Fallback to extra.toolStatus if inner content has no tool-result
	if len(results) == 0 && extraRoot.Get("toolStatus").Exists() {
		extraRoot.Get("toolStatus").ForEach(func(callID, status gjson.Result) bool {
			res := status.Get("result")
			rawOutput := ""
			if stdout := res.Get("result.stdout").Str; stdout != "" {
				rawOutput = stdout
			} else if res.Exists() {
				rawOutput = res.Raw
			}
			contentLen := len(rawOutput)
			q, _ := json.Marshal(rawOutput)
			results = append(results, ParsedToolResult{
				ToolUseID:     callID.Str,
				ContentLength: contentLen,
				ContentRaw:    string(q),
			})
			return true
		})
	}

	return results
}

func applyCodeBuddyUsage(msg *ParsedMessage, extraRoot gjson.Result) {
	inputTokens := int(extraRoot.Get("lastStepInputTokens").Int())
	outputTokens := int(extraRoot.Get("lastStepOutputTokens").Int())
	cachedTokens := int(extraRoot.Get("lastStepCachedInputTokens").Int())
	thinkingTokens := int(extraRoot.Get("statsSnapshot.thinkingTokens").Int())

	hasInput := extraRoot.Get("lastStepInputTokens").Type == gjson.Number && inputTokens >= 0
	hasOutput := extraRoot.Get("lastStepOutputTokens").Type == gjson.Number && outputTokens >= 0
	hasCached := extraRoot.Get("lastStepCachedInputTokens").Type == gjson.Number && cachedTokens >= 0
	hasThinking := extraRoot.Get("statsSnapshot.thinkingTokens").Type == gjson.Number && thinkingTokens >= 0

	if !hasInput && !hasOutput && !hasCached && !hasThinking {
		return
	}

	netInput := inputTokens
	if hasCached && cachedTokens > 0 {
		netInput = max(0, netInput-cachedTokens)
	}

	normalized := map[string]int{}
	if hasInput {
		normalized["input_tokens"] = netInput
	}
	if hasOutput {
		normalized["output_tokens"] = outputTokens
	}
	if hasCached {
		normalized["cache_read_input_tokens"] = cachedTokens
	}
	if hasThinking {
		normalized["reasoning_tokens"] = thinkingTokens
	}

	j, err := json.Marshal(normalized)
	if err != nil {
		return
	}

	msg.TokenUsage = j
	if hasOutput {
		msg.OutputTokens = outputTokens
	}
	msg.HasOutputTokens = hasOutput
	if hasInput {
		msg.ContextTokens = inputTokens
	}
	msg.HasContextTokens = hasInput
}

func parseCodeBuddyTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
