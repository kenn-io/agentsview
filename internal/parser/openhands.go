package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	openHandsMessageEvent     = "MessageEvent"
	openHandsActionEvent      = "ActionEvent"
	openHandsObservationEvent = "ObservationEvent"
)

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

// parseSession parses a single OpenHands CLI conversation
// directory into a session and messages.
func (p *openHandsProvider) parseSession(
	ctx context.Context,
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
		project = ExtractProjectFromCwdWithBranchContext(ctx, cwd, "")
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

	base := ExtractMessageContent(context.Background(), llmMessage.Get("content"))
	body := continueMessageContent(base)
	blocks := llmMessage.Get("thinking_blocks")
	if len(blocks.Array()) == 0 {
		blocks = ev.Get("thinking_blocks")
	}
	thinking := llmMessage.Get("reasoning_content").Str
	if thinking == "" {
		thinking = ev.Get("reasoning_content").Str
	}
	if thinking == base.ThinkingText {
		thinking = ""
	}
	appendOpenHandsThinking(body, blocks, thinking)
	// The SDK persists plaintext Responses reasoning separately from opaque
	// encrypted_content. Only summary/content are displayable body sources.
	if item := llmMessage.Get("responses_reasoning_item"); item.IsObject() {
		parts := append(openHandsStringArray(item.Get("summary")), openHandsStringArray(item.Get("content"))...)
		body.AddThinking(strings.Join(parts, "\n\n"))
	}
	msg := body.Message()
	msg.trimDialogue()
	if !msg.hasNativeBody() {
		return ParsedMessage{}, false, ""
	}
	msg.Ordinal = ordinal
	msg.Role = role
	msg.Timestamp = ts
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

	var body MessageContentBuilder
	body.AddText(openHandsText(ev.Get("thought")))
	rendering := formatOpenHandsAction(toolName, action, ev.Get("summary").Str)
	body.AddToolCall(ParsedToolCall{
		ToolUseID: ev.Get("tool_call_id").Str,
		ToolName:  toolName,
		Category:  openHandsToolCategory(toolName, action),
		InputJSON: inputJSON,
		Rendering: strings.TrimSpace(rendering),
	})
	appendOpenHandsThinking(&body, ev.Get("thinking_blocks"), ev.Get("reasoning_content").Str)
	msg := body.Message()
	msg.Ordinal = ordinal
	msg.Role = RoleAssistant
	msg.Timestamp = ts
	msg.Model = model
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
	if toolUseID == "" && display == "" {
		return ParsedMessage{}, false, ""
	}
	if raw == "" {
		b, _ := json.Marshal(display)
		raw = string(b)
	}
	contentLength := len(display)
	if contentLength == 0 {
		contentLength = toolResultContentLength(gjson.Parse(raw))
	}
	var body MessageContentBuilder
	body.AddToolResult(ParsedToolResult{
		ToolUseID:     toolUseID,
		ContentLength: contentLength,
		ContentRaw:    raw,
	})
	msg := body.Message()
	msg.Ordinal = ordinal
	msg.Role = RoleUser
	msg.SourceSubtype = SourceSubtypeToolResult
	msg.Timestamp = ts
	// Paired observations historically contribute work through the call.
	if toolUseID == "" {
		msg.ContentLength = sanitizedWorkLength(display)
	}
	return msg, true, workingDir
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
	text, _, _, _, _, _ := ExtractTextContent(context.Background(), content)
	return strings.TrimSpace(text)
}

func appendOpenHandsThinking(body *MessageContentBuilder, blocks gjson.Result, reasoning string) {
	workLength, workParts := body.workLength, body.workParts
	var parts []string
	blocks.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").Str {
		case "thinking":
			text := strings.TrimSpace(block.Get("thinking").Str)
			body.AddThinking(text)
			if text != "" {
				parts = append(parts, text)
			}
		case "redacted_thinking":
			body.AddThinking("")
		}
		return true
	})
	if len(parts) == 0 {
		if text := strings.TrimSpace(reasoning); text != "" {
			body.AddThinking(text)
			parts = append(parts, text)
		}
	}
	if len(parts) > 0 {
		// This provider historically rendered one reasoning envelope around
		// the joined blocks. Retain that work count with individual spans.
		body.workLength, body.workParts = workLength, workParts
		body.addWork(sanitizedWorkLength(strings.Join(parts, "\n\n")) + len("[Thinking]\n\n[/Thinking]"))
	}
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
		return "[Bash]\n$ " + cmd
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
