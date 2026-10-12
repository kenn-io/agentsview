package parser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/ctxio"
)

func readFxMetadata(path string) (gjson.Result, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return gjson.Result{}, nil
	}
	if err != nil {
		return gjson.Result{}, fmt.Errorf("read fx metadata: %w", err)
	}
	if !gjson.ValidBytes(data) {
		return gjson.Result{}, fmt.Errorf("invalid fx metadata in %s", path)
	}
	return gjson.ParseBytes(data), nil
}

func parseFxSession(ctx context.Context, filePath, machine string) (*ParsedSession, []ParsedMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	dir := filepath.Dir(filePath)
	metadata, err := readFxMetadata(filepath.Join(dir, "session.json"))
	if err != nil {
		return nil, nil, err
	}
	if metadata.Get("schema_version").Int() != 4 {
		return nil, nil, nil
	}
	rawID := filepath.Base(dir)
	if !IsValidSessionID(rawID) {
		return nil, nil, fmt.Errorf("invalid fx session ID")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	cwd := metadata.Get("workspace_root").Str
	session := &ParsedSession{
		ID: "fx:" + rawID, Agent: AgentFx, Machine: machine, Cwd: cwd,
		Project:     ExtractProjectFromCwdWithBranchContext(ctx, cwd, ""),
		SessionName: metadata.Get("title").Str,
		StartedAt:   time.UnixMilli(metadata.Get("created_at_ms").Int()).UTC(),
		EndedAt:     time.UnixMilli(metadata.Get("updated_at_ms").Int()).UTC(),
		File:        FileInfo{Path: filePath, Size: info.Size(), Mtime: info.ModTime().UnixNano()},
	}
	owner, err := readFxMetadata(filepath.Join(dir, "subagent", "owner.json"))
	if err != nil {
		return nil, nil, err
	}
	if parent := owner.Get("parent_id").Str; IsValidSessionID(parent) {
		session.ParentSessionID = "fx:" + parent
		session.RelationshipType = RelSubagent
	}
	var messages []ParsedMessage
	scanner := bufio.NewScanner(ctxio.Reader{Context: ctx, Reader: f})
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !gjson.Valid(line) {
			session.MalformedLines++
			continue
		}
		frame := gjson.Parse(line)
		event := frame.Get("event")
		if !event.IsObject() {
			session.MalformedLines++
			continue
		}
		timestamp := time.UnixMilli(frame.Get("timestamp_ms").Int()).UTC()
		if timestamp.After(session.EndedAt) {
			session.EndedAt = timestamp
		}
		fxAppendEvent(&messages, event, timestamp, metadata)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read fx events: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	for i := range messages {
		messages[i].Ordinal = i
		messages[i].ContentLength = len(messages[i].Content)
	}
	session.FirstMessage, session.UserMessageCount = firstMessageAndUserCount(messages)
	session.MessageCount = len(messages)
	if session.SessionName == "" {
		session.SessionName = session.FirstMessage
	}
	usage, err := readFxMetadata(filepath.Join(dir, "usage-v2.json"))
	if err != nil {
		return nil, nil, err
	}
	session.UsageEvents = fxUsageEvents(session, usage)
	applyUsageEventTokenTotals(session, session.UsageEvents)
	return session, messages, nil
}

func fxAppendEvent(messages *[]ParsedMessage, event gjson.Result, timestamp time.Time, metadata gjson.Result) {
	message := ParsedMessage{Timestamp: timestamp}
	switch {
	case event.Get("user").Exists():
		message.Role, message.Content = RoleUser, event.Get("user.text").Str
	case event.Get("steering").Exists():
		message.Role, message.Content = RoleUser, event.Get("steering.text").Str
	case event.Get("assistant").Exists():
		message.Role, message.Content = RoleAssistant, event.Get("assistant.text").Str
		message.Model = event.Get("assistant.provider_replay.source.model").Str
		message.ProviderID = event.Get("assistant.provider_replay.source.provider").Str
	case event.Get("tool_call").Exists():
		call := event.Get("tool_call")
		tool := ParsedToolCall{
			ToolUseID: call.Get("call_id").Str, ToolName: call.Get("tool_name").Str,
			InputJSON: call.Get("arguments_json").Str,
			Category:  NormalizeToolCategory(call.Get("tool_name").Str),
		}
		if tool.ToolName == "skill" {
			location := gjson.Get(tool.InputJSON, "location").Str
			if location != "" {
				tool.SkillName = path.Base(location)
			}
		}
		if len(*messages) == 0 || (*messages)[len(*messages)-1].Role != RoleAssistant {
			*messages = append(*messages, ParsedMessage{Role: RoleAssistant, Timestamp: timestamp, Model: metadata.Get("model").Str, ProviderID: metadata.Get("provider").Str})
		}
		assistant := &(*messages)[len(*messages)-1]
		assistant.ToolCalls = append(assistant.ToolCalls, tool)
		assistant.HasToolUse = true
		return
	case event.Get("tool_result").Exists():
		result := event.Get("tool_result")
		message.Role, message.Content = RoleTool, result.Get("preview").Str
		status := ""
		switch result.Get("status").Str {
		case "success":
			status = "completed"
		case "failure":
			status = "errored"
		}
		message.ToolResults = []ParsedToolResult{{ToolUseID: result.Get("call_id").Str, Status: status, ContentLength: len(message.Content), ContentRaw: result.Get("preview").Raw}}
	case event.Get("context_checkpoint").Exists():
		message.Role, message.Content = RoleSystem, event.Get("context_checkpoint.summary").Str
		message.IsSystem, message.IsCompactBoundary = true, true
		message.SourceSubtype = "compact_boundary"
	case event.Get("interrupted.partial_text").Str != "":
		message.Role, message.Content = RoleAssistant, event.Get("interrupted.partial_text").Str
	default:
		return
	}
	if message.Role == RoleAssistant {
		if message.Model == "" {
			message.Model = metadata.Get("model").Str
		}
		if message.ProviderID == "" {
			message.ProviderID = metadata.Get("provider").Str
		}
	}
	*messages = append(*messages, message)
}

func fxUsageEvents(session *ParsedSession, usage gjson.Result) []ParsedUsageEvent {
	var events []ParsedUsageEvent
	for _, model := range usage.Get("snapshot.models").Array() {
		cacheRead := max(0, int(model.Get("cache_read_tokens").Int()))
		cacheWrite := max(0, int(model.Get("cache_write_tokens").Int()))
		name := model.Get("model").Str
		events = append(events, ParsedUsageEvent{
			SessionID: session.ID, Source: "session", Model: name,
			DedupKey:             "session:" + session.ID + "|" + name,
			OccurredAt:           timeString(session.EndedAt, session.StartedAt),
			InputTokens:          max(0, int(model.Get("input_tokens").Int())-cacheRead-cacheWrite),
			OutputTokens:         max(0, int(model.Get("output_tokens").Int())),
			CacheReadInputTokens: cacheRead, CacheCreationInputTokens: cacheWrite,
			ReasoningTokens: max(0, int(model.Get("reasoning_tokens").Int())),
		})
	}
	return events
}
