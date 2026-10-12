package parser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
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
	if metadata.Raw == "" {
		log.Printf("skipping fx session: missing session.json")
		return nil, nil, nil
	}
	if fxUnsupportedSchema(metadata, 4, 4, "session.json") {
		return nil, nil, nil
	}
	rawID := filepath.Base(dir)
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
	if fxUnsupportedSchema(owner, 0, 1, "owner.json") {
		return nil, nil, nil
	}
	if parent := owner.Get("parent_id").Str; IsValidSessionID(parent) {
		session.ParentSessionID = "fx:" + parent
		session.RelationshipType = RelSubagent
	}
	var messages []ParsedMessage
	var latestEventTime time.Time
	reader := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(reader)
	for {
		line, ok := reader.next()
		if !ok {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !gjson.Valid(line) {
			var tail [1]byte
			if reader.bytesRead == info.Size() {
				if _, err := f.ReadAt(tail[:], reader.bytesRead-1); err == nil && tail[0] != '\n' {
					break
				}
			}
			return nil, nil, fmt.Errorf("invalid fx events.jsonl JSON frame")
		}
		frame := gjson.Parse(line)
		if fxUnsupportedSchema(frame, 0, 3, "events.jsonl frame") {
			return nil, nil, nil
		}
		event := frame.Get("event")
		if !event.IsObject() {
			return nil, nil, fmt.Errorf("invalid fx events.jsonl frame: event must be an object")
		}
		timestamp := time.UnixMilli(frame.Get("timestamp_ms").Int()).UTC()
		if timestamp.After(latestEventTime) {
			latestEventTime = timestamp
		}
		if timestamp.After(session.EndedAt) {
			session.EndedAt = timestamp
		}
		fxAppendEvent(&messages, event, timestamp, metadata)
	}
	if err := reader.Err(); err != nil {
		return nil, nil, fmt.Errorf("read fx events: %w", err)
	}
	if reader.skippedOversized {
		return nil, nil, fmt.Errorf("fx event exceeds maximum line size")
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
	if usage.Raw != "" {
		if !usage.IsObject() || !usage.Get("snapshot").IsObject() || !usage.Get("snapshot.models").IsArray() {
			return nil, nil, fmt.Errorf("invalid fx usage-v2.json: expected an object with snapshot object and models array")
		}
		if id := usage.Get("session_id"); id.Exists() && id.Str != rawID {
			return nil, nil, fmt.Errorf("fx usage-v2.json session_id does not match session folder")
		}
	}
	if fxUnsupportedSchema(usage, 0, 1, "usage-v2.json") || fxUnsupportedSchema(usage.Get("snapshot"), 0, 3, "usage-v2.json snapshot") {
		return nil, nil, nil
	}
	session.UsageEvents = fxUsageEvents(session, usage, latestEventTime)
	// Cumulative session usage cannot establish a per-request context peak.
	session.TotalOutputTokens, session.HasTotalOutputTokens, _, _ = UsageEventTokenAggregate(session.UsageEvents)
	return session, messages, nil
}

func fxUnsupportedSchema(value gjson.Result, minimum, maximum int64, source string) bool {
	version := value.Get("schema_version").Int()
	if version >= minimum && version <= maximum {
		return false
	}
	direction, supported := "newer", maximum
	if version < minimum {
		direction, supported = "older", minimum
	}
	log.Printf("skipping fx session: unsupported %s schema_version %d is %s than supported %d", source, version, direction, supported)
	return true
}

func fxProviderName(provider gjson.Result) string {
	if provider.IsObject() {
		return provider.Get("name").Str
	}
	return provider.Str
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
		message.ProviderID = fxProviderName(event.Get("assistant.provider_replay.source.provider"))
	case event.Get("tool_call").Exists():
		call := event.Get("tool_call")
		tool := ParsedToolCall{
			ToolUseID: call.Get("call_id").Str, ToolName: call.Get("tool_name").Str,
			InputJSON: call.Get("arguments_json").Str,
			Category:  NormalizeToolCategory(call.Get("tool_name").Str),
		}
		if tool.ToolName == "skill" {
			tool.SkillName = gjson.Get(tool.InputJSON, "name").Str
			location := gjson.Get(tool.InputJSON, "location").Str
			if tool.SkillName == "" && location != "" {
				tool.SkillName = path.Base(location)
				if decoded, err := url.PathUnescape(tool.SkillName); err == nil {
					tool.SkillName = decoded
				}
			}
		}
		if len(*messages) == 0 || (*messages)[len(*messages)-1].Role != RoleAssistant {
			model, provider := metadata.Get("model").Str, fxProviderName(metadata.Get("provider"))
			for i := len(*messages) - 1; i >= 0; i-- {
				previous := (*messages)[i]
				if previous.Role == RoleUser && len(previous.ToolResults) == 0 {
					break
				}
				if previous.Role == RoleAssistant {
					model, provider = previous.Model, previous.ProviderID
					break
				}
			}
			*messages = append(*messages, ParsedMessage{Role: RoleAssistant, Timestamp: timestamp, Model: model, ProviderID: provider})
		}
		assistant := &(*messages)[len(*messages)-1]
		assistant.ToolCalls = append(assistant.ToolCalls, tool)
		assistant.HasToolUse = true
		return
	case event.Get("tool_result").Exists():
		result := event.Get("tool_result")
		message.Role = RoleUser
		status := ""
		switch result.Get("status").Str {
		case "success":
			status = "completed"
		case "failure":
			status = "errored"
		}
		message.ToolResults = []ParsedToolResult{{ToolUseID: result.Get("call_id").Str, Source: "tool_result", Status: status, ContentLength: len(result.Get("preview").Str), ContentRaw: result.Get("preview").Raw}}
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
			message.ProviderID = fxProviderName(metadata.Get("provider"))
		}
	}
	*messages = append(*messages, message)
}

func fxUsageEvents(session *ParsedSession, usage gjson.Result, latestEventTime time.Time) []ParsedUsageEvent {
	var events []ParsedUsageEvent
	for _, model := range usage.Get("snapshot.models").Array() {
		cacheRead := max(0, int(model.Get("cache_read_tokens").Int()))
		cacheWrite := max(0, int(model.Get("cache_write_tokens").Int()))
		name := model.Get("model").Str
		provider, _, prefixed := strings.Cut(name, "/")
		if !prefixed {
			provider = ""
		}
		events = append(events, ParsedUsageEvent{
			SessionID: session.ID, Source: "session", Model: name, ProviderID: provider,
			DedupKey:             "session:" + session.ID + "|" + name,
			OccurredAt:           timeString(latestEventTime, session.StartedAt),
			InputTokens:          max(0, int(model.Get("input_tokens").Int())-cacheRead-cacheWrite),
			OutputTokens:         int(model.Get("output_tokens").Int()),
			CacheReadInputTokens: cacheRead, CacheCreationInputTokens: cacheWrite,
			ReasoningTokens: int(model.Get("reasoning_tokens").Int()),
		})
	}
	return events
}
