package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	openHandsMessageEvent     = "MessageEvent"
	openHandsActionEvent      = "ActionEvent"
	openHandsObservationEvent = "ObservationEvent"
)

// DiscoverOpenHandsSessions finds OpenHands CLI conversation
// directories under ~/.openhands/conversations.
func DiscoverOpenHandsSessions(
	conversationsDir string,
) []DiscoveredFile {
	entries, err := os.ReadDir(conversationsDir)
	if err != nil {
		return nil
	}

	var files []DiscoveredFile
	for _, entry := range entries {
		if !entry.IsDir() || !IsValidSessionID(entry.Name()) {
			continue
		}
		sessionDir := filepath.Join(
			conversationsDir, entry.Name(),
		)
		if !isOpenHandsSessionDir(sessionDir) {
			continue
		}
		files = append(files, DiscoveredFile{
			Path:  sessionDir,
			Agent: AgentOpenHands,
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

// FindOpenHandsSourceFile locates an OpenHands conversation
// directory by its raw session ID.
func FindOpenHandsSourceFile(
	conversationsDir, rawID string,
) string {
	if conversationsDir == "" || !IsValidSessionID(rawID) {
		return ""
	}

	candidates := []string{rawID}
	stripped := strings.ReplaceAll(rawID, "-", "")
	if stripped != rawID {
		candidates = append(candidates, stripped)
	}

	for _, cand := range candidates {
		sessionDir := filepath.Join(conversationsDir, cand)
		if isOpenHandsSessionDir(sessionDir) {
			return sessionDir
		}
	}

	entries, err := os.ReadDir(conversationsDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionDir := filepath.Join(
			conversationsDir, entry.Name(),
		)
		if !isOpenHandsSessionDir(sessionDir) {
			continue
		}
		if normalizeOpenHandsSessionID(entry.Name()) == normalizeOpenHandsSessionID(rawID) {
			return sessionDir
		}
	}
	return ""
}

// OpenHandsSnapshot computes synthetic file metadata for an
// OpenHands conversation directory by hashing the relevant
// metadata of base_state.json, TASKS.json, and events/*.json.
func OpenHandsSnapshot(path string) (FileInfo, error) {
	sessionDir, err := normalizeOpenHandsSessionPath(path)
	if err != nil {
		return FileInfo{}, err
	}

	rootInfo, err := os.Stat(sessionDir)
	if err != nil {
		return FileInfo{}, fmt.Errorf("stat %s: %w", sessionDir, err)
	}

	var (
		totalSize int64
		maxMtime  = rootInfo.ModTime().UnixNano()
		manifest  []string
	)

	addFile := func(rel string) error {
		full := filepath.Join(sessionDir, rel)
		info, err := os.Stat(full)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		mtime := info.ModTime().UnixNano()
		totalSize += info.Size()
		if mtime > maxMtime {
			maxMtime = mtime
		}
		manifest = append(manifest, fmt.Sprintf(
			"%s:%d:%d", rel, info.Size(), mtime,
		))
		return nil
	}

	for _, rel := range []string{
		"base_state.json",
		"TASKS.json",
	} {
		if err := addFile(rel); err != nil {
			return FileInfo{}, fmt.Errorf(
				"stat %s: %w", filepath.Join(sessionDir, rel), err,
			)
		}
	}

	eventsDir := filepath.Join(sessionDir, "events")
	eventEntries, err := os.ReadDir(eventsDir)
	if err != nil {
		return FileInfo{}, fmt.Errorf("read %s: %w", eventsDir, err)
	}
	for _, entry := range eventEntries {
		if entry.IsDir() ||
			!strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := addFile(filepath.Join(
			"events", entry.Name(),
		)); err != nil {
			return FileInfo{}, fmt.Errorf(
				"stat %s: %w",
				filepath.Join(eventsDir, entry.Name()), err,
			)
		}
	}

	h := sha256.New()
	for _, line := range manifest {
		_, _ = h.Write([]byte(line))
		_, _ = h.Write([]byte{'\n'})
	}

	return FileInfo{
		Path:  sessionDir,
		Size:  totalSize,
		Mtime: maxMtime,
		Hash:  hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// ParseOpenHandsSession parses a single OpenHands CLI
// conversation directory into a session and messages.
func ParseOpenHandsSession(
	path, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	sessionDir, err := normalizeOpenHandsSessionPath(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	snapshot, err := OpenHandsSnapshot(sessionDir)
	if err != nil {
		return nil, nil, err
	}

	baseState := readOpenHandsJSON(filepath.Join(
		sessionDir, "base_state.json",
	))
	sessionID := baseState.Get("id").Str
	if sessionID == "" {
		sessionID = normalizeOpenHandsSessionID(
			filepath.Base(sessionDir),
		)
	}

	model := baseState.Get("agent.llm.model").Str
	cwd := openHandsBaseStateCwd(baseState)

	eventEntries, err := os.ReadDir(
		filepath.Join(sessionDir, "events"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"read %s: %w",
			filepath.Join(sessionDir, "events"), err,
		)
	}

	var (
		messages      []ParsedMessage
		firstMessage  string
		startedAt     time.Time
		endedAt       time.Time
		ordinal       int
		realUserCount int
	)

	for _, entry := range eventEntries {
		if entry.IsDir() ||
			!strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		eventPath := filepath.Join(
			sessionDir, "events", entry.Name(),
		)
		data, err := os.ReadFile(eventPath)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"read %s: %w", eventPath, err,
			)
		}
		if !gjson.ValidBytes(data) {
			return nil, nil, fmt.Errorf(
				"invalid JSON in %s", eventPath,
			)
		}

		ev := gjson.ParseBytes(data)
		ts := parseHermesTimestamp(
			ev.Get("timestamp").Str,
		)
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(endedAt) {
				endedAt = ts
			}
		}

		var (
			msg        ParsedMessage
			ok         bool
			discovered string
		)
		switch ev.Get("kind").Str {
		case openHandsMessageEvent:
			msg, ok, discovered = parseOpenHandsMessageEvent(
				ev, ordinal, model, ts,
			)
			if ok && msg.Role == RoleUser &&
				strings.TrimSpace(msg.Content) != "" {
				realUserCount++
				if firstMessage == "" {
					firstMessage = truncate(
						strings.ReplaceAll(
							msg.Content, "\n", " ",
						),
						300,
					)
				}
			}
		case openHandsActionEvent:
			msg, ok, discovered = parseOpenHandsActionEvent(
				ev, ordinal, model, ts,
			)
		case openHandsObservationEvent:
			msg, ok, discovered = parseOpenHandsObservationEvent(
				ev, ordinal, ts,
			)
		}
		if !ok {
			continue
		}
		if cwd == "" && discovered != "" {
			cwd = discovered
		}

		messages = append(messages, msg)
		ordinal++
	}

	if len(messages) == 0 {
		return nil, nil, nil
	}

	project := ""
	if cwd != "" {
		project = ExtractProjectFromCwd(cwd)
	}
	if project == "" {
		project = "openhands"
	}

	sess := &ParsedSession{
		ID:               "openhands:" + sessionID,
		Project:          project,
		Machine:          machine,
		Agent:            AgentOpenHands,
		Cwd:              cwd,
		FirstMessage:     firstMessage,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(messages),
		UserMessageCount: realUserCount,
		File:             snapshot,
	}
	return sess, messages, nil
}

func parseOpenHandsMessageEvent(
	ev gjson.Result,
	ordinal int,
	model string,
	ts time.Time,
) (ParsedMessage, bool, string) {
	llmMessage := ev.Get("llm_message")
	role := RoleType(llmMessage.Get("role").Str)
	if role != RoleUser && role != RoleAssistant {
		return ParsedMessage{}, false, ""
	}

	content, _, _, _, toolCalls, toolResults :=
		ExtractTextContent(llmMessage.Get("content"))
	content, hasThinking := openHandsAppendThinking(
		content, ev,
	)
	content = strings.TrimSpace(content)
	if content == "" &&
		len(toolCalls) == 0 &&
		len(toolResults) == 0 {
		return ParsedMessage{}, false, ""
	}

	msg := ParsedMessage{
		Ordinal:       ordinal,
		Role:          role,
		Content:       content,
		Timestamp:     ts,
		HasThinking:   hasThinking,
		HasToolUse:    len(toolCalls) > 0,
		ContentLength: len(content),
		ToolCalls:     toolCalls,
		ToolResults:   toolResults,
	}
	if role == RoleAssistant {
		msg.Model = model
	}
	return msg, true, ""
}

func parseOpenHandsActionEvent(
	ev gjson.Result,
	ordinal int,
	model string,
	ts time.Time,
) (ParsedMessage, bool, string) {
	toolName := ev.Get("tool_name").Str
	if toolName == "" {
		toolName = ev.Get("tool_call.name").Str
	}
	if toolName == "" {
		return ParsedMessage{}, false, ""
	}

	action := ev.Get("action")
	inputJSON := strings.TrimSpace(
		ev.Get("tool_call.arguments").Str,
	)
	if inputJSON == "" {
		inputJSON = action.Raw
	}

	content := openHandsText(ev.Get("thought"))
	content = joinOpenHandsParts(
		content,
		formatOpenHandsAction(
			toolName, action, ev.Get("summary").Str,
		),
	)
	content, hasThinking := openHandsAppendThinking(
		content, ev,
	)
	content = strings.TrimSpace(content)
	if content == "" {
		return ParsedMessage{}, false, ""
	}

	msg := ParsedMessage{
		Ordinal:       ordinal,
		Role:          RoleAssistant,
		Content:       content,
		Timestamp:     ts,
		HasThinking:   hasThinking,
		HasToolUse:    true,
		ContentLength: len(content),
		Model:         model,
		ToolCalls: []ParsedToolCall{{
			ToolUseID: ev.Get("tool_call_id").Str,
			ToolName:  toolName,
			Category:  openHandsToolCategory(toolName, action),
			InputJSON: inputJSON,
		}},
	}
	return msg, true, openHandsActionCwd(toolName, action)
}

func parseOpenHandsObservationEvent(
	ev gjson.Result,
	ordinal int,
	ts time.Time,
) (ParsedMessage, bool, string) {
	observation := ev.Get("observation")
	raw, display := openHandsObservationContent(
		observation,
	)
	display = strings.TrimSpace(display)
	workingDir := observation.Get(
		"metadata.working_dir",
	).Str

	toolUseID := ev.Get("tool_call_id").Str
	if toolUseID == "" {
		if display == "" {
			return ParsedMessage{}, false, ""
		}
		return ParsedMessage{
			Ordinal:       ordinal,
			Role:          RoleUser,
			Content:       display,
			Timestamp:     ts,
			ContentLength: len(display),
		}, true, workingDir
	}

	if raw == "" {
		b, _ := json.Marshal(display)
		raw = string(b)
	}
	contentLength := len(display)
	if contentLength == 0 {
		contentLength = toolResultContentLength(
			gjson.Parse(raw),
		)
	}

	return ParsedMessage{
		Ordinal:   ordinal,
		Role:      RoleUser,
		Timestamp: ts,
		ToolResults: []ParsedToolResult{{
			ToolUseID:     toolUseID,
			ContentLength: contentLength,
			ContentRaw:    raw,
		}},
	}, true, workingDir
}

func openHandsBaseStateCwd(base gjson.Result) string {
	for _, path := range []string{
		"workspace.cwd",
		"workspace.path",
		"workspace.mount_path",
		"workspace.root",
		"workspace.repo_path",
		"workspace.repo_root",
		"workspace.working_dir",
	} {
		if value := strings.TrimSpace(
			base.Get(path).Str,
		); value != "" {
			return value
		}
	}
	return ""
}

func openHandsText(content gjson.Result) string {
	text, _, _, _, _, _ := ExtractTextContent(content)
	return strings.TrimSpace(text)
}

func openHandsAppendThinking(
	content string, ev gjson.Result,
) (string, bool) {
	thinking := openHandsThinkingText(ev)
	if thinking == "" {
		return content, false
	}
	block := "[Thinking]\n" + thinking + "\n[/Thinking]"
	if strings.TrimSpace(content) == "" {
		return block, true
	}
	return content + "\n" + block, true
}

func openHandsThinkingText(ev gjson.Result) string {
	var parts []string
	ev.Get("thinking_blocks").ForEach(func(_, block gjson.Result) bool {
		if t := strings.TrimSpace(
			block.Get("thinking").Str,
		); t != "" {
			parts = append(parts, t)
		}
		return true
	})
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n")
	}
	return strings.TrimSpace(
		ev.Get("reasoning_content").Str,
	)
}

func openHandsToolCategory(
	toolName string, action gjson.Result,
) string {
	switch toolName {
	case "file_editor":
		switch action.Get("command").Str {
		case "view":
			return "Read"
		case "create", "write":
			return "Write"
		case "str_replace", "insert", "append", "edit":
			return "Edit"
		default:
			return "Tool"
		}
	case "delegate":
		return "Task"
	case "task_tracker":
		return "Tool"
	}
	return NormalizeToolCategory(toolName)
}

func formatOpenHandsAction(
	toolName string,
	action gjson.Result,
	summary string,
) string {
	switch toolName {
	case "terminal":
		cmd := action.Get("command").Str
		if summary != "" {
			return fmt.Sprintf(
				"[Bash: %s]\n$ %s", summary, cmd,
			)
		}
		return fmt.Sprintf("[Bash]\n$ %s", cmd)
	case "file_editor":
		path := action.Get("path").Str
		switch action.Get("command").Str {
		case "view":
			return fmt.Sprintf("[Read: %s]", path)
		case "create", "write":
			return fmt.Sprintf("[Write: %s]", path)
		case "str_replace", "insert", "append", "edit":
			return fmt.Sprintf("[Edit: %s]", path)
		default:
			return fmt.Sprintf(
				"[FileEditor: %s %s]",
				action.Get("command").Str, path,
			)
		}
	case "delegate":
		cmd := action.Get("command").Str
		ids := openHandsStringArray(action.Get("ids"))
		if len(ids) > 0 {
			return fmt.Sprintf(
				"[Task: %s %s]",
				cmd, strings.Join(ids, ", "),
			)
		}
		if cmd != "" {
			return fmt.Sprintf("[Task: %s]", cmd)
		}
		return "[Task]"
	case "task_tracker":
		if cmd := action.Get("command").Str; cmd != "" {
			return fmt.Sprintf(
				"[TaskTracker: %s]",
				cmd,
			)
		}
		return "[TaskTracker]"
	default:
		if summary != "" {
			return fmt.Sprintf(
				"[%s: %s]",
				toolName, summary,
			)
		}
		return fmt.Sprintf("[%s]", toolName)
	}
}

func openHandsObservationContent(
	observation gjson.Result,
) (string, string) {
	content := observation.Get("content")
	if content.Exists() {
		raw := content.Raw
		return raw, DecodeContent(raw)
	}

	parts := []string{}
	if cmd := observation.Get("command").Str; cmd != "" {
		parts = append(parts, cmd)
	}
	if detail := observation.Get("detail").Str; detail != "" {
		parts = append(parts, detail)
	}
	display := strings.TrimSpace(strings.Join(parts, "\n"))
	if display == "" {
		display = strings.TrimSpace(observation.Raw)
	}
	if display == "" {
		return "", ""
	}
	raw, _ := json.Marshal(display)
	return string(raw), display
}

func openHandsActionCwd(
	toolName string, action gjson.Result,
) string {
	if toolName != "file_editor" {
		return ""
	}
	path := strings.TrimSpace(action.Get("path").Str)
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	return filepath.Dir(path)
}

func joinOpenHandsParts(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n")
}

func openHandsStringArray(value gjson.Result) []string {
	if !value.IsArray() {
		return nil
	}
	var out []string
	value.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String && item.Str != "" {
			out = append(out, item.Str)
		}
		return true
	})
	return out
}

func readOpenHandsJSON(path string) gjson.Result {
	data, err := os.ReadFile(path)
	if err != nil || !gjson.ValidBytes(data) {
		return gjson.Result{}
	}
	return gjson.ParseBytes(data)
}

func normalizeOpenHandsSessionPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return path, nil
	}
	name := filepath.Base(path)
	switch name {
	case "base_state.json", "TASKS.json":
		return filepath.Dir(path), nil
	}
	if filepath.Base(filepath.Dir(path)) == "events" {
		return filepath.Dir(filepath.Dir(path)), nil
	}
	return "", fmt.Errorf("openhands path is not a session dir: %s", path)
}

func isOpenHandsSessionDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	eventsDir := filepath.Join(path, "events")
	eventsInfo, err := os.Stat(eventsDir)
	if err != nil || eventsInfo == nil {
		return false
	}
	return eventsInfo.IsDir()
}

func normalizeOpenHandsSessionID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) != 32 || !isHexString(id) {
		return id
	}
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		id[0:8], id[8:12], id[12:16], id[16:20], id[20:32],
	)
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
