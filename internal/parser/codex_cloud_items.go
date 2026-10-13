package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

// codexCloudItem holds every item field the mapping reads. Fields absent
// from an item type stay zero.
type codexCloudItem struct {
	ID               string           `json:"id"`
	Type             string           `json:"type"`
	Status           jsontext.Value   `json:"status"`
	Text             string           `json:"text"`
	Content          jsontext.Value   `json:"content"`
	Summary          []string         `json:"summary"`
	Command          jsontext.Value   `json:"command"`
	Cwd              string           `json:"cwd"`
	AggregatedOutput string           `json:"aggregatedOutput"`
	ExitCode         *float64         `json:"exitCode"`
	DurationMs       *float64         `json:"durationMs"`
	Server           string           `json:"server"`
	Tool             string           `json:"tool"`
	Name             string           `json:"name"`
	Arguments        jsontext.Value   `json:"arguments"`
	Result           jsontext.Value   `json:"result"`
	Error            jsontext.Value   `json:"error"`
	Output           jsontext.Value   `json:"output"`
	Query            string           `json:"query"`
	Path             string           `json:"path"`
	Changes          []codexCloudEdit `json:"changes"`
}

type codexCloudEdit struct {
	Path string `json:"path"`
}

// codexCloudItemMessages maps one item to messages. Every item type yields
// at least one message, so nothing is dropped silently, except a reasoning
// item with no text, which would only add an empty thinking row.
func codexCloudItemMessages(
	record mirror.ItemRecord, ts time.Time, meta *mirror.Thread,
) []ParsedMessage {
	var item codexCloudItem
	if err := json.Unmarshal(record.Item, &item); err != nil {
		var header mirror.ItemHeader
		_ = json.Unmarshal(record.Item, &header)
		return []ParsedMessage{codexCloudNote(fmt.Sprintf("[unreadable %s item] %s", header.Type, record.Item), ts)}
	}
	assistant := func(msg ParsedMessage) ParsedMessage {
		msg.Role = RoleAssistant
		msg.Timestamp = ts
		msg.Model = meta.Model
		msg.ReasoningEffort = meta.ReasoningEffort
		msg.ContentLength = len(msg.Content) + len(msg.ThinkingText)
		return msg
	}
	switch item.Type {
	case "userMessage":
		text := codexCloudUserText(item.Content)
		return []ParsedMessage{{
			Role: RoleUser, Content: text, Timestamp: ts, ContentLength: len(text),
		}}
	case "agentMessage":
		return []ParsedMessage{assistant(ParsedMessage{Content: item.Text})}
	case "plan":
		return []ParsedMessage{assistant(ParsedMessage{Content: item.Text})}
	case "reasoning":
		thinking := strings.Join(item.Summary, "\n\n")
		if thinking == "" {
			thinking = strings.Join(codexCloudStrings(item.Content), "\n\n")
		}
		if strings.TrimSpace(thinking) == "" {
			return nil
		}
		return []ParsedMessage{assistant(ParsedMessage{ThinkingText: thinking, HasThinking: true})}
	case "contextCompaction":
		return []ParsedMessage{{
			Role: RoleAssistant, Content: "Context compacted", Timestamp: ts,
			IsSystem: true, ContentLength: len("Context compacted"),
			SourceType: "system", SourceSubtype: "compact_boundary",
			IsCompactBoundary: true,
		}}
	case "hookPrompt", "subAgentActivity", "enteredReviewMode", "exitedReviewMode", "sleep":
		return []ParsedMessage{codexCloudNote(codexCloudNoteText(item, record.Item), ts)}
	}

	call, result := codexCloudToolCall(item, record.Item)
	callMsg := assistant(ParsedMessage{
		Content: call.Rendering, HasToolUse: true, ToolCalls: []ParsedToolCall{call},
	})
	if item.Type == "fileChange" && len(item.Changes) > 0 {
		// One call per file, as the codex parser emits for apply_patch, so
		// file-keyed views group by path. Results pair by tool-use ID, so
		// each file of a multi-file change gets its own ID and result.
		calls := make([]ParsedToolCall, 0, len(item.Changes))
		for i, change := range item.Changes {
			perFile := call
			perFile.FilePath = change.Path
			if len(item.Changes) > 1 {
				perFile.ToolUseID = fmt.Sprintf("%s#%d", item.ID, i+1)
			}
			calls = append(calls, perFile)
		}
		callMsg.ToolCalls = calls
	}
	messages := []ParsedMessage{callMsg}
	if result == "" && !codexCloudTerminal(item.Status) {
		return messages
	}
	quoted, err := json.Marshal(result)
	if err != nil {
		return messages
	}
	resultTS := ts
	if done := mirror.Millis(record.CompletedAtMs); !done.IsZero() {
		resultTS = done
	}
	status := codexCloudResultStatus(item)
	results := make([]ParsedToolResult, 0, len(callMsg.ToolCalls))
	for _, call := range callMsg.ToolCalls {
		results = append(results, ParsedToolResult{
			ToolUseID: call.ToolUseID, Source: "tool_result", Status: status,
			ContentRaw: string(quoted), ContentLength: len(result),
		})
	}
	return append(messages, ParsedMessage{
		Role: RoleUser, Timestamp: resultTS, ContentLength: len(result),
		ToolResults: results,
	})
}

// codexCloudToolCall returns the call and its result text for tool-like
// items, and a generic call carrying the raw type name for anything else.
func codexCloudToolCall(item codexCloudItem, raw jsontext.Value) (ParsedToolCall, string) {
	call := ParsedToolCall{ToolUseID: item.ID}
	var result string
	switch item.Type {
	case "commandExecution":
		command := codexCloudCommand(item.Command)
		call.ToolName, call.Category = "shell_command", "Bash"
		call.InputJSON = codexCloudJSON(map[string]string{"command": command, "cwd": item.Cwd})
		call.Rendering = "$ " + command
		result = codexCloudJoinResult(item.AggregatedOutput, codexCloudCommandFooter(item))
	case "mcpToolCall":
		call.ToolName = "mcp__" + item.Server + "__" + item.Tool
		call.Category = NormalizeToolCategory(call.ToolName)
		call.InputJSON = string(item.Arguments)
		call.Rendering = item.Server + "/" + item.Tool
		result = codexCloudJoinResult(codexCloudPayload(item.Result, item.Error), codexCloudDuration(item.DurationMs))
	case "fileChange":
		call.ToolName, call.Category = "apply_patch", "Edit"
		call.InputJSON = string(raw)
		paths := make([]string, 0, len(item.Changes))
		for _, change := range item.Changes {
			paths = append(paths, change.Path)
		}
		call.Rendering = "Edit " + strings.Join(paths, ", ")
	case "webSearch":
		call.ToolName, call.Category = "web_search", "Tool"
		call.InputJSON = codexCloudJSON(map[string]string{"query": item.Query})
		call.Rendering = "Search: " + item.Query
	case "functionCallOutput":
		call.ToolName = firstNonEmpty(item.Name, item.Type)
		call.Category = NormalizeToolCategory(call.ToolName)
		call.InputJSON = "{}"
		call.Rendering = call.ToolName
		result = codexCloudPayload(item.Output, nil)
	case "dynamicToolCall":
		call.ToolName = firstNonEmpty(item.Tool, item.Name, item.Type)
		call.Category = NormalizeToolCategory(call.ToolName)
		call.InputJSON = string(item.Arguments)
		call.Rendering = call.ToolName
		result = codexCloudPayload(item.Result, item.Error)
	case "collabAgentToolCall":
		call.ToolName, call.Category = firstNonEmpty(item.Tool, item.Type), "Task"
		call.InputJSON = string(raw)
		call.Rendering = call.ToolName
	case "imageView":
		call.ToolName, call.Category = "view_image", "Read"
		call.InputJSON = codexCloudJSON(map[string]string{"path": item.Path})
		call.Rendering = "View " + item.Path
	case "imageGeneration":
		call.ToolName, call.Category = "image_generation", "Tool"
		call.InputJSON = string(raw)
		call.Rendering = "Generate image"
	default:
		call.ToolName, call.Category = item.Type, "Other"
		call.InputJSON = string(raw)
		call.Rendering = "[" + item.Type + "]"
	}
	if call.InputJSON == "" {
		call.InputJSON = "{}"
	}
	return call, result
}

func codexCloudNote(text string, ts time.Time) ParsedMessage {
	return ParsedMessage{
		Role: RoleAssistant, Content: text, Timestamp: ts, IsSystem: true,
		ContentLength: len(text), SourceType: "system",
	}
}

func codexCloudNoteText(item codexCloudItem, raw jsontext.Value) string {
	if item.Text != "" {
		return "[" + item.Type + "] " + item.Text
	}
	return "[" + item.Type + "] " + string(raw)
}

func codexCloudUserText(content jsontext.Value) string {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Path string `json:"path"`
		URL  string `json:"url"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return codexCloudString(content)
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		switch {
		case part.Text != "":
			texts = append(texts, part.Text)
		case part.Path != "" || part.URL != "":
			texts = append(texts, "[image: "+firstNonEmpty(part.Path, part.URL)+"]")
		}
	}
	return strings.Join(texts, "\n")
}

func codexCloudStrings(v jsontext.Value) []string {
	var values []string
	if json.Unmarshal(v, &values) == nil {
		return values
	}
	return nil
}

// codexCloudCommand accepts a command as a string or an argument array.
func codexCloudCommand(v jsontext.Value) string {
	if s := codexCloudString(v); s != "" {
		return s
	}
	return strings.Join(codexCloudStrings(v), " ")
}

func codexCloudCommandFooter(item codexCloudItem) string {
	var parts []string
	if item.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit code %d", int64(*item.ExitCode)))
	}
	if d := codexCloudDuration(item.DurationMs); d != "" {
		parts = append(parts, d)
	}
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func codexCloudDuration(ms *float64) string {
	if ms == nil {
		return ""
	}
	return fmt.Sprintf("%d ms", int64(*ms))
}

func codexCloudJoinResult(parts ...string) string {
	kept := parts[:0]
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n")
}

// codexCloudPayload renders a result or error value as text: a string as
// itself, anything else as its JSON.
func codexCloudPayload(result, errValue jsontext.Value) string {
	if len(errValue) > 0 && string(errValue) != "null" {
		if s := codexCloudString(errValue); s != "" {
			return "Error: " + s
		}
		return "Error: " + string(errValue)
	}
	if len(result) == 0 || string(result) == "null" {
		return ""
	}
	if s := codexCloudString(result); s != "" {
		return s
	}
	return string(result)
}

func codexCloudJSON(fields map[string]string) string {
	data, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return "{}"
	}
	return string(data)
}

func codexCloudTerminal(status jsontext.Value) bool {
	switch codexCloudString(status) {
	case "completed", "failed", "declined", "interrupted":
		return true
	default:
		return false
	}
}

// codexCloudResultStatus maps an item status to a tool result status. An
// interrupted call did not succeed, so it counts as errored; a call still
// in progress reports partial output as running.
func codexCloudResultStatus(item codexCloudItem) string {
	status := codexCloudString(item.Status)
	switch {
	case status == "failed" || status == "declined" || status == "interrupted":
		return "errored"
	case len(item.Error) > 0 && string(item.Error) != "null":
		return "errored"
	case status == "inProgress":
		return "running"
	default:
		return "completed"
	}
}
