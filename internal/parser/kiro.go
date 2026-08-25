package parser

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Kiro JSONL message kinds.
const (
	kiroKindPrompt    = "Prompt"
	kiroKindAssistant = "AssistantMessage"
	kiroKindToolRes   = "ToolResults"
)

// kiroMeta holds fields from the companion .json metadata file.
type kiroMeta struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// discoverLegacyJSONL finds all .jsonl session files under a Kiro
// CLI sessions directory. Layout:
// <sessionsDir>/<uuid>.jsonl  (with companion <uuid>.json)
func (s kiroSourceSet) discoverLegacyJSONL(sessionsDir string) ([]DiscoveredFile, error) {
	var files []DiscoveredFile
	for _, dir := range []string{sessionsDir, filepath.Join(sessionsDir, "cli")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read Kiro legacy directory %s: %w", dir, err)
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if _, err := loadKiroMetaStrict(path); err != nil {
				return nil, err
			}
			regular, err := kiroRegularFileUnderRootChecked(sessionsDir, path)
			if err != nil {
				return nil, fmt.Errorf("probe Kiro legacy transcript %s: %w", path, err)
			}
			if regular {
				files = append(files, DiscoveredFile{Path: path, Agent: AgentKiro})
			}
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// legacySourceFile locates a legacy Kiro JSONL session file by its raw
// session ID (without the "kiro:" prefix).
func (s kiroSourceSet) legacySourceFile(sessionsDir, rawID string) string {
	if sessionsDir == "" || !IsValidSessionID(rawID) {
		return ""
	}
	for _, dir := range []string{sessionsDir, filepath.Join(sessionsDir, "cli")} {
		candidate := filepath.Join(dir, rawID+".jsonl")
		if _, err := os.Stat(candidate); err == nil && kiroRegularFileUnderRoot(sessionsDir, candidate) {
			return candidate
		}
	}
	return ""
}

// KiroSessionIDFromPath returns the logical raw session ID for a
// legacy JSONL-backed Kiro session, preferring companion metadata
// when present because the filename is only a storage detail.
func KiroSessionIDFromPath(jsonlPath string) string {
	if meta := loadKiroMeta(jsonlPath); meta != nil &&
		meta.SessionID != "" {
		return meta.SessionID
	}
	return strings.TrimSuffix(filepath.Base(jsonlPath), ".jsonl")
}

// loadKiroMeta reads the companion .json metadata file for a
// session JSONL file.
func loadKiroMeta(jsonlPath string) *kiroMeta {
	meta, _ := loadKiroMetaStrict(jsonlPath)
	return meta
}

func loadKiroMetaStrict(jsonlPath string) (*kiroMeta, error) {
	jsonPath := strings.TrimSuffix(jsonlPath, ".jsonl") + ".json"
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read Kiro legacy metadata %s: %w", jsonPath, err)
	}
	var m kiroMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode Kiro legacy metadata %s: %w", jsonPath, err)
	}
	return &m, nil
}

// parseLegacySession parses a Kiro CLI session from its JSONL file.
// Returns (nil, nil, nil) if the file doesn't exist or contains
// no user/assistant messages.
func (p *kiroProvider) parseLegacySession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	var messages []ParsedMessage
	var firstMessage string
	ordinal := 0

	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}

		kind := gjson.Get(line, "kind").Str
		data := gjson.Get(line, "data")

		switch kind {
		case kiroKindPrompt:
			content := kiroExtractText(data)
			if content == "" {
				continue
			}
			if firstMessage == "" {
				firstMessage = truncate(
					strings.ReplaceAll(content, "\n", " "), 300,
				)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       content,
				ContentLength: len(content),
			})
			ordinal++

		case kiroKindAssistant:
			text, toolCalls := kiroExtractAssistant(data)
			hasToolUse := len(toolCalls) > 0

			displayContent := text
			if hasToolUse && text == "" {
				displayContent = kiroFormatToolCalls(toolCalls)
			}
			if displayContent == "" && !hasToolUse {
				continue
			}

			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       displayContent,
				ContentLength: len(displayContent),
				HasToolUse:    hasToolUse,
				ToolCalls:     toolCalls,
			})
			ordinal++

		case kiroKindToolRes:
			results := kiroExtractToolResults(data)
			if len(results) == 0 {
				continue
			}
			messages = append(messages, ParsedMessage{
				Ordinal:     ordinal,
				Role:        RoleUser,
				ToolResults: results,
			})
			ordinal++
		}
	}

	if err := lr.Err(); err != nil {
		return nil, nil,
			fmt.Errorf("reading kiro %s: %w", path, err)
	}

	// Require at least one message with content.
	hasContent := false
	for _, m := range messages {
		if m.Content != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return nil, nil, nil
	}

	// Extract metadata from companion .json file.
	meta, err := loadKiroMetaStrict(path)
	if err != nil {
		return nil, nil, err
	}

	sessionID := strings.TrimSuffix(
		filepath.Base(path), ".jsonl",
	)

	var project, cwd string
	var startedAt, endedAt time.Time

	if meta != nil {
		if meta.SessionID != "" {
			sessionID = meta.SessionID
		}
		cwd = meta.Cwd
		if cwd != "" {
			project = ExtractProjectFromCwd(cwd)
		}
		if meta.Title != "" && firstMessage == "" {
			firstMessage = meta.Title
		}
		startedAt = parseTimestamp(meta.CreatedAt)
		endedAt = parseTimestamp(meta.UpdatedAt)
	}

	if project == "" {
		project = "unknown"
	}

	sessionID = "kiro:" + sessionID

	userCount := 0
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentKiro,
		Cwd:              cwd,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	return sess, messages, nil
}

type kiroCurrentMeta struct {
	Title          string   `json:"title"`
	CreatedAt      string   `json:"createdAt"`
	LastModifiedAt string   `json:"lastModifiedAt"`
	WorkspacePaths []string `json:"workspacePaths"`
}

func (p *kiroProvider) parseCurrentSession(path, sessionID, machine string) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	lr := newLineReader(f, maxLineSize)
	defer releaseLineReader(lr)
	var messages []ParsedMessage
	firstMessage := ""
	ordinal := 0
	var earliestMessage, latestMessage time.Time
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !gjson.Valid(line) {
			continue
		}
		payload := gjson.Get(line, "payload")
		timestamp := kiroCurrentMessageTimestamp(gjson.Get(line, "timestamp"))
		typ := payload.Get("type").Str
		content := strings.TrimSpace(payload.Get("content").Str)
		accepted := false
		switch typ {
		case "user", "assistant":
			if content == "" {
				continue
			}
			role := RoleUser
			if typ == "assistant" {
				role = RoleAssistant
			}
			if role == RoleUser && firstMessage == "" {
				firstMessage = truncate(strings.ReplaceAll(content, "\n", " "), 300)
			}
			messages = append(messages, ParsedMessage{Ordinal: ordinal, Role: role, Content: content, Timestamp: timestamp, ContentLength: len(content), Model: payload.Get("reasoningModelId").Str})
			accepted = true
			ordinal++
		case "tool_call":
			name, id := payload.Get("toolName").Str, payload.Get("toolCallId").Str
			if name == "" || id == "" {
				continue
			}
			call := ParsedToolCall{ToolUseID: id, ToolName: name, Category: NormalizeToolCategory(name), InputJSON: payload.Get("args").Raw}
			display := kiroFormatToolCalls([]ParsedToolCall{call})
			messages = append(messages, ParsedMessage{Ordinal: ordinal, Role: RoleAssistant, Content: display, Timestamp: timestamp, ContentLength: len(display), HasToolUse: true, ToolCalls: []ParsedToolCall{call}})
			accepted = true
			ordinal++
		case "tool_result":
			id := payload.Get("toolCallId").Str
			if id == "" {
				continue
			}
			raw := payload.Get("content").Raw
			messages = append(messages, ParsedMessage{Ordinal: ordinal, Role: RoleUser, Timestamp: timestamp, ToolResults: []ParsedToolResult{{ToolUseID: id, ContentRaw: raw, ContentLength: len(raw)}}})
			accepted = true
			ordinal++
		}
		if accepted && !timestamp.IsZero() {
			if earliestMessage.IsZero() || timestamp.Before(earliestMessage) {
				earliestMessage = timestamp
			}
			if latestMessage.IsZero() || timestamp.After(latestMessage) {
				latestMessage = timestamp
			}
		}
	}
	if err := lr.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading kiro %s: %w", path, err)
	}
	if len(messages) == 0 {
		return nil, nil, nil
	}
	meta := kiroCurrentMeta{}
	if sidecar, ok := kiroCurrentSidecarPath(p.sources.roots, path); ok {
		data, readErr := os.ReadFile(sidecar)
		if readErr != nil {
			return nil, nil, fmt.Errorf("read Kiro current metadata %s: %w", sidecar, readErr)
		}
		if unmarshalErr := json.Unmarshal(data, &meta); unmarshalErr != nil {
			return nil, nil, fmt.Errorf("decode Kiro current metadata %s: %w", sidecar, unmarshalErr)
		}
	}
	cwd := ""
	if len(meta.WorkspacePaths) > 0 {
		cwd = meta.WorkspacePaths[0]
	}
	project := ExtractProjectFromCwd(cwd)
	if project == "" {
		project = "unknown"
	}
	startedAt, endedAt := parseTimestamp(meta.CreatedAt), parseTimestamp(meta.LastModifiedAt)
	if startedAt.IsZero() {
		startedAt = earliestMessage
		if startedAt.IsZero() {
			startedAt = info.ModTime()
		}
	}
	if endedAt.IsZero() {
		endedAt = latestMessage
		if endedAt.IsZero() {
			endedAt = info.ModTime()
		}
	}
	userCount := 0
	for _, msg := range messages {
		if msg.Role == RoleUser && msg.Content != "" {
			userCount++
		}
	}
	return &ParsedSession{ID: "kiro:" + sessionID, Project: project, Machine: machine, Agent: AgentKiro, Cwd: cwd, FirstMessage: firstMessage, StartedAt: startedAt, EndedAt: endedAt, MessageCount: len(messages), UserMessageCount: userCount, SessionName: meta.Title, File: FileInfo{Path: path, Size: info.Size(), Mtime: info.ModTime().UnixNano()}}, messages, nil
}

func kiroCurrentMessageTimestamp(value gjson.Result) time.Time {
	if ts := parseTimestamp(value.Str); !ts.IsZero() {
		return ts
	}
	if value.Type == gjson.Number {
		millis := value.Int()
		if millis != 0 {
			return time.UnixMilli(millis).UTC()
		}
	}
	return time.Time{}
}

// kiroExtractText extracts concatenated text from a Kiro message's
// content array.
func kiroExtractText(data gjson.Result) string {
	var parts []string
	data.Get("content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("kind").Str == "text" {
			if t := strings.TrimSpace(block.Get("data").Str); t != "" {
				parts = append(parts, t)
			}
		}
		return true
	})
	return strings.Join(parts, "\n\n")
}

// kiroExtractAssistant extracts text and tool calls from an
// AssistantMessage's content array.
func kiroExtractAssistant(
	data gjson.Result,
) (string, []ParsedToolCall) {
	var textParts []string
	var toolCalls []ParsedToolCall

	data.Get("content").ForEach(func(_, block gjson.Result) bool {
		switch block.Get("kind").Str {
		case "text":
			if t := strings.TrimSpace(block.Get("data").Str); t != "" {
				textParts = append(textParts, t)
			}
		case "toolUse":
			tu := block.Get("data")
			name := tu.Get("name").Str
			if name == "" {
				return true
			}
			inputJSON := tu.Get("input").Raw
			cat := NormalizeToolCategory(name)
			displayName := name
			// Normalize kiro-cli "write" tool to Edit/Write based on command
			if name == "write" {
				cmd := tu.Get("input.command").Str
				if cmd == "strReplace" {
					displayName = "Edit"
					cat = "Edit"
				} else {
					displayName = "Write"
				}
			}
			toolCalls = append(toolCalls, ParsedToolCall{
				ToolUseID: tu.Get("toolUseId").Str,
				ToolName:  displayName,
				Category:  cat,
				InputJSON: inputJSON,
			})
		}
		return true
	})

	return strings.Join(textParts, "\n\n"), toolCalls
}

// kiroExtractToolResults extracts tool results from a ToolResults
// message's content array.
func kiroExtractToolResults(
	data gjson.Result,
) []ParsedToolResult {
	var results []ParsedToolResult
	data.Get("content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("kind").Str != "toolResult" {
			return true
		}
		tr := block.Get("data")
		toolUseID := tr.Get("toolUseId").Str
		if toolUseID == "" {
			return true
		}
		contentRaw := tr.Get("content").Raw
		results = append(results, ParsedToolResult{
			ToolUseID:     toolUseID,
			ContentLength: len(contentRaw),
			ContentRaw:    contentRaw,
		})
		return true
	})
	return results
}

// kiroFormatToolCalls formats tool calls for display when there
// is no accompanying text.
func kiroFormatToolCalls(calls []ParsedToolCall) string {
	var parts []string
	for _, tc := range calls {
		parts = append(parts,
			formatToolHeader(tc.Category, tc.ToolName))
	}
	return strings.Join(parts, "\n")
}
