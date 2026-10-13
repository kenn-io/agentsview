package parser

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

type fxMetadata struct {
	Version int    `json:"schema_version"`
	Title   string `json:"title"`
	Cwd     string `json:"workspace_root"`
	Created int64  `json:"created_at_ms"`
	Updated int64  `json:"updated_at_ms"`
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
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &provider); err != nil {
			return err
		}
		if provider == nil || provider.Name == "" {
			return errors.New("invalid fx provider")
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
		ID          string         `json:"call_id"`
		Status      string         `json:"status"`
		Preview     jsontext.Value `json:"preview"`
		ArtifactRef string         `json:"artifact_ref"`
		StoredBytes int64          `json:"stored_bytes"`
	} `json:"tool_result"`
	Checkpoint *struct {
		Summary string `json:"summary"`
	} `json:"context_checkpoint"`
	Interrupted *struct {
		Text string `json:"partial_text"`
	} `json:"interrupted"`
	TurnCompleted *struct{} `json:"turn_completed"`
}

func (e *fxEvent) UnmarshalJSON(data []byte) error {
	var keys map[string]jsontext.Value
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("fx event must contain one event type")
	}
	var fields fxEvent
	known := false
	for key, value := range keys {
		var target any
		switch key {
		case "user":
			target = &fields.User
		case "steering":
			target = &fields.Steering
		case "assistant":
			target = &fields.Assistant
		case "tool_call":
			target = &fields.ToolCall
		case "tool_result":
			target = &fields.ToolResult
		case "context_checkpoint":
			target = &fields.Checkpoint
		case "interrupted":
			target = &fields.Interrupted
		case "turn_completed":
			target = &fields.TurnCompleted
		}
		if target != nil {
			if known {
				return errors.New("fx event must contain one event type")
			}
			known = true
			if string(value) == "null" {
				return fmt.Errorf("fx %s event must be an object", key)
			}
			if err := json.Unmarshal(value, target); err != nil {
				return err
			}
		}
	}
	*e = fields
	return nil
}

type fxCount int

func (c *fxCount) UnmarshalJSON(data []byte) error {
	var count int
	if err := json.Unmarshal(data, &count); err != nil {
		return err
	}
	if count < 0 {
		return errors.New("negative fx token count")
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

func fxSchemaOK(version, minimum, maximum int, session, file string) bool {
	if version >= minimum && version <= maximum {
		return true
	}
	direction, supported := "newer", maximum
	if version < minimum {
		direction, supported = "older", minimum
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
	if !fxSchemaOK(metadata.Version, 4, 4, rawID, "session.json") {
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
		File: FileInfo{Path: filePath, Size: info.Size(), Mtime: info.ModTime().UnixNano()},
	}
	if metadata.Created != 0 {
		session.StartedAt = time.UnixMilli(metadata.Created).UTC()
	}
	if metadata.Updated != 0 {
		session.EndedAt = time.UnixMilli(metadata.Updated).UTC()
	}
	owner, err := readFxMetadata[fxOwner](filepath.Join(dir, "subagent", "owner.json"))
	if err != nil {
		return nil, nil, err
	}
	if owner != nil {
		if !fxSchemaOK(owner.Version, 0, 1, rawID, "owner.json") {
			return nil, nil, nil
		}
		if fxSessionIDValid(owner.Parent) {
			session.ParentSessionID, session.RelationshipType = "fx:"+owner.Parent, RelSubagent
		} else {
			log.Printf("fx session %s: ignoring invalid owner.json parent_id %q", rawID, owner.Parent)
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
			// Complete frames with duplicate names or invalid UTF-8 must fail rather than look torn.
			if !jsontext.Value(line).IsValid(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)) {
				var tail [1]byte
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
			return nil, nil, errors.New("invalid fx events.jsonl frame: event must be an object")
		}
		if !fxSchemaOK(frame.Version, 1, 3, rawID, "events.jsonl frame") {
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
		fxAppendEvent(&messages, frame.Event, timestamp, dir)
	}
	if err := reader.Err(); err != nil {
		return nil, nil, fmt.Errorf("read fx events: %w", err)
	}
	if reader.skippedOversized {
		return nil, nil, errors.New("fx event exceeds maximum line size")
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
	session.UsageEvents = fxUsageEvents(dir, rawID, session.ID, latestEventTime, session.StartedAt)
	// Cumulative session usage cannot establish a per-request context peak.
	session.TotalOutputTokens, session.HasTotalOutputTokens, _, _ = UsageEventTokenAggregate(session.UsageEvents)
	return session, messages, nil
}

// fxUsageEvents reads the legacy usage-v2.json. Bad content drops all usage and keeps the transcript.
func fxUsageEvents(dir, rawID, sessionID string, latest, started time.Time) []ParsedUsageEvent {
	usagePath := filepath.Join(dir, "usage-v2.json")
	usage, err := readFxMetadata[fxUsage](usagePath)
	if err != nil {
		// Unreadable or malformed usage must not block the transcript.
		log.Printf("skipping fx usage for session %s: %v", rawID, err)
		return nil
	}
	if usage == nil {
		return nil
	}
	// Resuming an old session adds events without touching usage-v2.json, so cap the date at its mtime.
	occurredAt := timeString(latest, started)
	if info, err := os.Stat(usagePath); err == nil && !latest.IsZero() && info.ModTime().Before(latest) {
		occurredAt = timeString(info.ModTime().UTC(), started)
	}
	skip := func(reason string) []ParsedUsageEvent {
		log.Printf("skipping fx usage for session %s: %s", rawID, reason)
		return nil
	}
	if usage.Snapshot == nil || usage.Snapshot.Models == nil {
		return skip("expected snapshot object and models array")
	}
	if usage.SessionID != nil && *usage.SessionID != rawID {
		return skip("session_id does not match session folder")
	}
	if usage.Version < 0 || usage.Version > 1 || usage.Snapshot.Version < 0 || usage.Snapshot.Version > 3 {
		return skip(fmt.Sprintf("unsupported schema_version %d (snapshot %d)", usage.Version, usage.Snapshot.Version))
	}
	var events []ParsedUsageEvent
	seen := make(map[string]bool)
	for _, model := range usage.Snapshot.Models {
		if model == nil || model.Model == "" {
			return skip("invalid model entry")
		}
		if seen[model.Model] {
			return skip("duplicate model entry")
		}
		seen[model.Model] = true
		events = append(events, ParsedUsageEvent{
			SessionID: sessionID, Source: "session", Model: model.Model,
			DedupKey: "session:" + sessionID + "|" + model.Model, OccurredAt: occurredAt,
			InputTokens: max(0, int(model.Input)-int(model.CacheRead)-int(model.CacheWrite)), OutputTokens: int(model.Output),
			CacheReadInputTokens: int(model.CacheRead), CacheCreationInputTokens: int(model.CacheWrite), ReasoningTokens: int(model.Reasoning),
		})
	}
	return events
}

// fxFullToolOutput reads a .txt result file larger than the preview; any failure keeps the preview.
func fxFullToolOutput(dir, ref string, storedBytes int64, previewLen int) (string, bool) {
	if !fxArtifactRefValid(ref) || storedBytes <= int64(previewLen) {
		return "", false
	}
	resultsDir := filepath.Join(dir, "tool-results")
	info, err := os.Lstat(resultsDir)
	if err != nil || info.Mode().Type() != os.ModeDir {
		return "", false
	}
	resultPath := filepath.Join(resultsDir, ref)
	info, err = os.Lstat(resultPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	f, err := openNoFollow(resultPath)
	if err != nil {
		return "", false
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", false
	}
	b, err := readPersistedToolResultBytes(f)
	if err != nil || !utf8.ValidString(persistedToolResultPrefix(b)) || len(b) <= previewLen || (int64(len(b)) != storedBytes && len(b) < maxPersistedToolResultSize) {
		return "", false
	}
	return persistedToolResultText(b), true
}

func fxArtifactRefValid(ref string) bool {
	return strings.HasSuffix(ref, ".txt") && filepath.Base(ref) == ref && !strings.ContainsAny(ref, `/\`)
}

func fxAppendEvent(messages *[]ParsedMessage, event *fxEvent, timestamp time.Time, dir string) {
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
		preview := gjson.ParseBytes(result.Preview)
		toolResult := ParsedToolResult{ToolUseID: result.ID, Source: "tool_result", Status: status, ContentLength: toolResultContentLength(preview)}
		if preview.Type != gjson.Null {
			toolResult.ContentRaw = preview.Raw
		}
		if full, ok := fxFullToolOutput(dir, result.ArtifactRef, result.StoredBytes, len(preview.Str)); ok {
			if raw, err := json.Marshal(full); err == nil {
				toolResult.ContentLength, toolResult.ContentRaw = len(full), string(raw)
			}
		}
		message.ToolResults = []ParsedToolResult{toolResult}
	case event.Checkpoint != nil:
		message.Role, message.Content = RoleSystem, event.Checkpoint.Summary
		message.IsSystem, message.IsCompactBoundary = true, true
		message.SourceSubtype = "compact_boundary"
	case event.Interrupted != nil:
		if event.Interrupted.Text == "" {
			return
		}
		message.Role, message.Content = RoleAssistant, event.Interrupted.Text
	default:
		return
	}
	*messages = append(*messages, message)
}
