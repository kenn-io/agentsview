package parser

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"
)

// kiroIDEDefaultDirs returns the platform-specific default
// directories for Kiro IDE session storage.
func kiroIDEDefaultDirs() []string {
	return []string{
		// macOS
		"Library/Application Support/Kiro/User/globalStorage/kiro.kiroagent",
		// Windows
		"AppData/Roaming/Kiro/User/globalStorage/kiro.kiroagent",
		// Linux
		".config/Kiro/User/globalStorage/kiro.kiroagent",
	}
}

// kiroIDEChat is the top-level structure of a .chat file.
type kiroIDEChat struct {
	ExecutionID string           `json:"executionId"`
	ActionID    string           `json:"actionId"`
	Chat        []kiroIDEMessage `json:"chat"`
	Metadata    kiroIDEMeta      `json:"metadata"`
}

type kiroIDEMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type kiroIDEMeta struct {
	ModelID    string `json:"modelId"`
	Workflow   string `json:"workflow"`
	WorkflowID string `json:"workflowId"`
	StartTime  int64  `json:"startTime"`
	EndTime    int64  `json:"endTime"`
}

// kiroIDESessionEntry is one entry in a workspace-sessions/*/sessions.json.
type kiroIDESessionEntry struct {
	SessionID          string `json:"sessionId"`
	Title              string `json:"title"`
	DateCreated        string `json:"dateCreated"`
	WorkspaceDirectory string `json:"workspaceDirectory"`
}

// kiroIDENewSession is the new-format session JSON stored in
// workspace-sessions/<b64-path>/<uuid>.json.
type kiroIDENewSession struct {
	SessionID          string                `json:"sessionId"`
	Title              string                `json:"title"`
	WorkspaceDirectory string                `json:"workspaceDirectory"`
	History            []kiroIDEHistoryEntry `json:"history"`
}

type kiroIDEHistoryEntry struct {
	Message     kiroIDEHistoryMessage `json:"message"`
	PromptLogs  []kiroIDEPromptLog    `json:"promptLogs"`
	ExecutionID string                `json:"executionId"`
}

type kiroIDEPromptLog struct {
	Completion string `json:"completion"`
}

type kiroIDEHistoryMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	ID      string `json:"id"`
}

// kiroIDEExecAction represents an action in an execution log.
type kiroIDEExecAction struct {
	ActionID   string              `json:"actionId"`
	ActionType string              `json:"actionType"`
	Input      kiroIDEActionInput  `json:"input"`
	Output     kiroIDEActionOutput `json:"output"`
}

type kiroIDEActionInput struct {
	File            string `json:"file"`
	OriginalContent string `json:"originalContent"`
	ModifiedContent string `json:"modifiedContent"`
}

type kiroIDEActionOutput struct {
	Message string `json:"message"`
}

// DiscoverKiroIDESessions finds all session files under the
// Kiro IDE globalStorage directory. It scans both:
//   - <dir>/<workspace-hash>/<execution-hash>.chat  (old format)
//   - <dir>/workspace-sessions/<b64-path>/<uuid>.json (new format)
func DiscoverKiroIDESessions(dir string) []DiscoveredFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile

	for _, wsEntry := range entries {
		if !wsEntry.IsDir() {
			continue
		}
		name := wsEntry.Name()
		if name == "default" || name == "dev_data" ||
			name == "index" || name == "workspace-sessions" ||
			strings.HasPrefix(name, ".") {
			continue
		}

		wsDir := filepath.Join(dir, name)
		chatFiles, err := os.ReadDir(wsDir)
		if err != nil {
			continue
		}
		for _, cf := range chatFiles {
			if cf.IsDir() ||
				!strings.HasSuffix(cf.Name(), ".chat") {
				continue
			}
			files = append(files, DiscoveredFile{
				Path:  filepath.Join(wsDir, cf.Name()),
				Agent: AgentKiroIDE,
			})
		}
	}

	// Scan workspace-sessions for new-format session JSONs.
	wsSessionsDir := filepath.Join(dir, "workspace-sessions")
	wsDirs, err := os.ReadDir(wsSessionsDir)
	if err == nil {
		for _, wsEntry := range wsDirs {
			if !wsEntry.IsDir() {
				continue
			}
			wsDir := filepath.Join(wsSessionsDir, wsEntry.Name())
			jsonFiles, err := os.ReadDir(wsDir)
			if err != nil {
				continue
			}
			for _, jf := range jsonFiles {
				name := jf.Name()
				if name == "sessions.json" ||
					!strings.HasSuffix(name, ".json") {
					continue
				}
				files = append(files, DiscoveredFile{
					Path:  filepath.Join(wsDir, name),
					Agent: AgentKiroIDE,
				})
			}
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindKiroIDESourceFile locates a Kiro IDE session file by
// raw session ID. Supports both formats:
//   - Old: "<workspace-hash>:<filename-hash>" → .chat file
//   - New: "<uuid>" → workspace-sessions/*/<uuid>.json
func FindKiroIDESourceFile(dir, rawID string) string {
	cleanDir := filepath.Clean(dir)

	// Old format: <workspace-hash>:<filename-hash>
	wsHash, fileHash, ok := strings.Cut(rawID, ":")
	if ok && IsValidSessionID(wsHash) && IsValidSessionID(fileHash) {
		candidate := filepath.Join(dir, wsHash, fileHash+".chat")
		if abs, err := filepath.Abs(candidate); err == nil &&
			strings.HasPrefix(abs, cleanDir) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}

	// New format: rawID is a UUID, file is at
	// workspace-sessions/<b64-path>/<uuid>.json
	if !IsValidSessionID(rawID) {
		return ""
	}
	wsSessionsDir := filepath.Join(dir, "workspace-sessions")
	wsDirs, err := os.ReadDir(wsSessionsDir)
	if err != nil {
		return ""
	}
	for _, wsEntry := range wsDirs {
		if !wsEntry.IsDir() {
			continue
		}
		candidate := filepath.Join(
			wsSessionsDir, wsEntry.Name(), rawID+".json",
		)
		abs, err := filepath.Abs(candidate)
		if err != nil || !strings.HasPrefix(abs, cleanDir) {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// isKiroIDESystemMessage returns true for system prompt and
// rules messages that should not be shown as user content.
func isKiroIDESystemMessage(content string) bool {
	return strings.HasPrefix(content, "# System Prompt") ||
		strings.HasPrefix(content, "# Identity") ||
		strings.HasPrefix(content, "<identity>") ||
		strings.HasPrefix(content, "## Included Rules") ||
		strings.HasPrefix(content, "You are operating in a workspace")
}

// ParseKiroIDESession parses a Kiro IDE session file.
// Supports both old (.chat) and new (.json) formats.
// Returns (nil, nil, nil) if the file doesn't exist or
// contains no meaningful messages.
func ParseKiroIDESession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	if strings.HasSuffix(path, ".json") {
		return parseKiroIDENewFormat(path, machine)
	}
	return parseKiroIDEChatFormat(path, machine)
}

// parseKiroIDENewFormat parses the new workspace-sessions
// JSON format with history array and structured content.
func parseKiroIDENewFormat(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	var sess kiroIDENewSession
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, nil, nil
	}

	if len(sess.History) == 0 {
		return nil, nil, nil
	}

	// Build execution log index for resolving model output.
	execDir := kiroIDEExecLogDir(path)
	execIndex := kiroIDEBuildExecIndex(execDir)

	var messages []ParsedMessage
	var firstMessage string
	ordinal := 0

	for _, h := range sess.History {
		msg := h.Message
		role := msg.Role
		content := kiroIDEExtractText(msg.Content)

		switch role {
		case "user":
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

		case "assistant":
			// Resolve model output from execution log.
			resolved, toolCalls := kiroIDEResolveAssistant(
				h, execIndex,
			)
			if resolved == "" {
				resolved = content
			}
			// Fall back to concatenated prompt log completions
			// when execution logs are missing.
			if resolved == "" {
				var parts []string
				for _, pl := range h.PromptLogs {
					t := strings.TrimSpace(pl.Completion)
					if t != "" {
						parts = append(parts, t)
					}
				}
				resolved = strings.Join(parts, "\n\n")
			}
			hasToolUse := len(toolCalls) > 0
			if resolved == "" && !hasToolUse {
				continue
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       resolved,
				ContentLength: len(resolved),
				HasToolUse:    hasToolUse,
				ToolCalls:     toolCalls,
			})
			ordinal++

		case "tool":
			// Tool results are consumed by the assistant
			// message; skip to avoid blank transcript rows.
			continue
		}
	}

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

	sessionID := "kiro-ide:" + sess.SessionID
	if sessionID == "kiro-ide:" {
		sessionID = "kiro-ide:" + strings.TrimSuffix(
			filepath.Base(path), ".json",
		)
	}

	project := "unknown"
	if sess.WorkspaceDirectory != "" {
		if p := ExtractProjectFromCwd(
			sess.WorkspaceDirectory,
		); p != "" {
			project = p
		}
	}

	userCount := 0
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	title := sess.Title
	if title == "" {
		title = firstMessage
	}

	ps := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentKiroIDE,
		SessionName:      title,
		FirstMessage:     firstMessage,
		StartedAt:        info.ModTime(),
		EndedAt:          info.ModTime(),
		MessageCount:     len(messages),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  path,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixNano(),
		},
	}

	return ps, messages, nil
}

// kiroIDEExtractText extracts text from content that can be
// either a string or a list of content blocks.
func kiroIDEExtractText(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "text" {
				if text, ok := block["text"].(string); ok {
					t := strings.TrimSpace(text)
					if t != "" {
						parts = append(parts, t)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func parseKiroIDEChatFormat(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	var chat kiroIDEChat
	if err := json.Unmarshal(data, &chat); err != nil {
		return nil, nil, nil // malformed, skip
	}

	if len(chat.Chat) == 0 {
		return nil, nil, nil
	}

	var messages []ParsedMessage
	var firstMessage string
	ordinal := 0

	for _, m := range chat.Chat {
		content := strings.TrimSpace(m.Content)

		switch m.Role {
		case "human":
			if isKiroIDESystemMessage(content) {
				continue
			}
			if content == "" {
				continue
			}
			// Strip <kiro-ide-message> wrapper
			if after, ok := strings.CutPrefix(content, "<kiro-ide-message>"); ok {
				content = after
				content = strings.TrimSuffix(content, "</kiro-ide-message>")
				content = strings.TrimSpace(content)
			}
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

		case "bot":
			if content == "" ||
				content == "I will follow these instructions." {
				continue
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleAssistant,
				Content:       content,
				ContentLength: len(content),
				Model:         chat.Metadata.ModelID,
			})
			ordinal++

		case "tool":
			// Tool results are consumed by the assistant
			// message; skip to avoid blank transcript rows.
			continue
		}
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

	// Build session ID from workspace dir hash + file hash.
	wsHash := filepath.Base(filepath.Dir(path))
	fileHash := strings.TrimSuffix(filepath.Base(path), ".chat")
	sessionID := "kiro-ide:" + wsHash + ":" + fileHash

	var startedAt, endedAt time.Time
	if chat.Metadata.StartTime > 0 {
		startedAt = time.UnixMilli(chat.Metadata.StartTime)
	} else {
		startedAt = info.ModTime()
	}
	if chat.Metadata.EndTime > 0 {
		endedAt = time.UnixMilli(chat.Metadata.EndTime)
	} else {
		endedAt = info.ModTime()
	}

	// Derive project from the workspace directory path.
	project := kiroIDEProjectFromPath(path)

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
		Agent:            AgentKiroIDE,
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

// kiroIDEExecLogDir returns the execution log directory for a
// session JSON. Layout:
//
//	session: <base>/workspace-sessions/<b64>/<uuid>.json
//	exec logs: <base>/<ws-hash>/414d1636299d2b9e4ce7e17fb11f63e9/
//
// We need the workspace path to compute ws-hash. Read it from
// the sibling sessions.json.
const kiroIDEExecSubdir = "414d1636299d2b9e4ce7e17fb11f63e9"

func kiroIDEExecLogDir(sessionPath string) string {
	wsSessionDir := filepath.Dir(sessionPath)
	sjPath := filepath.Join(wsSessionDir, "sessions.json")
	sjData, err := os.ReadFile(sjPath)
	if err != nil {
		return ""
	}
	var entries []kiroIDESessionEntry
	if err := json.Unmarshal(sjData, &entries); err != nil ||
		len(entries) == 0 {
		return ""
	}
	wsPath := entries[0].WorkspaceDirectory
	if wsPath == "" {
		return ""
	}
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(wsPath)))[:32]
	base := filepath.Dir(filepath.Dir(wsSessionDir))
	return filepath.Join(base, h, kiroIDEExecSubdir)
}

// kiroIDEBuildExecIndex builds a map from executionId to file
// path for all execution logs in the directory.
func kiroIDEBuildExecIndex(dir string) map[string]string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	idx := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		// Read enough to find executionId near the top of
		// the JSON object. 1 KiB covers typical metadata
		// fields that may precede it.
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		buf := make([]byte, 1024)
		n, _ := f.Read(buf)
		f.Close()
		if n == 0 {
			continue
		}
		// Quick extract: {"executionId":"<uuid>",...}
		if i := strings.Index(string(buf[:n]),
			`"executionId"`); i >= 0 {
			rest := string(buf[i+len(`"executionId"`) : n])
			// skip `: "`
			if j := strings.Index(rest, `"`); j >= 0 {
				rest = rest[j+1:]
				if before, _, ok := strings.Cut(rest, `"`); ok {
					idx[before] = path
				}
			}
		}
	}
	return idx
}

// kiroIDEResolveAssistant extracts the model's text output
// and tool calls from the execution log referenced by an
// assistant history entry.
func kiroIDEResolveAssistant(
	h kiroIDEHistoryEntry, execIndex map[string]string,
) (string, []ParsedToolCall) {
	execID := h.ExecutionID
	if execID == "" || execIndex == nil {
		return "", nil
	}
	path, ok := execIndex[execID]
	if !ok {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}

	var exec struct {
		Actions []kiroIDEExecAction `json:"actions"`
	}
	if err := json.Unmarshal(data, &exec); err != nil {
		return "", nil
	}

	var textParts []string
	var toolCalls []ParsedToolCall
	for _, a := range exec.Actions {
		switch a.ActionType {
		case "say":
			if a.Output.Message != "" {
				textParts = append(textParts, a.Output.Message)
			}
		case "replace":
			if a.Input.File != "" {
				diff := kiroIDEComputeDiff(a.Input)
				toolCalls = append(toolCalls, ParsedToolCall{
					ToolUseID: a.ActionID,
					ToolName:  "Edit",
					Category:  "Edit",
					InputJSON: diff,
				})
			}
		case "create":
			if a.Input.File != "" {
				m := map[string]string{"file": a.Input.File}
				if a.Input.ModifiedContent != "" {
					m["content"] = a.Input.ModifiedContent
				}
				inputJSON, _ := json.Marshal(m)
				toolCalls = append(toolCalls, ParsedToolCall{
					ToolUseID: a.ActionID,
					ToolName:  "Write",
					Category:  "Write",
					InputJSON: string(inputJSON),
				})
			}
		case "readCode":
			if a.Input.File != "" {
				inputJSON, _ := json.Marshal(a.Input)
				toolCalls = append(toolCalls, ParsedToolCall{
					ToolUseID: a.ActionID,
					ToolName:  "readCode",
					Category:  "Read",
					InputJSON: string(inputJSON),
				})
			}
		}
	}
	return strings.Join(textParts, "\n\n"), toolCalls
}

// kiroIDEComputeDiff produces a JSON object with "file" and
// "diff" (unified diff string) from a replace action's input.
func kiroIDEComputeDiff(input kiroIDEActionInput) string {
	if input.OriginalContent == "" && input.ModifiedContent == "" {
		j, _ := json.Marshal(map[string]string{
			"file": input.File,
		})
		return string(j)
	}

	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(input.OriginalContent),
		B:        difflib.SplitLines(input.ModifiedContent),
		FromFile: "a/" + input.File,
		ToFile:   "b/" + input.File,
		Context:  3,
	}
	text, err := difflib.GetUnifiedDiffString(diff)
	if err != nil || text == "" {
		j, _ := json.Marshal(map[string]string{
			"file": input.File,
		})
		return string(j)
	}

	j, _ := json.Marshal(map[string]string{
		"file": input.File,
		"diff": text,
	})
	return string(j)
}

// kiroIDEProjectFromPath resolves the project name for a Kiro
// IDE session. The workspace dir hash is sha256(path)[:32].
// We reverse-lookup by scanning workspace-sessions/*/sessions.json
// for a workspaceDirectory whose sha256 matches the dir hash.
func kiroIDEProjectFromPath(chatPath string) string {
	wsHash := filepath.Base(filepath.Dir(chatPath))
	base := filepath.Dir(filepath.Dir(chatPath))
	wsSessionsDir := filepath.Join(base, "workspace-sessions")

	entries, err := os.ReadDir(wsSessionsDir)
	if err != nil {
		return "unknown"
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sjPath := filepath.Join(
			wsSessionsDir, entry.Name(), "sessions.json",
		)
		sjData, err := os.ReadFile(sjPath)
		if err != nil {
			continue
		}
		var sessions []kiroIDESessionEntry
		if err := json.Unmarshal(sjData, &sessions); err != nil ||
			len(sessions) == 0 {
			continue
		}
		wsDir := sessions[0].WorkspaceDirectory
		if wsDir == "" {
			continue
		}
		h := fmt.Sprintf("%x", sha256.Sum256([]byte(wsDir)))[:32]
		if h == wsHash {
			if p := ExtractProjectFromCwd(wsDir); p != "" {
				return p
			}
		}
	}
	return "unknown"
}
