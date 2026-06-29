package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// reasonixMessage represents a single line in a Reasonix JSONL
// transcript.
type reasonixMessage struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"`
	ToolCalls        []reasonixToolCall `json:"tool_calls"`
	ToolCallID       string             `json:"tool_call_id"`
}

// reasonixToolCall represents a tool call in a Reasonix message.
type reasonixToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// reasonixMetadata represents the .jsonl.meta sidecar file format.
type reasonixMetadata struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	Scope         string `json:"scope"`
	WorkspaceRoot string `json:"workspace_root"`
	TopicTitle    string `json:"topic_title"`
	Model         string `json:"model"`
}

// reasonixSessionBuilder accumulates state while scanning a
// Reasonix JSONL session file line by line.
type reasonixSessionBuilder struct {
	messages     []ParsedMessage
	firstMessage string
	startedAt    time.Time
	endedAt      time.Time
	sessionID    string
	ordinal      int
	model        string
}

func newReasonixSessionBuilder() *reasonixSessionBuilder {
	return &reasonixSessionBuilder{}
}

// processLine handles a single non-empty, valid JSON line.
func (b *reasonixSessionBuilder) processLine(line string) error {
	var msg reasonixMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return nil // skip invalid lines silently
	}

	if msg.Role == "" {
		return nil
	}

	roleName := strings.ToLower(msg.Role)
	if roleName == "tool" {
		return b.processToolResult(msg)
	}

	// Extract and normalize role
	var role RoleType
	switch roleName {
	case "user":
		role = RoleUser
	case "assistant":
		role = RoleAssistant
	default:
		// Skip unsupported roles (system, tool, etc.)
		return nil
	}

	// Normalize content
	content := strings.TrimSpace(msg.Content)

	// For assistant messages, prepend reasoning content if present
	if role == RoleAssistant && msg.ReasoningContent != "" {
		thinkBlock := "[Thinking]\n" + strings.TrimSpace(msg.ReasoningContent) + "\n[/Thinking]"
		if content != "" {
			content = thinkBlock + "\n\n" + content
		} else {
			content = thinkBlock
		}
	}

	// Extract tool calls
	hasToolUse := len(msg.ToolCalls) > 0
	var toolCalls []ParsedToolCall
	for _, tc := range msg.ToolCalls {
		toolCalls = append(toolCalls, ParsedToolCall{
			ToolUseID: tc.ID,
			ToolName:  tc.Name,
			Category:  NormalizeToolCategory(tc.Name),
			InputJSON: tc.Arguments,
		})
	}

	// Skip messages with no content and no tool calls
	if content == "" && !hasToolUse {
		return nil
	}

	// Update first message for user messages
	if role == RoleUser && b.firstMessage == "" && content != "" {
		b.firstMessage = truncate(
			strings.ReplaceAll(content, "\n", " "), 300,
		)
	}

	// Message ordering is tracked via ordinal; timestamps are
	// set from metadata or file mtime after parsing completes.

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          role,
		Content:       content,
		ContentLength: len(content),
		HasThinking:   msg.ReasoningContent != "",
		HasToolUse:    hasToolUse,
		ToolCalls:     toolCalls,
		Model:         b.model,
	})
	b.ordinal++

	return nil
}

func (b *reasonixSessionBuilder) processToolResult(
	msg reasonixMessage,
) error {
	if msg.ToolCallID == "" {
		return nil
	}

	content := msg.Content
	quoted, err := json.Marshal(content)
	if err != nil {
		return nil
	}

	b.messages = append(b.messages, ParsedMessage{
		Ordinal:       b.ordinal,
		Role:          RoleUser,
		Content:       "",
		ContentLength: len(content),
		ToolResults: []ParsedToolResult{{
			ToolUseID:     msg.ToolCallID,
			ContentRaw:    string(quoted),
			ContentLength: len(content),
		}},
	})
	b.ordinal++

	return nil
}

// parseReasonixSession parses a Reasonix JSONL session file.
// Returns (nil, nil, nil, nil) if the file doesn't exist or
// contains no user/assistant messages.
func parseReasonixSession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	lr := newLineReader(f, maxLineSize)
	b := newReasonixSessionBuilder()

	// Extract session ID from path
	b.sessionID = sessionIDFromReasonixPath(path)

	// Load metadata from .jsonl.meta sidecar if present.
	meta, err := loadReasonixMetadata(path)
	if err != nil {
		return nil, nil, nil, err
	}
	sessionName := ""
	cwd := ""
	project := ""
	if meta != nil {
		if meta.Model != "" {
			b.model = meta.Model
		}
		sessionName = strings.TrimSpace(meta.TopicTitle)
		cwd = strings.TrimSpace(meta.WorkspaceRoot)
		if cwd != "" {
			project = ExtractProjectFromCwd(cwd)
		}
		var gotStart, gotEnd bool
		var metaStart, metaEnd time.Time
		if meta.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, meta.CreatedAt); err == nil {
				metaStart = t
				gotStart = true
			}
		}
		if meta.UpdatedAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, meta.UpdatedAt); err == nil {
				metaEnd = t
				gotEnd = true
			}
		}
		if gotStart && gotEnd {
			b.startedAt = metaStart
			b.endedAt = metaEnd
		}
	}

	// Parse JSONL lines
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if line == "" {
			continue
		}
		_ = b.processLine(line)
	}

	if err := lr.Err(); err != nil {
		return nil, nil, nil,
			fmt.Errorf("reading reasonix %s: %w", path, err)
	}

	// Filter: require at least one message with content.
	hasContent := false
	for _, m := range b.messages {
		if m.Content != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return nil, nil, nil, nil
	}

	// Fall back to file mtime for sessions without metadata timestamps
	if b.startedAt.IsZero() {
		b.startedAt = info.ModTime()
	}
	if b.endedAt.IsZero() {
		b.endedAt = info.ModTime()
	}

	sessionID := "reasonix:" + b.sessionID

	userCount := 0
	for _, m := range b.messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentReasonix,
		Cwd:              cwd,
		FirstMessage:     b.firstMessage,
		SessionName:      sessionName,
		StartedAt:        b.startedAt,
		EndedAt:          b.endedAt,
		MessageCount:     len(b.messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	accumulateMessageTokenUsage(sess, b.messages)

	return sess, b.messages, nil, nil
}

// sessionIDFromReasonixPath extracts a session ID from a Reasonix
// file path. Handles project sessions, global sessions, subagents, and archive.
func sessionIDFromReasonixPath(path string) string {
	base := filepath.Base(path)

	// Strip .jsonl extension and .meta suffix if present
	if cut, ok := strings.CutSuffix(base, ".jsonl"); ok {
		base = cut
		if cut2, ok2 := strings.CutSuffix(base, ".meta"); ok2 {
			base = cut2
		}
	}

	// For project/global sessions, base is already the session ID
	// For subagents, base is like sa_20260612_105316_000000000_<hash>
	return base
}

// loadReasonixMetadata reads the .jsonl.meta sidecar file if it exists. A
// missing sidecar is valid, but an unreadable or malformed sidecar is returned
// as an error so sync does not overwrite previously parsed metadata with empty
// fields during partial writes.
func loadReasonixMetadata(transcriptPath string) (*reasonixMetadata, error) {
	metaPath := transcriptPath + ".meta"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read reasonix metadata %s: %w", metaPath, err)
	}

	var meta reasonixMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse reasonix metadata %s: %w", metaPath, err)
	}

	return &meta, nil
}

// discoverReasonixSessions discovers Reasonix sessions across
// four layouts: project sessions, global sessions, global subagents,
// and archive sessions.
func discoverReasonixSessions(reasonixDir string) []DiscoveredFile {
	if reasonixDir == "" {
		return nil
	}

	var files []DiscoveredFile

	// 1. Project sessions: {reasonixDir}/projects/{project}/sessions/*.jsonl
	projectsDir := filepath.Join(reasonixDir, "projects")
	if entries, err := os.ReadDir(projectsDir); err == nil {
		for _, projectEntry := range entries {
			if !projectEntry.IsDir() {
				continue
			}
			projectName := projectEntry.Name()
			sessionsDir := filepath.Join(
				projectsDir, projectName, "sessions",
			)
			if sessEntries, err := os.ReadDir(sessionsDir); err == nil {
				for _, sessEntry := range sessEntries {
					if sessEntry.IsDir() {
						// Check for {id}/{id}.jsonl inside
						sessionID := sessEntry.Name()
						candidate := filepath.Join(
							sessionsDir, sessionID,
							sessionID+".jsonl",
						)
						if _, err := os.Stat(candidate); err == nil {
							files = append(files, DiscoveredFile{
								Path:    candidate,
								Project: projectName,
								Agent:   AgentReasonix,
							})
						}
						continue
					}
					if strings.HasSuffix(sessEntry.Name(), ".jsonl") &&
						!strings.HasSuffix(sessEntry.Name(), ".meta") {
						files = append(files, DiscoveredFile{
							Path:    filepath.Join(sessionsDir, sessEntry.Name()),
							Project: projectName,
							Agent:   AgentReasonix,
						})
					}
				}
			}
		}
	}

	// 2. Global sessions: {reasonixDir}/sessions/*.jsonl
	globalSessDir := filepath.Join(reasonixDir, "sessions")
	if entries, err := os.ReadDir(globalSessDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				// Skip subagents directory here (handled separately)
				if entry.Name() == "subagents" {
					continue
				}
				// If it's a directory, it should contain {id}.jsonl
				sessionID := entry.Name()
				candidate := filepath.Join(
					globalSessDir, sessionID, sessionID+".jsonl",
				)
				if _, err := os.Stat(candidate); err == nil {
					files = append(files, DiscoveredFile{
						Path:  candidate,
						Agent: AgentReasonix,
					})
				}
			} else if strings.HasSuffix(entry.Name(), ".jsonl") &&
				!strings.HasSuffix(entry.Name(), ".meta") {
				// Bare {id}.jsonl files
				path := filepath.Join(globalSessDir, entry.Name())
				files = append(files, DiscoveredFile{
					Path:  path,
					Agent: AgentReasonix,
				})
			}
		}
	}

	// 3. Global subagents: {reasonixDir}/sessions/subagents/*.jsonl
	subagentsDir := filepath.Join(globalSessDir, "subagents")
	if entries, err := os.ReadDir(subagentsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() &&
				strings.HasSuffix(entry.Name(), ".jsonl") &&
				!strings.HasSuffix(entry.Name(), ".meta") {
				path := filepath.Join(subagentsDir, entry.Name())
				files = append(files, DiscoveredFile{
					Path:  path,
					Agent: AgentReasonix,
				})
			}
		}
	}

	// 4. Archive sessions: {reasonixDir}/archive/*.jsonl
	archiveDir := filepath.Join(reasonixDir, "archive")
	if entries, err := os.ReadDir(archiveDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() &&
				strings.HasSuffix(entry.Name(), ".jsonl") &&
				!strings.HasSuffix(entry.Name(), ".meta") {
				path := filepath.Join(archiveDir, entry.Name())
				files = append(files, DiscoveredFile{
					Path:  path,
					Agent: AgentReasonix,
				})
			}
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// findReasonixSourceFile locates a Reasonix session file by
// session ID. Searches project, global, subagent, and archive layouts.
func findReasonixSourceFile(reasonixDir, rawID string) string {
	if reasonixDir == "" || rawID == "" {
		return ""
	}

	// Try project sessions: {reasonixDir}/projects/{project}/sessions/{id}/{id}.jsonl
	projectsDir := filepath.Join(reasonixDir, "projects")
	if entries, err := os.ReadDir(projectsDir); err == nil {
		for _, projectEntry := range entries {
			if !projectEntry.IsDir() {
				continue
			}
			candidate := filepath.Join(
				projectsDir, projectEntry.Name(), "sessions",
				rawID, rawID+".jsonl",
			)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
			candidate = filepath.Join(
				projectsDir, projectEntry.Name(), "sessions",
				rawID+".jsonl",
			)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	// Try global sessions: {reasonixDir}/sessions/{id}/{id}.jsonl
	candidate := filepath.Join(
		reasonixDir, "sessions", rawID, rawID+".jsonl",
	)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// Try bare global: {reasonixDir}/sessions/{id}.jsonl
	candidate = filepath.Join(reasonixDir, "sessions", rawID+".jsonl")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// Try subagents: {reasonixDir}/sessions/subagents/{id}.jsonl
	candidate = filepath.Join(
		reasonixDir, "sessions", "subagents", rawID+".jsonl",
	)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// Try archive: {reasonixDir}/archive/{id}.jsonl
	candidate = filepath.Join(reasonixDir, "archive", rawID+".jsonl")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	return ""
}
