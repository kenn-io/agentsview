package parser

import (
	"context"
	"encoding/json"
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

type fxMetadata struct {
	Version  int        `json:"schema_version"`
	Title    string     `json:"title"`
	Cwd      string     `json:"workspace_root"`
	Created  int64      `json:"created_at_ms"`
	Updated  int64      `json:"updated_at_ms"`
	Model    string     `json:"model"`
	Provider fxProvider `json:"provider"`
}
type fxOwner struct {
	Version int    `json:"schema_version"`
	Parent  string `json:"parent_id"`
}
type fxProvider string

func (p *fxProvider) UnmarshalJSON(data []byte) error {
	var name string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &name); err != nil {
			return err
		}
	} else {
		var provider *struct {
			Name    string `json:"name"`
			Binding string `json:"binding"`
		}
		if err := json.Unmarshal(data, &provider); err != nil {
			return err
		}
		if provider == nil || provider.Name == "" {
			return fmt.Errorf("invalid fx provider")
		}
		name = provider.Name
	}
	*p = fxProvider(name)
	return nil
}

type fxFrame struct {
	Version   int      `json:"schema_version"`
	Timestamp int64    `json:"timestamp_ms"`
	Event     *fxEvent `json:"event"`
}
type fxText struct {
	Text string `json:"text"`
}
type fxEvent struct {
	User      *fxText `json:"user"`
	Steering  *fxText `json:"steering"`
	Assistant *struct {
		Text   string `json:"text"`
		Replay *struct {
			Source *struct {
				Model    string     `json:"model"`
				Provider fxProvider `json:"provider"`
			} `json:"source"`
		} `json:"provider_replay"`
	} `json:"assistant"`
	ToolCall *struct {
		ID        string `json:"call_id"`
		Name      string `json:"tool_name"`
		Arguments string `json:"arguments_json"`
	} `json:"tool_call"`
	ToolResult *struct {
		ID      string          `json:"call_id"`
		Status  string          `json:"status"`
		Preview json.RawMessage `json:"preview"`
	} `json:"tool_result"`
	Checkpoint *struct {
		Summary string `json:"summary"`
	} `json:"context_checkpoint"`
	Interrupted *struct {
		Text string `json:"partial_text"`
	} `json:"interrupted"`
}
type fxCount int

func (c *fxCount) UnmarshalJSON(data []byte) error {
	var count int
	if string(data) == "null" {
		return fmt.Errorf("null fx token count")
	}
	if err := json.Unmarshal(data, &count); err != nil {
		return err
	}
	if count < 0 {
		return fmt.Errorf("negative fx token count")
	}
	*c = fxCount(count)
	return nil
}

type fxUsageModel struct {
	Model      string  `json:"model"`
	Input      fxCount `json:"input_tokens"`
	Output     fxCount `json:"output_tokens"`
	CacheRead  fxCount `json:"cache_read_tokens"`
	CacheWrite fxCount `json:"cache_write_tokens"`
	Reasoning  fxCount `json:"reasoning_tokens"`
}
type fxUsage struct {
	Version   int     `json:"schema_version"`
	SessionID *string `json:"session_id"`
	Snapshot  *struct {
		Version int             `json:"schema_version"`
		Models  []*fxUsageModel `json:"models"`
	} `json:"snapshot"`
}

func readFxMetadata[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read fx metadata: %w", err)
	}
	var value *T
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("invalid fx metadata in %s: %w", path, err)
	}
	if value == nil {
		return nil, fmt.Errorf("invalid fx metadata in %s: null object", path)
	}
	return value, nil
}

func fxSchemaOK(version, max int, session, file string) bool {
	min := 0
	if max == 4 {
		min = 4
	}
	if version >= min && version <= max {
		return true
	}
	direction, supported := "newer", max
	if version < min {
		direction, supported = "older", min
	}
	log.Printf("skipping fx session %s: unsupported %s schema_version %d is %s than supported %d", session, file, version, direction, supported)
	return false
}

func parseFxSession(ctx context.Context, filePath, machine string) (*ParsedSession, []ParsedMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	dir := filepath.Dir(filePath)
	rawID := filepath.Base(dir)
	metadata, err := readFxMetadata[fxMetadata](filepath.Join(dir, "session.json"))
	if err != nil {
		return nil, nil, err
	}
	if metadata == nil {
		log.Printf("skipping fx session %s: missing session.json", rawID)
		return nil, nil, nil
	}
	if !fxSchemaOK(metadata.Version, 4, rawID, "session.json") {
		return nil, nil, nil
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
	session := &ParsedSession{
		ID: "fx:" + rawID, Agent: AgentFx, Machine: machine, Cwd: metadata.Cwd,
		Project: ExtractProjectFromCwdWithBranchContext(ctx, metadata.Cwd, ""), SessionName: metadata.Title,
		EndedAt: time.UnixMilli(metadata.Updated).UTC(),
		File:    FileInfo{Path: filePath, Size: info.Size(), Mtime: info.ModTime().UnixNano()},
	}
	if metadata.Created != 0 {
		session.StartedAt = time.UnixMilli(metadata.Created).UTC()
	}
	owner, err := readFxMetadata[fxOwner](filepath.Join(dir, "subagent", "owner.json"))
	if err != nil {
		return nil, nil, err
	}
	if owner != nil {
		if !fxSchemaOK(owner.Version, 1, rawID, "owner.json") {
			return nil, nil, nil
		}
		if IsValidSessionID(owner.Parent) {
			session.ParentSessionID, session.RelationshipType = "fx:"+owner.Parent, RelSubagent
		}
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
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame *fxFrame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			var tail [1]byte
			if !json.Valid([]byte(line)) {
				if _, err := f.ReadAt(tail[:], reader.bytesRead-1); err != nil {
					return nil, nil, err
				}
				if tail[0] != '\n' {
					break
				}
			}
			return nil, nil, fmt.Errorf("invalid fx events.jsonl frame: %w", err)
		}
		if frame == nil || frame.Event == nil {
			return nil, nil, fmt.Errorf("invalid fx events.jsonl frame: event must be an object")
		}
		if !fxSchemaOK(frame.Version, 3, rawID, "events.jsonl frame") {
			return nil, nil, nil
		}
		timestamp := time.UnixMilli(frame.Timestamp).UTC()
		if session.StartedAt.IsZero() {
			session.StartedAt = timestamp
		}
		if timestamp.After(latestEventTime) {
			latestEventTime = timestamp
		}
		if timestamp.After(session.EndedAt) {
			session.EndedAt = timestamp
		}
		fxAppendEvent(&messages, frame.Event, timestamp)
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
		messages[i].Ordinal, messages[i].ContentLength = i, len(messages[i].Content)
	}
	session.FirstMessage, session.UserMessageCount = firstMessageAndUserCount(messages)
	session.MessageCount = len(messages)
	if session.SessionName == "" {
		session.SessionName = session.FirstMessage
	}
	usage, err := readFxMetadata[fxUsage](filepath.Join(dir, "usage-v2.json"))
	if err != nil {
		return nil, nil, err
	}
	if usage != nil {
		if usage.Snapshot == nil || usage.Snapshot.Models == nil {
			return nil, nil, fmt.Errorf("invalid fx usage-v2.json: expected snapshot object and models array")
		}
		if usage.SessionID != nil && *usage.SessionID != rawID {
			return nil, nil, fmt.Errorf("fx usage-v2.json session_id does not match session folder")
		}
		if !fxSchemaOK(usage.Version, 1, rawID, "usage-v2.json") || !fxSchemaOK(usage.Snapshot.Version, 3, rawID, "usage-v2.json snapshot") {
			return nil, nil, nil
		}
		for _, model := range usage.Snapshot.Models {
			if model == nil || model.Model == "" {
				return nil, nil, fmt.Errorf("invalid fx usage-v2.json model entry")
			}
			provider, _, prefixed := strings.Cut(model.Model, "/")
			if !prefixed {
				provider = ""
			}
			session.UsageEvents = append(session.UsageEvents, ParsedUsageEvent{
				SessionID: session.ID, Source: "session", Model: model.Model, ProviderID: provider,
				DedupKey: "session:" + session.ID + "|" + model.Model, OccurredAt: timeString(latestEventTime, session.StartedAt),
				InputTokens: max(0, int(model.Input)-int(model.CacheRead)-int(model.CacheWrite)), OutputTokens: int(model.Output),
				CacheReadInputTokens: int(model.CacheRead), CacheCreationInputTokens: int(model.CacheWrite), ReasoningTokens: int(model.Reasoning),
			})
		}
	}
	// Cumulative session usage cannot establish a per-request context peak.
	session.TotalOutputTokens, session.HasTotalOutputTokens, _, _ = UsageEventTokenAggregate(session.UsageEvents)
	return session, messages, nil
}

func fxAppendEvent(messages *[]ParsedMessage, event *fxEvent, timestamp time.Time) {
	message := ParsedMessage{Timestamp: timestamp}
	switch {
	case event.User != nil:
		message.Role, message.Content = RoleUser, event.User.Text
	case event.Steering != nil:
		message.Role, message.Content = RoleUser, event.Steering.Text
	case event.Assistant != nil:
		message.Role, message.Content = RoleAssistant, event.Assistant.Text
		if replay := event.Assistant.Replay; replay != nil && replay.Source != nil {
			message.Model, message.ProviderID = replay.Source.Model, string(replay.Source.Provider)
		}
	case event.ToolCall != nil:
		call := event.ToolCall
		tool := ParsedToolCall{
			ToolUseID: call.ID, ToolName: call.Name,
			InputJSON: call.Arguments,
			Category:  NormalizeToolCategory(call.Name),
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
			*messages = append(*messages, ParsedMessage{Role: RoleAssistant, Timestamp: timestamp})
		}
		assistant := &(*messages)[len(*messages)-1]
		assistant.ToolCalls = append(assistant.ToolCalls, tool)
		assistant.HasToolUse = true
		return
	case event.ToolResult != nil:
		result := event.ToolResult
		message.Role = RoleUser
		status := ""
		switch result.Status {
		case "success":
			status = "completed"
		case "failure":
			status = "errored"
		}
		message.ToolResults = []ParsedToolResult{{ToolUseID: result.ID, Source: "tool_result", Status: status, ContentLength: len(gjson.ParseBytes(result.Preview).Str), ContentRaw: string(result.Preview)}}
	case event.Checkpoint != nil:
		message.Role, message.Content = RoleSystem, event.Checkpoint.Summary
		message.IsSystem, message.IsCompactBoundary = true, true
		message.SourceSubtype = "compact_boundary"
	case event.Interrupted != nil && event.Interrupted.Text != "":
		message.Role, message.Content = RoleAssistant, event.Interrupted.Text
	default:
		return
	}
	*messages = append(*messages, message)
}
