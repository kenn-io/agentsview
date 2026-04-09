package parser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// vscodeCopilotSession is the top-level JSON structure of a
// VSCode Copilot chat session file (chatSessions/<uuid>.json).
type vscodeCopilotSession struct {
	Version         int                    `json:"version"`
	SessionID       string                 `json:"sessionId"`
	CreationDate    jsonMillis             `json:"creationDate"`
	LastMessageDate jsonMillis             `json:"lastMessageDate"`
	CustomTitle     string                 `json:"customTitle"`
	Requests        []vscodeCopilotRequest `json:"requests"`
}

// jsonMillis is a unix-millisecond timestamp that
// unmarshals from a JSON number.
type jsonMillis int64

func (m jsonMillis) Time() time.Time {
	if m == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(m))
}

// vscodeCopilotRequest is one turn (user prompt + response).
type vscodeCopilotRequest struct {
	RequestID string               `json:"requestId"`
	Message   vscodeCopilotMessage `json:"message"`
	Response  []json.RawMessage    `json:"response"`
	Agent     *vscodeCopilotAgent  `json:"agent,omitempty"`
	ModelID   string               `json:"modelId"`
	Timestamp jsonMillis           `json:"timestamp"`
	Result    *vscodeCopilotResult `json:"result,omitempty"`
	FollowUps []json.RawMessage    `json:"followups,omitempty"`
}

// vscodeCopilotMessage is the user prompt.
type vscodeCopilotMessage struct {
	Text  string          `json:"text"`
	Parts json.RawMessage `json:"parts,omitempty"`
}

// vscodeCopilotAgent identifies the agent that handled the request.
type vscodeCopilotAgent struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	FullName    string          `json:"fullName"`
	ExtensionID json.RawMessage `json:"extensionId,omitempty"`
}

// vscodeCopilotResult holds timing and metadata.
type vscodeCopilotResult struct {
	Timings  *vscodeCopilotTimings `json:"timings,omitempty"`
	Metadata json.RawMessage       `json:"metadata,omitempty"`
}

type vscodeCopilotTimings struct {
	FirstProgress int64 `json:"firstProgress"`
	TotalElapsed  int64 `json:"totalElapsed"`
}

// vscodeCopilotResponseItem is a single element of the
// response array, with flexible typing.
type vscodeCopilotResponseItem struct {
	Kind              string          `json:"kind,omitempty"`
	Value             string          `json:"value,omitempty"`
	ToolID            string          `json:"toolId,omitempty"`
	ToolCallID        string          `json:"toolCallId,omitempty"`
	IsConfirmed       bool            `json:"isConfirmed,omitempty"`
	IsComplete        bool            `json:"isComplete,omitempty"`
	InvocationMessage json.RawMessage `json:"invocationMessage,omitempty"`
	PastTenseMessage  json.RawMessage `json:"pastTenseMessage,omitempty"`
	ToolName          string          `json:"toolName,omitempty"`
	InlineReference   json.RawMessage `json:"inlineReference,omitempty"`
	ToolSpecificData  json.RawMessage `json:"toolSpecificData,omitempty"`
}

// vscodeCopilotToolData holds terminal-specific tool data.
type vscodeCopilotToolData struct {
	Kind     string `json:"kind"`
	Language string `json:"language,omitempty"`
	Command  string `json:"command,omitempty"`
}

// vscodeCopilotInvocationMsg holds a structured invocation message.
type vscodeCopilotInvocationMsg struct {
	Value string `json:"value"`
}

// vscodeCopilotWorkspace holds the workspace.json manifest.
type vscodeCopilotWorkspace struct {
	Folder    string `json:"folder"`
	Workspace string `json:"workspace"`
}

// ParseVSCodeCopilotSession parses a VSCode Copilot chat
// session file (.json or .jsonl). Returns (nil, nil, nil)
// if the file is empty or contains no meaningful content.
func ParseVSCodeCopilotSession(
	path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat %s: %w", path, err)
	}

	var data []byte
	if strings.HasSuffix(path, ".jsonl") {
		data, err = reconstructJSONL(path)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, nil, nil
	}

	sess, msgs, err := parseVSCodeCopilotData(
		data, path, project, machine,
	)
	if err != nil {
		return nil, nil, err
	}
	if sess == nil {
		return nil, nil, nil
	}

	sess.Agent = AgentVSCodeCopilot
	sess.ID = "vscode-copilot:" + strings.TrimPrefix(
		sess.ID, "vscode-copilot:",
	)
	sess.File = FileInfo{
		Path:  path,
		Size:  info.Size(),
		Mtime: info.ModTime().UnixNano(),
	}

	return sess, msgs, nil
}

// parseVSCodeCopilotData parses VSCode-style chat session JSON
// data. Used by both VSCode Copilot and Positron parsers since
// the formats are identical.
func parseVSCodeCopilotData(
	data []byte, path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	var session vscodeCopilotSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, nil, fmt.Errorf(
			"unmarshal %s: %w", path, err,
		)
	}

	if len(session.Requests) == 0 {
		return nil, nil, nil
	}

	var messages []ParsedMessage
	var firstMessage string
	ordinal := 0

	for _, req := range session.Requests {
		// User message
		text := strings.TrimSpace(req.Message.Text)
		if text != "" {
			if firstMessage == "" {
				firstMessage = truncate(
					strings.ReplaceAll(text, "\n", " "), 300,
				)
			}
			messages = append(messages, ParsedMessage{
				Ordinal:       ordinal,
				Role:          RoleUser,
				Content:       text,
				Timestamp:     req.Timestamp.Time(),
				ContentLength: len(text),
			})
			ordinal++
		}

		// Assistant response: parse response items
		respText, toolCalls := parseVSCodeCopilotResponse(
			req.Response,
		)

		hasToolUse := len(toolCalls) > 0
		displayContent := respText
		if hasToolUse {
			toolText := formatVSCodeCopilotToolCalls(toolCalls)
			if respText == "" {
				displayContent = toolText
			} else {
				displayContent = toolText + "\n\n" + respText
			}
		}

		if displayContent == "" && !hasToolUse {
			continue
		}

		messages = append(messages, ParsedMessage{
			Ordinal:       ordinal,
			Role:          RoleAssistant,
			Content:       displayContent,
			Timestamp:     req.Timestamp.Time(),
			HasToolUse:    hasToolUse,
			ContentLength: len(displayContent),
			ToolCalls:     toolCalls,
		})
		ordinal++
	}

	if len(messages) == 0 {
		return nil, nil, nil
	}

	sessionID := session.SessionID
	if sessionID == "" {
		// Fall back to filename (strip .json or .jsonl)
		base := filepath.Base(path)
		sessionID = strings.TrimSuffix(
			strings.TrimSuffix(base, ".jsonl"), ".json",
		)
	}

	// Use customTitle as first message if we have no user text
	if firstMessage == "" && session.CustomTitle != "" {
		firstMessage = session.CustomTitle
	}

	userCount := 0
	for _, m := range messages {
		if m.Role == RoleUser && m.Content != "" {
			userCount++
		}
	}

	startedAt := session.CreationDate.Time()
	endedAt := session.LastMessageDate.Time()
	if endedAt.IsZero() && len(session.Requests) > 0 {
		last := session.Requests[len(session.Requests)-1]
		endedAt = last.Timestamp.Time()
	}

	sess := &ParsedSession{
		ID:               sessionID,
		Project:          project,
		Machine:          machine,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: userCount,
	}

	return sess, messages, nil
}

// parseVSCodeCopilotResponse extracts text and tool calls
// from the response items array.
func parseVSCodeCopilotResponse(
	raw []json.RawMessage,
) (string, []ParsedToolCall) {
	var textParts []string
	var toolCalls []ParsedToolCall

	for _, r := range raw {
		var item vscodeCopilotResponseItem
		if err := json.Unmarshal(r, &item); err != nil {
			continue
		}

		switch item.Kind {
		case "toolInvocationSerialized":
			if item.ToolID != "" {
				tc := ParsedToolCall{
					ToolUseID: item.ToolCallID,
					ToolName:  item.ToolID,
					Category: NormalizeToolCategory(
						normalizeVSCodeToolName(item.ToolID),
					),
				}
				tc.InputJSON = extractVSCopilotInputJSON(
					item.InvocationMessage,
					item.PastTenseMessage,
					item.ToolSpecificData,
				)
				toolCalls = append(toolCalls, tc)
			}
		case "prepareToolInvocation":
			// Skip, the actual invocation comes later.
		case "inlineReference", "undoStop",
			"codeblockUri", "textEditGroup":
			// Skip non-text items.
		case "":
			// Items without a kind are markdown text
			if item.Value != "" {
				textParts = append(textParts, item.Value)
			}
		default:
			// Unknown kind, try to extract value
			if item.Value != "" {
				textParts = append(textParts, item.Value)
			}
		}
	}

	text := strings.TrimSpace(strings.Join(textParts, ""))
	return text, toolCalls
}

// normalizeVSCodeToolName maps VSCode Copilot tool IDs to
// names that the taxonomy can categorize.
func normalizeVSCodeToolName(toolID string) string {
	switch toolID {
	// File reading
	case "copilot_readFile",
		"copilot_getNotebookSummary",
		"copilot_readNotebookCellOutput",
		"copilot_getVSCodeAPI",
		"copilot_getChangedFiles",
		"copilot_listCodeUsages",
		"copilot_getErrors":
		return "read_file"

	// Editing
	case "copilot_replaceString",
		"copilot_multiReplaceString",
		"copilot_applyPatch",
		"copilot_editNotebook",
		"vscode_editFile_internal":
		return "edit_file"

	// File insertion / creation
	case "copilot_insertEdit":
		return "edit_file"
	case "copilot_createFile",
		"copilot_createDirectory":
		return "create_file"
	case "copilot_deleteFile":
		return "write"

	// Terminal / shell execution
	case "copilot_runInTerminal",
		"copilot_runTerminalLastCommand",
		"copilot_runCommand",
		"copilot_terminalCommand",
		"copilot_getTerminalOutput",
		"copilot_getTerminalLastCommand",
		"copilot_getTerminalSelection",
		"copilot_runTests",
		"copilot_runNotebookCell",
		"copilot_runVscodeCommand",
		"run_in_terminal",
		"runTests",
		"terminal_last_command",
		"get_terminal_output":
		return "shell"

	// Search / grep
	case "copilot_searchFiles",
		"copilot_findFilesByName",
		"copilot_findTextInFiles",
		"copilot_searchCodebase":
		return "grep"

	// Directory listing
	case "copilot_listDir",
		"copilot_listDirectory",
		"copilot_findFiles":
		return "glob"

	// Web
	case "copilot_fetchWebPage",
		"vscode_fetchWebPage_internal",
		"copilot_openSimpleBrowser":
		return "read_web_page"

	// GitHub
	case "copilot_githubRepo":
		return "read_file"

	// Subagent
	case "runSubagent":
		return "Task"

	// Todo
	case "manage_todo_list":
		return "Task"

	// Thinking (treated as tool)
	case "copilot_think":
		return "Tool"

	default:
		return toolID
	}
}

func formatVSCodeCopilotToolCalls(
	calls []ParsedToolCall,
) string {
	var parts []string
	for _, tc := range calls {
		header := formatToolHeader(tc.Category, tc.ToolName)
		body := extractVSCopilotToolBody(tc)
		if body != "" {
			parts = append(parts, header+"\n"+body)
		} else {
			parts = append(parts, header)
		}
	}
	return strings.Join(parts, "\n\n")
}

// extractVSCopilotToolBody returns a human-readable body
// line for the tool block, derived from InputJSON.
func extractVSCopilotToolBody(tc ParsedToolCall) string {
	if tc.InputJSON == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(
		[]byte(tc.InputJSON), &m,
	); err != nil {
		return ""
	}
	if cmd, ok := m["command"].(string); ok && cmd != "" {
		return "$ " + cmd
	}
	if msg, ok := m["message"].(string); ok && msg != "" {
		return msg
	}
	return ""
}

// extractVSCopilotInputJSON builds an InputJSON string from
// the invocationMessage and toolSpecificData fields.
func extractVSCopilotInputJSON(
	invocationMsg, pastTenseMsg, toolData json.RawMessage,
) string {
	result := make(map[string]any)

	// Extract message from invocationMessage (string or object)
	msg := extractInvocationText(pastTenseMsg)
	if msg == "" {
		msg = extractInvocationText(invocationMsg)
	}
	if msg != "" {
		result["message"] = msg
	}

	// Extract command from toolSpecificData
	if len(toolData) > 0 {
		var td vscodeCopilotToolData
		if err := json.Unmarshal(toolData, &td); err == nil {
			if td.Command != "" {
				result["command"] = td.Command
			}
		}
	}

	if len(result) == 0 {
		return ""
	}
	data, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	return string(data)
}

// extractInvocationText extracts a human-readable string
// from an invocationMessage field which can be a plain
// string or a {"value": "..."} object.
func extractInvocationText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try object with value field
	var obj vscodeCopilotInvocationMsg
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Value
	}
	return ""
}

// ReadVSCodeWorkspaceManifest reads the workspace.json file
// in a workspaceStorage hash directory and extracts the
// project folder path.
func ReadVSCodeWorkspaceManifest(hashDir string) string {
	path := filepath.Join(hashDir, "workspace.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	var ws vscodeCopilotWorkspace
	if err := json.Unmarshal(data, &ws); err != nil {
		return ""
	}

	uri := ws.Folder
	if uri == "" {
		uri = ws.Workspace
	}
	if uri == "" {
		return ""
	}

	// Extract path from file:// URI
	return extractProjectFromURI(uri)
}

// extractProjectFromURI extracts a human-readable project
// name from a file URI like "file:///Users/dev/projects/myapp".
func extractProjectFromURI(uri string) string {
	path := strings.TrimPrefix(uri, "file://")
	if path == uri {
		// Not a file URI, return as-is
		return filepath.Base(uri)
	}

	// On Windows the path might start with /C:/ - trim the
	// leading slash for windows paths.
	if len(path) > 2 && path[0] == '/' &&
		path[2] == ':' {
		path = path[1:]
	}

	return filepath.Base(path)
}

// jsonlOp represents a single operation in a VSCode JSONL
// session operation log.
type jsonlOp struct {
	Kind int               `json:"kind"`
	K    []json.RawMessage `json:"k,omitempty"`
	V    json.RawMessage   `json:"v,omitempty"`
	I    *int              `json:"i,omitempty"`
}

// reconstructJSONL reads a VSCode JSONL operation log and
// replays mutations to reconstruct the full session JSON.
//
// Format:
//   - kind=0 (Initial): first line, contains full snapshot
//   - kind=1 (Set): update property at path k
//   - kind=2 (Push): append/splice items into array at path k
//   - kind=3 (Delete): remove property at path k
func reconstructJSONL(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)

	var state any

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var op jsonlOp
		if err := json.Unmarshal(line, &op); err != nil {
			continue
		}

		switch op.Kind {
		case 0: // Initial
			if err := json.Unmarshal(op.V, &state); err != nil {
				return nil, fmt.Errorf(
					"jsonl initial: %w", err,
				)
			}

		case 1: // Set
			if state == nil || len(op.K) == 0 {
				continue
			}
			keys := decodeJSONLKeys(op.K)
			var val any
			if err := json.Unmarshal(op.V, &val); err != nil {
				continue
			}
			jsonlSet(state, keys, val)

		case 2: // Push
			if state == nil || len(op.K) == 0 {
				continue
			}
			keys := decodeJSONLKeys(op.K)
			var items []any
			if err := json.Unmarshal(op.V, &items); err != nil {
				continue
			}
			jsonlPush(state, keys, items, op.I)

		case 3: // Delete
			if state == nil || len(op.K) == 0 {
				continue
			}
			keys := decodeJSONLKeys(op.K)
			jsonlDelete(state, keys)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}

	if state == nil {
		return nil, nil
	}

	return json.Marshal(state)
}

// decodeJSONLKeys converts raw JSON key elements to strings.
// Keys can be strings (object keys) or numbers (array indices).
func decodeJSONLKeys(raw []json.RawMessage) []string {
	keys := make([]string, len(raw))
	for i, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err == nil {
			keys[i] = s
			continue
		}
		// Must be a number (array index)
		keys[i] = strings.TrimSpace(string(r))
	}
	return keys
}

// jsonlNavigate traverses the state tree to the parent of
// the target, returning the parent and the final key.
func jsonlNavigate(
	state any, keys []string,
) (any, string) {
	current := state
	for _, k := range keys[:len(keys)-1] {
		current = jsonlChild(current, k)
		if current == nil {
			return nil, ""
		}
	}
	return current, keys[len(keys)-1]
}

func jsonlChild(node any, key string) any {
	switch n := node.(type) {
	case map[string]any:
		return n[key]
	case []any:
		idx, err := strconv.Atoi(key)
		if err != nil || idx < 0 || idx >= len(n) {
			return nil
		}
		return n[idx]
	}
	return nil
}

func jsonlSet(
	state any, keys []string, val any,
) {
	parent, lastKey := jsonlNavigate(state, keys)
	if parent == nil {
		return
	}
	switch p := parent.(type) {
	case map[string]any:
		p[lastKey] = val
	case []any:
		idx, err := strconv.Atoi(lastKey)
		if err != nil || idx < 0 || idx >= len(p) {
			return
		}
		p[idx] = val
	}
}

func jsonlPush(
	state any, keys []string,
	items []any, spliceIdx *int,
) {
	// Navigate to the array
	var target any
	if len(keys) == 0 {
		return
	}
	if len(keys) == 1 {
		target = state
	} else {
		target = state
		for _, k := range keys[:len(keys)-1] {
			target = jsonlChild(target, k)
			if target == nil {
				return
			}
		}
	}

	lastKey := keys[len(keys)-1]

	switch p := target.(type) {
	case map[string]any:
		arr, ok := p[lastKey].([]any)
		if !ok {
			return
		}
		if spliceIdx != nil {
			idx := max(0, min(*spliceIdx, len(arr)))
			newArr := make(
				[]any, 0, len(arr)+len(items),
			)
			newArr = append(newArr, arr[:idx]...)
			newArr = append(newArr, items...)
			newArr = append(newArr, arr[idx:]...)
			p[lastKey] = newArr
		} else {
			p[lastKey] = append(arr, items...)
		}
	case []any:
		idx, err := strconv.Atoi(lastKey)
		if err != nil || idx < 0 || idx >= len(p) {
			return
		}
		arr, ok := p[idx].([]any)
		if !ok {
			return
		}
		if spliceIdx != nil {
			si := max(0, min(*spliceIdx, len(arr)))
			newArr := make(
				[]any, 0, len(arr)+len(items),
			)
			newArr = append(newArr, arr[:si]...)
			newArr = append(newArr, items...)
			newArr = append(newArr, arr[si:]...)
			p[idx] = newArr
		} else {
			p[idx] = append(arr, items...)
		}
	}
}

func jsonlDelete(state any, keys []string) {
	parent, lastKey := jsonlNavigate(state, keys)
	if parent == nil {
		return
	}
	if m, ok := parent.(map[string]any); ok {
		delete(m, lastKey)
	}
}
