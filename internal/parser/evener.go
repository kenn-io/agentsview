package parser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type evenerHeader struct {
	Kind             string         `json:"kind"`
	FormatVersion    int            `json:"format_version"`
	SessionID        string         `json:"session_id"`
	ParentSessionID  string         `json:"parent_session_id"`
	ParentToolCallID string         `json:"parent_tool_call_id"`
	CreatedAt        time.Time      `json:"created_at"`
	ProfileID        string         `json:"profile_id"`
	Model            string         `json:"model"`
	WorkingDir       string         `json:"working_dir"`
	BuildVersion     string         `json:"build_version"`
	Depth            int            `json:"depth"`
	SystemPrompt     string         `json:"system_prompt"`
	Task             string         `json:"task"`
	AgentTasks       jsontext.Value `json:"agent_tasks"`
}

type evenerMeta struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ParentSessionID string `json:"parent_session_id"`
	DivergenceTurn  int    `json:"divergence_turn"`
	IsSubagent      bool   `json:"is_subagent"`
}

type evenerTurn struct {
	Kind      string    `json:"kind"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Content []evenerContent `json:"content"`
	} `json:"message"`
	SteeringSource   string       `json:"steering_source"`
	Usage            *evenerUsage `json:"usage"`
	ResponseModel    string       `json:"response_model"`
	ResponseProvider string       `json:"response_provider"`
	StableTurnID     string       `json:"stable_turn_id"`
	ModelSwitch      *struct {
		NewProvider string `json:"new_provider"`
		NewModel    string `json:"new_model"`
	} `json:"model_switch"`
	Error               jsontext.Value `json:"error"`
	Hook                jsontext.Value `json:"hook"`
	AttentionResolution jsontext.Value `json:"attention_resolution"`
}

type evenerContent struct {
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Thinking *struct {
		Text    string   `json:"text"`
		Summary []string `json:"summary"`
	} `json:"thinking"`
	Image    jsontext.Value `json:"image"`
	Audio    jsontext.Value `json:"audio"`
	Document *struct {
		FileName string `json:"file_name"`
	} `json:"document"`
	WebSearch *struct {
		Query string `json:"query"`
	} `json:"web_search"`
	ToolCall *struct {
		ID              string         `json:"id"`
		Name            string         `json:"name"`
		Arguments       jsontext.Value `json:"arguments"`
		ParsedArguments jsontext.Value `json:"parsed_arguments"`
	} `json:"tool_call"`
	ToolResult *struct {
		ToolCallID string         `json:"tool_call_id"`
		Content    jsontext.Value `json:"content"`
		IsError    bool           `json:"is_error"`
		ImageData  string         `json:"image_data"`
		ToolState  jsontext.Value `json:"tool_state"`
	} `json:"tool_result"`
}

type evenerUsage struct {
	Input        *int `json:"input_tokens"`
	Output       *int `json:"output_tokens"`
	Reasoning    *int `json:"reasoning_tokens"`
	CacheRead    *int `json:"cache_read_tokens"`
	CacheWrite   *int `json:"cache_write_tokens"`
	CacheWrite1h *int `json:"cache_write_1h_tokens"`
}

type evenerEntry struct {
	turn evenerTurn
	raw  jsontext.Value
}
type evenerTranscript struct {
	header    evenerHeader
	entries   []evenerEntry
	file      FileInfo
	truncated bool
}

func parseEvenerSession(ctx context.Context, path, machine string) (*ParsedSession, []ParsedMessage, error) {
	transcript, meta, metaPresent, err := readEvenerSource(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	h := transcript.header
	parentID := h.ParentSessionID
	if meta.ParentSessionID != "" {
		parentID = meta.ParentSessionID
	}
	sess := &ParsedSession{ID: "evener:" + h.SessionID, Agent: AgentEvener, Machine: machine, Cwd: h.WorkingDir, SourceVersion: h.BuildVersion, StartedAt: h.CreatedAt, File: transcript.file, IsTruncated: transcript.truncated, SessionName: meta.Name, SessionNamePresent: metaPresent}
	sess.Project = ExtractProjectFromCwdWithBranchContext(ctx, h.WorkingDir, "")
	if parentID != "" {
		sess.ParentSessionID = "evener:" + parentID
		if meta.DivergenceTurn > 0 {
			sess.RelationshipType = RelFork
		} else if meta.IsSubagent {
			sess.RelationshipType = RelSubagent
		}
	}
	skip := 0
	// Forks copy source turns, including diagnostics and model changes. Verify
	// that exact semantic prefix before removing any of the child's history.
	if parentPath := evenerForkParentPath(path, meta); parentPath != "" {
		count := meta.DivergenceTurn - 1
		parentInfo, statErr := os.Lstat(parentPath)
		if count <= len(transcript.entries) && statErr == nil && parentInfo.Mode().IsRegular() {
			parent, _, _, readErr := readEvenerSource(ctx, parentPath)
			if readErr == nil && !parent.truncated && len(parent.entries) >= count {
				equal := true
				for i := range count {
					if !bytes.Equal(transcript.entries[i].raw, parent.entries[i].raw) {
						equal = false
						break
					}
				}
				if equal {
					skip = count
				}
			}
		}
	}
	model, provider := h.Model, h.ProfileID
	var messages []ParsedMessage
	// Initial instructions are system context, never the first human prompt
	// or an additional usage-bearing response.
	for _, initial := range []struct{ kind, content string }{
		{"system_prompt", h.SystemPrompt},
		{"initial_task", h.Task},
		{"agent_tasks", string(h.AgentTasks)},
	} {
		content := strings.TrimSpace(initial.content)
		if content == "" || (initial.kind == "agent_tasks" && (content == "null" || content == "[]")) {
			continue
		}
		messages = append(messages, ParsedMessage{
			Ordinal: len(messages), Role: RoleSystem, IsSystem: true,
			Content: content, ContentLength: len(content), Timestamp: h.CreatedAt,
			SourceType: "header", SourceSubtype: initial.kind,
		})
	}
	type callLocation struct{ message, call int }
	calls := map[string]callLocation{}
	for i, entry := range transcript.entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		turn := entry.turn
		if turn.Kind == "MODEL_SWITCH" {
			model, provider = "", ""
			if turn.ModelSwitch != nil {
				model, provider = turn.ModelSwitch.NewModel, turn.ModelSwitch.NewProvider
			}
		}
		if i < skip {
			continue
		}
		msg := evenerMessage(entry, len(messages))
		if turn.Kind == "ASSISTANT" {
			msg.Model, msg.ProviderID = model, provider
			if turn.ResponseModel != "" {
				msg.Model = turn.ResponseModel
			}
			if turn.ResponseProvider != "" {
				msg.ProviderID = turn.ResponseProvider
			}
			applyEvenerUsage(&msg, turn.Usage)
		}
		for j, call := range msg.ToolCalls {
			if call.ToolUseID != "" {
				calls[call.ToolUseID] = callLocation{len(messages), j}
			}
		}
		messages = append(messages, msg)
		for _, part := range turn.Message.Content {
			if part.ToolResult == nil {
				continue
			}
			result := part.ToolResult
			if location, ok := calls[result.ToolCallID]; ok {
				status := "completed"
				if result.IsError {
					status = "error"
				}
				event := ParsedToolResultEvent{ToolUseID: result.ToolCallID, Source: "tool_result", Status: status, Content: evenerResultText(part), Timestamp: turn.Timestamp}
				tc := &messages[location.message].ToolCalls[location.call]
				if tc.ToolName == "delegate" || tc.ToolName == "delegate_send" || tc.ToolName == "job_status" {
					var state struct {
						Type          string `json:"type"`
						TranscriptRef string `json:"transcript_ref"`
						DelegateID    string `json:"delegate_id"`
						Status        string `json:"status"`
					}
					if json.Unmarshal(result.ToolState, &state) == nil && state.Type == "delegate" {
						if id, ok := strings.CutPrefix(state.TranscriptRef, "local:"); ok && id != "" && !strings.ContainsAny(id, "/\\:") {
							tc.Category = "Task"
							tc.SubagentSessionID = "evener:" + id
							event.SubagentSessionID = tc.SubagentSessionID
						}
						event.AgentID = state.DelegateID
						if !result.IsError && state.Status != "" {
							event.Status = state.Status
						}
					}
				}
				tc.ResultEvents = append(tc.ResultEvents, event)
			}
		}
		if msg.Role == RoleUser && msg.Content != "" {
			sess.UserMessageCount++
			if sess.FirstMessage == "" {
				sess.FirstMessage = truncateFirstMessage(msg.Content)
			}
		}
		if msg.Timestamp.After(sess.EndedAt) {
			sess.EndedAt = msg.Timestamp
		}
	}
	sess.MessageCount = len(messages)
	if err := accumulateMessageTokenUsageContext(ctx, sess, messages); err != nil {
		return nil, nil, err
	}
	return sess, messages, ctx.Err()
}

// Fork verification uses the same source validation as the parent's import,
// without recursively parsing its ancestry or normalized messages.
func readEvenerSource(ctx context.Context, path string) (evenerTranscript, evenerMeta, bool, error) {
	transcript, err := readEvenerTranscript(ctx, path)
	if err != nil {
		return transcript, evenerMeta{}, false, err
	}
	meta, present, err := readEvenerMeta(path)
	if err != nil {
		return transcript, meta, present, err
	}
	if meta.ParentSessionID != "" && transcript.header.ParentSessionID != "" && meta.ParentSessionID != transcript.header.ParentSessionID {
		return transcript, meta, present, fmt.Errorf("evener parent identities disagree")
	}
	return transcript, meta, present, nil
}

// readEvenerTranscript accepts only newline-framed v2 records. A valid JSON
// value without its final newline is still an uncommitted writer tail.
func readEvenerTranscript(ctx context.Context, path string) (evenerTranscript, error) {
	var out evenerTranscript
	if err := ctx.Err(); err != nil {
		return out, err
	}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	out.file = FileInfo{Path: path, Size: info.Size(), Mtime: info.ModTime().UnixNano()}
	reader := bufio.NewReader(&countingReader{ctx: ctx, r: f})
	headerSeen := false
	for lineNo := 1; ; lineNo++ {
		line, complete, readErr := readEvenerLine(reader)
		if readErr != nil {
			return out, fmt.Errorf("evener line %d: %w", lineNo, readErr)
		}
		if !complete {
			out.truncated = len(line) > 0
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !headerSeen {
			if err := json.Unmarshal(line, &out.header); err != nil {
				return out, fmt.Errorf("decode Evener header: %w", err)
			}
			if out.header.Kind != "header" || out.header.FormatVersion != 2 {
				return out, fmt.Errorf("unsupported Evener transcript: require semantic format_version 2")
			}
			if out.header.SessionID == "" || filepath.Base(path) != out.header.SessionID+".transcript.jsonl" {
				return out, fmt.Errorf("evener filename and header identity disagree")
			}
			headerSeen = true
			continue
		}
		var envelope struct {
			Kind string         `json:"kind"`
			Seq  int            `json:"seq"`
			Turn jsontext.Value `json:"turn"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			return out, fmt.Errorf("decode Evener line %d: %w", lineNo, err)
		}
		if envelope.Kind != "entry" {
			return out, fmt.Errorf("invalid Evener record kind %q", envelope.Kind)
		}
		var turn evenerTurn
		if err := json.Unmarshal(envelope.Turn, &turn); err != nil {
			return out, fmt.Errorf("decode Evener turn %d: %w", lineNo, err)
		}
		if turn.Kind == "" {
			return out, fmt.Errorf("evener line %d has no semantic turn kind", lineNo)
		}
		if err := envelope.Turn.Canonicalize(); err != nil {
			return out, err
		}
		out.entries = append(out.entries, evenerEntry{turn: turn, raw: envelope.Turn})
	}
	if !headerSeen {
		return out, fmt.Errorf("unsupported Evener transcript: missing complete v2 header")
	}
	return out, ctx.Err()
}

func readEvenerLine(reader *bufio.Reader) ([]byte, bool, error) {
	const limit = 128 << 20
	var line []byte
	oversized := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !oversized {
			if len(line)+len(chunk) > limit+1 {
				oversized = true
				line = nil
			} else {
				line = append(line, chunk...)
			}
		}
		if err == nil {
			if oversized {
				return nil, false, fmt.Errorf("record exceeds 128 MiB")
			}
			return line, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if oversized {
				return []byte{0}, false, nil
			}
			return line, false, nil
		}
		return nil, false, err
	}
}

func evenerMessage(entry evenerEntry, ordinal int) ParsedMessage {
	turn := entry.turn
	msg := ParsedMessage{Ordinal: ordinal, Role: RoleSystem, IsSystem: true, Timestamp: turn.Timestamp, SourceType: "entry", SourceSubtype: turn.Kind, SourceUUID: turn.StableTurnID, PromptSource: turn.SteeringSource}
	switch turn.Kind {
	case "USER_INPUT":
		msg.Role = RoleUser
	case "STEERING":
		if turn.SteeringSource == "user" {
			msg.Role = RoleUser
		}
	case "ASSISTANT":
		msg.Role = RoleAssistant
	case "TOOL", "TOOL_RESULTS":
		msg.Role = RoleTool
	case "CHECKPOINT", "SUMMARY":
		msg.IsCompactBoundary = true
	}
	msg.IsSystem = msg.Role == RoleSystem
	var content, thinking []string
	for _, part := range turn.Message.Content {
		switch part.Kind {
		case "text":
			content = append(content, part.Text)
		case "thinking", "redacted_thinking":
			msg.HasThinking = true
			text := "[thinking unavailable]"
			if part.Kind == "redacted_thinking" {
				text = "[redacted thinking]"
			} else if part.Thinking != nil {
				parts := append([]string{part.Thinking.Text}, part.Thinking.Summary...)
				if readable := strings.TrimSpace(strings.Join(parts, "\n")); readable != "" {
					text = readable
				}
			}
			thinking = append(thinking, text)
			content = append(content, "[Thinking]\n"+text+"\n[/Thinking]")
		case "image", "audio":
			content = append(content, "["+part.Kind+"]")
		case "document":
			label := "document"
			if part.Document != nil && part.Document.FileName != "" {
				label += " " + part.Document.FileName
			}
			content = append(content, "["+label+"]")
		case "web_search":
			text := "[web search]"
			if part.WebSearch != nil && part.WebSearch.Query != "" {
				text += " " + part.WebSearch.Query
			}
			content = append(content, text)
		case "tool_call":
			if part.ToolCall != nil {
				call := part.ToolCall
				args := call.Arguments
				if len(args) == 0 {
					args = call.ParsedArguments
				}
				msg.ToolCalls = append(msg.ToolCalls, ParsedToolCall{ToolUseID: call.ID, ToolName: call.Name, Category: NormalizeToolCategory(call.Name), InputJSON: string(args)})
				content = append(content, "[Tool: "+call.Name+"]\n")
			}
		case "tool_result":
			if part.ToolResult != nil {
				text := evenerResultText(part)
				raw, _ := json.Marshal(text)
				msg.ToolResults = append(msg.ToolResults, ParsedToolResult{ToolUseID: part.ToolResult.ToolCallID, ContentLength: len(text), ContentRaw: string(raw)})
				content = append(content, text)
			}
		default:
			content = append(content, "["+part.Kind+"] "+part.Text)
		}
	}
	for _, detail := range []jsontext.Value{turn.Error, turn.Hook, turn.AttentionResolution} {
		if len(detail) > 0 && !bytes.Equal(detail, []byte("null")) {
			content = append(content, string(detail))
		}
	}
	switch turn.Kind {
	case "USER_INPUT", "STEERING", "ASSISTANT", "TOOL", "TOOL_RESULTS", "SYSTEM", "CHECKPOINT", "SUMMARY", "MODEL_SWITCH", "TURN_FAILURE", "HOOK_COMPLETED", "ENVIRONMENT", "ATTENTION_RESOLUTION":
	default:
		content = append(content, "["+turn.Kind+"] "+string(entry.raw))
	}
	msg.Content = strings.TrimSpace(strings.Join(content, "\n"))
	if msg.Content == "" && !msg.HasThinking && len(msg.ToolCalls) == 0 {
		msg.Content = "[" + turn.Kind + "]"
	}
	msg.ThinkingText = strings.TrimSpace(strings.Join(thinking, "\n"))
	msg.HasToolUse = len(msg.ToolCalls) > 0
	msg.ContentLength = len(msg.Content)
	return msg
}

func evenerResultText(part evenerContent) string {
	result := part.ToolResult
	text := string(result.Content)
	if len(result.Content) > 0 && result.Content[0] == '"' {
		_ = json.Unmarshal(result.Content, &text)
	}
	if len(result.ToolState) > 0 && !bytes.Equal(result.ToolState, []byte("null")) {
		text += "\n" + string(result.ToolState)
	}
	if result.IsError {
		text = "[tool error] " + text
	}
	if result.ImageData != "" {
		text += "\n[image]"
	}
	return text
}

func applyEvenerUsage(msg *ParsedMessage, usage *evenerUsage) {
	msg.tokenPresenceKnown = true
	if usage == nil {
		return
	}
	normalized := map[string]any{}
	put := func(key string, value *int) {
		if value != nil {
			normalized[key] = max(*value, 0)
		}
	}
	put("input_tokens", usage.Input)
	put("output_tokens", usage.Output)
	put("reasoning_tokens", usage.Reasoning)
	put("cache_read_input_tokens", usage.CacheRead)
	value := func(v *int) int {
		if v == nil {
			return 0
		}
		return max(*v, 0)
	}
	if usage.CacheWrite != nil || usage.CacheWrite1h != nil {
		normalized["cache_creation_input_tokens"] = value(usage.CacheWrite) + value(usage.CacheWrite1h)
		normalized["cache_creation"] = map[string]int{"ephemeral_5m_input_tokens": value(usage.CacheWrite), "ephemeral_1h_input_tokens": value(usage.CacheWrite1h)}
	}
	if len(normalized) == 0 {
		return
	}
	msg.TokenUsage, _ = json.Marshal(normalized)
	msg.ContextTokens = value(usage.Input) + value(usage.CacheRead) + value(usage.CacheWrite) + value(usage.CacheWrite1h)
	msg.OutputTokens = value(usage.Output)
	msg.HasContextTokens = usage.Input != nil || usage.CacheRead != nil || usage.CacheWrite != nil || usage.CacheWrite1h != nil
	msg.HasOutputTokens = usage.Output != nil
}

// evenerParentTranscriptPath names the optional immediate parent dependency,
// including when it is absent, so arrival can invalidate the child fingerprint.
func evenerParentTranscriptPath(path string) (string, error) {
	meta, _, err := readEvenerMeta(path)
	if err != nil {
		return "", err
	}
	return evenerForkParentPath(path, meta), nil
}

func evenerForkParentPath(path string, meta evenerMeta) string {
	id := meta.ParentSessionID
	if meta.DivergenceTurn <= 1 || id == meta.ID || !evenerSafeID(id) {
		return ""
	}
	return filepath.Join(filepath.Dir(path), id+".transcript.jsonl")
}

func readEvenerMeta(path string) (evenerMeta, bool, error) {
	var meta evenerMeta
	metaPath := strings.TrimSuffix(path, ".transcript.jsonl") + ".meta.json"
	info, err := os.Lstat(metaPath)
	if errors.Is(err, os.ErrNotExist) {
		return meta, false, nil
	}
	if err != nil {
		return meta, false, fmt.Errorf("read Evener metadata: %w", err)
	}
	if !info.Mode().IsRegular() {
		return meta, true, fmt.Errorf("evener metadata is not a regular file")
	}
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return meta, true, fmt.Errorf("read Evener metadata: %w", err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, true, fmt.Errorf("decode Evener metadata: %w", err)
	}
	if meta.ID == "" || filepath.Base(path) != meta.ID+".transcript.jsonl" {
		return meta, true, fmt.Errorf("evener metadata identity does not match transcript")
	}
	return meta, true, nil
}
