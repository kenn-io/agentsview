package parser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

type tauEntry struct {
	Value    gjson.Result
	Type     string
	ID       string
	ParentID string
}

func readTauEntries(ctx context.Context, path string) ([]tauEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	reader := bufio.NewReader(checkedContextReader{ctx: ctx, reader: f})
	entries := make([]tauEntry, 0)
	var line []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part, prefix, readErr := reader.ReadLine()
		if len(line)+len(part) > maxLineSize {
			return nil, fmt.Errorf("tau record exceeds %d bytes", maxLineSize)
		}
		line = append(line, part...)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && len(line) != 0 {
				if err := appendTauEntry(&entries, line); err != nil {
					return nil, err
				}
				break
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read %s: %w", path, readErr)
		}
		if prefix {
			continue
		}
		if len(line) != 0 {
			if err := appendTauEntry(&entries, line); err != nil {
				return nil, err
			}
		}
		line = nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func appendTauEntry(entries *[]tauEntry, line []byte) error {
	if !utf8.Valid(line) {
		return errors.New("tau record is not valid UTF-8")
	}
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil
	}
	if !gjson.Valid(trimmed) {
		return errors.New("tau record is malformed JSON")
	}
	value := gjson.Parse(trimmed)
	*entries = append(*entries, tauEntry{
		Value:    value,
		Type:     value.Get("type").Str,
		ID:       value.Get("id").Str,
		ParentID: value.Get("parent_id").Str,
	})
	return nil
}

func selectTauEntries(entries []tauEntry) ([]tauEntry, error) {
	leaf := -1
	for i := range entries {
		if entries[i].Type == "leaf" {
			leaf = i
		}
	}
	if leaf < 0 {
		return entries, nil
	}
	leafID := entries[leaf].Value.Get("entry_id")
	if !leafID.Exists() || leafID.Type == gjson.Null || leafID.Str == "" {
		return nil, nil
	}

	byID := make(map[string][]tauEntry)
	for _, entry := range entries {
		if entry.ID != "" {
			byID[entry.ID] = append(byID[entry.ID], entry)
		}
	}

	selected := make([]tauEntry, 0)
	visited := make(map[string]struct{})
	current := leafID.Str
	for current != "" {
		if _, ok := visited[current]; ok {
			return nil, fmt.Errorf("tau ancestry cycle at entry %q", current)
		}
		visited[current] = struct{}{}
		candidates := byID[current]
		if len(candidates) == 0 {
			if len(selected) == 0 {
				return nil, fmt.Errorf("tau leaf target %q is missing", current)
			}
			break
		}
		if len(candidates) != 1 {
			return nil, fmt.Errorf("tau entry ID %q is duplicated", current)
		}
		entry := candidates[0]
		selected = append(selected, entry)
		current = entry.ParentID
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	return selected, nil
}

func parseTauSession(
	ctx context.Context, path, machine string, pathRewriter func(string) string,
) (*ParsedSession, []ParsedMessage, error) {
	entries, err := readTauEntries(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	recognized := false
	for _, entry := range entries {
		if entry.Type == "session_info" {
			recognized = true
			break
		}
	}
	if !recognized {
		return nil, nil, nil
	}
	selected, err := selectTauEntries(entries)
	if err != nil {
		return nil, nil, err
	}

	info := tauSessionInfo(entries)
	cwd := info.Get("cwd").Str
	project := ExtractProjectFromCwdWithBranchContext(ctx, cwd, "")
	rawID := tauSessionIDFromPathWithRewriter("", path, pathRewriter)
	startedAt := tauOuterTimestamp(info.Get("created_at"))
	if startedAt.IsZero() {
		startedAt = tauSessionInfoTimestamp(entries)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	session := &ParsedSession{
		ID:        "tau:" + rawID,
		Project:   project,
		Machine:   machine,
		Agent:     AgentTau,
		Cwd:       cwd,
		StartedAt: startedAt,
		File: FileInfo{
			Path:  path,
			Size:  fileInfo.Size(),
			Mtime: fileInfo.ModTime().UnixNano(),
		},
	}

	messages, label := decodeTauMessages(ctx, selected)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	firstMessage, userCount := firstMessageAndUserCount(messages)
	session.FirstMessage = firstMessage
	session.UserMessageCount = userCount
	session.MessageCount = len(messages)
	session.SessionName = label
	if session.SessionName == "" {
		session.SessionName = info.Get("title").Str
	}
	if session.SessionName == "" {
		session.SessionName = firstMessage
	}
	if session.StartedAt.IsZero() {
		for _, message := range messages {
			if message.Timestamp.IsZero() {
				continue
			}
			if session.StartedAt.IsZero() || message.Timestamp.Before(session.StartedAt) {
				session.StartedAt = message.Timestamp
			}
		}
	}
	for _, message := range messages {
		if message.Timestamp.IsZero() {
			continue
		}
		if session.EndedAt.IsZero() || message.Timestamp.After(session.EndedAt) {
			session.EndedAt = message.Timestamp
		}
	}
	accumulateMessageTokenUsage(session, messages)
	return session, messages, nil
}

func tauSessionInfo(entries []tauEntry) gjson.Result {
	var info gjson.Result
	for _, entry := range entries {
		if entry.Type == "session_info" {
			info = entry.Value
		}
	}
	return info
}

func tauSessionInfoTimestamp(entries []tauEntry) time.Time {
	for _, entry := range slices.Backward(entries) {
		if entry.Type == "session_info" {
			return tauOuterTimestamp(entry.Value.Get("timestamp"))
		}
	}
	return time.Time{}
}

func decodeTauMessages(
	ctx context.Context, entries []tauEntry,
) ([]ParsedMessage, string) {
	messages := make([]ParsedMessage, 0)
	visibleAncestorByID := make(map[string]string)
	model := ""
	label := ""
	for i, entry := range entries {
		if err := contextErrEvery(ctx, i); err != nil {
			break
		}
		parent := visibleAncestorByID[entry.ParentID]
		var message *ParsedMessage
		switch entry.Type {
		case "message":
			message = decodeTauMessage(entry, len(messages), parent, model)
		case "model_change":
			model = entry.Value.Get("model").Str
		case "label":
			label = entry.Value.Get("label").Str
		case "compaction":
			summary := entry.Value.Get("summary").Str
			message = &ParsedMessage{
				Ordinal:           len(messages),
				Role:              RoleAssistant,
				Content:           summary,
				Timestamp:         tauOuterTimestamp(entry.Value.Get("timestamp")),
				IsSystem:          true,
				IsCompactBoundary: true,
				ContentLength:     len(summary),
				SourceType:        "system",
				SourceSubtype:     "compact_boundary",
				SourceUUID:        entry.ID,
				SourceParentUUID:  parent,
			}
		case "branch_summary":
			summary := entry.Value.Get("summary").Str
			message = &ParsedMessage{
				Ordinal:          len(messages),
				Role:             RoleUser,
				Content:          "The following is a summary of a branch that this conversation came back from:\n<summary>\n" + summary + "\n</summary>",
				Timestamp:        tauOuterTimestamp(entry.Value.Get("timestamp")),
				IsSystem:         true,
				ContentLength:    len(summary),
				SourceType:       "branch_summary",
				SourceUUID:       entry.ID,
				SourceParentUUID: parent,
			}
		}
		if message != nil {
			messages = append(messages, *message)
			if entry.ID != "" {
				visibleAncestorByID[entry.ID] = entry.ID
			}
		} else if entry.ID != "" {
			visibleAncestorByID[entry.ID] = parent
		}
	}
	return messages, label
}

func decodeTauMessage(
	entry tauEntry, ordinal int, parent, fallbackModel string,
) *ParsedMessage {
	message := entry.Value.Get("message")
	role := message.Get("role").Str
	timestamp := tauMessageTimestamp(message.Get("timestamp"))
	if timestamp.IsZero() {
		timestamp = tauOuterTimestamp(entry.Value.Get("timestamp"))
	}
	switch role {
	case "user":
		content, _, _, _, _ := tauExtractContent(message.Get("content"))
		return &ParsedMessage{
			Ordinal:          ordinal,
			Role:             RoleUser,
			Content:          content,
			Timestamp:        timestamp,
			ContentLength:    len(content),
			SourceType:       "user",
			SourceUUID:       entry.ID,
			SourceParentUUID: parent,
		}
	case "assistant":
		content, thinking, hasThinking, hasToolUse, toolCalls :=
			tauExtractContent(message.Get("content"))
		if content == "" && message.Get("errorMessage").Str != "" {
			content = message.Get("errorMessage").Str
		}
		pm := &ParsedMessage{
			Ordinal:          ordinal,
			Role:             RoleAssistant,
			Content:          content,
			ThinkingText:     thinking,
			Timestamp:        timestamp,
			HasThinking:      hasThinking,
			HasToolUse:       hasToolUse,
			ContentLength:    len(content),
			ToolCalls:        toolCalls,
			Model:            message.Get("model").Str,
			StopReason:       message.Get("stopReason").Str,
			SourceType:       "assistant",
			SourceUUID:       entry.ID,
			SourceParentUUID: parent,
		}
		if pm.Model == "" {
			pm.Model = fallbackModel
		}
		applyTauUsage(pm, message.Get("usage"))
		return pm
	case "toolResult":
		content := message.Get("content")
		return &ParsedMessage{
			Ordinal:       ordinal,
			Role:          RoleUser,
			Timestamp:     timestamp,
			ContentLength: toolResultContentLength(content),
			ToolResults: []ParsedToolResult{{
				ToolUseID:     message.Get("toolCallId").Str,
				ContentLength: toolResultContentLength(content),
				ContentRaw:    content.Raw,
			}},
			SourceType:       "toolResult",
			SourceUUID:       entry.ID,
			SourceParentUUID: parent,
		}
	default:
		return nil
	}
}

func tauExtractContent(
	content gjson.Result,
) (string, string, bool, bool, []ParsedToolCall) {
	if content.Type == gjson.String {
		return content.Str, "", false, false, nil
	}
	if !content.IsArray() {
		return "", "", false, false, nil
	}
	var parts, thinkingParts []string
	var toolCalls []ParsedToolCall
	hasThinking, hasToolUse := false, false
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "text":
			if text := block.Get("text").Str; text != "" {
				parts = append(parts, text)
			}
		case "thinking":
			hasThinking = true
			thinking := block.Get("thinking").Str
			if thinking != "" {
				thinkingParts = append(thinkingParts, thinking)
				parts = append(parts, "[Thinking]\n"+thinking+"\n[/Thinking]")
			}
		case "toolCall":
			hasToolUse = true
			input := toolCallInput(block)
			name := block.Get("name").Str
			if name != "" {
				toolCalls = append(toolCalls, ParsedToolCall{
					ToolUseID: block.Get("id").Str,
					ToolName:  name,
					Category:  NormalizeToolCategory(name),
					InputJSON: input.Raw,
				})
			}
			parts = append(parts, formatToolUse(block))
		}
		return true
	})
	return strings.Join(parts, "\n"), strings.Join(thinkingParts, "\n\n"),
		hasThinking, hasToolUse, toolCalls
}

func tauOuterTimestamp(value gjson.Result) time.Time {
	if !value.Exists() || value.Type == gjson.Null {
		return time.Time{}
	}
	seconds, fraction := math.Modf(value.Float())
	nanos := int64(math.Round(fraction * 1e9))
	if nanos == int64(time.Second) {
		seconds++
		nanos = 0
	}
	return time.Unix(int64(seconds), nanos).UTC()
}

func tauMessageTimestamp(value gjson.Result) time.Time {
	if !value.Exists() || value.Type == gjson.Null {
		return time.Time{}
	}
	return time.UnixMilli(value.Int()).UTC()
}

func applyTauUsage(pm *ParsedMessage, usage gjson.Result) {
	if !usage.Exists() || !usage.IsObject() {
		return
	}
	fields := map[string]struct {
		name string
		key  string
	}{
		"input":      {"input_tokens", "input"},
		"output":     {"output_tokens", "output"},
		"cacheRead":  {"cache_read_input_tokens", "cacheRead"},
		"cacheWrite": {"cache_creation_input_tokens", "cacheWrite"},
	}
	normalized := make(map[string]int)
	for _, field := range fields {
		value := usage.Get(field.key)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		normalized[field.name] = int(value.Int())
	}
	if len(normalized) == 0 {
		return
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return
	}
	pm.TokenUsage = encoded
	if value := usage.Get("output"); value.Exists() && value.Type != gjson.Null {
		pm.OutputTokens = int(value.Int())
		pm.HasOutputTokens = true
	}
	for _, key := range []string{"input", "cacheRead", "cacheWrite"} {
		value := usage.Get(key)
		if value.Exists() && value.Type != gjson.Null {
			pm.ContextTokens += int(value.Int())
			pm.HasContextTokens = true
		}
	}
}
