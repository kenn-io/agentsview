package parser

import (
	"encoding/json"
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
func (s kiroSourceSet) discoverLegacyJSONL(sessionsDir string) []DiscoveredFile {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		files = append(files, DiscoveredFile{
			Path:  filepath.Join(sessionsDir, name),
			Agent: AgentKiro,
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// legacySourceFile locates a legacy Kiro JSONL session file by its raw
// session ID (without the "kiro:" prefix).
func (s kiroSourceSet) legacySourceFile(sessionsDir, rawID string) string {
	if sessionsDir == "" || !IsValidSessionID(rawID) {
		return ""
	}
	candidate := filepath.Join(sessionsDir, rawID+".jsonl")
	if abs, err := filepath.Abs(candidate); err != nil || !strings.HasPrefix(abs, filepath.Clean(sessionsDir)) {
		return ""
	}
	if _, err := os.Stat(candidate); err != nil {
		return ""
	}
	return candidate
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
	jsonPath := strings.TrimSuffix(jsonlPath, ".jsonl") + ".json"
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil
	}
	var m kiroMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return &m
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
	meta := loadKiroMeta(path)

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
