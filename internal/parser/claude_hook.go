package parser

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
)

// claudeHookSuccess holds imported Claude Code hook output and metadata.
type claudeHookSuccess struct {
	subtype   string
	hookName  string
	hookEvent string
	command   string
	toolUseID string
	content   string
	stdout    string
	stderr    string
	timedOut  bool
	timeoutMs *float64
	summary   *claudeStopHookSummary
}

type claudeStopHookSummary struct {
	HookLabel string `json:"hookLabel"`
	HookCount *int   `json:"hookCount"`
	HookInfos []struct {
		Command    string `json:"command"`
		PromptText string `json:"promptText"`
	} `json:"hookInfos"`
	HookErrors            []string `json:"-"`
	HookAdditionalContext []string `json:"hookAdditionalContext"`
	PreventedContinuation *bool    `json:"preventedContinuation"`
	StopReason            string   `json:"stopReason"`
	ToolUseID             string   `json:"toolUseID"`
}

// IsClaudeHookSubtype reports whether the parser imports this hook subtype.
func IsClaudeHookSubtype(subtype string) bool {
	return slices.Contains(ClaudeHookSubtypes(), subtype)
}

// ClaudeHookSubtypes returns the hook subtypes imported by the parser.
func ClaudeHookSubtypes() []string {
	return []string{"hook_success", "hook_additional_context", "hook_cancelled", "stop_hook_summary"}
}

// extractClaudeHookSuccess decodes the hook output payload of one
// transcript line. It decodes the payload only: it does not store, order,
// or display the result.
func extractClaudeHookSuccess(line string, stopSummary bool) (claudeHookSuccess, bool) {
	if stopSummary {
		var summary claudeStopHookSummary
		if err := json.Unmarshal([]byte(line), &summary); err != nil {
			return claudeHookSuccess{}, false
		}
		for _, value := range gjson.Get(line, "hookErrors").Array() {
			if value.Type == gjson.String {
				summary.HookErrors = append(summary.HookErrors, value.Str)
			}
		}
		hasContent := len(summary.HookErrors) > 0 || summary.StopReason != "" ||
			strings.Join(summary.HookAdditionalContext, "\n") != "" ||
			(summary.PreventedContinuation != nil && *summary.PreventedContinuation)
		for _, info := range summary.HookInfos {
			hasContent = hasContent || info.PromptText != ""
		}
		if !hasContent {
			return claudeHookSuccess{}, false
		}
		return claudeHookSuccess{subtype: "stop_hook_summary", toolUseID: summary.ToolUseID, summary: &summary}, true
	}
	attachment := gjson.Get(line, "attachment")
	attachmentType := attachment.Get("type")
	if !IsClaudeHookSubtype(attachmentType.Str) || attachmentType.Str == "stop_hook_summary" {
		return claudeHookSuccess{}, false
	}
	payload := claudeHookSuccess{subtype: attachmentType.Str}
	members := []struct {
		key    string
		target *string
	}{
		{key: "hookName", target: &payload.hookName},
		{key: "hookEvent", target: &payload.hookEvent},
		{key: "command", target: &payload.command},
		{key: "toolUseID", target: &payload.toolUseID},
		{key: "content", target: &payload.content},
		{key: "stdout", target: &payload.stdout},
		{key: "stderr", target: &payload.stderr},
	}
	for _, member := range members {
		value := attachment.Get(member.key)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		if member.key == "content" && payload.subtype == "hook_additional_context" {
			if !value.IsArray() {
				return claudeHookSuccess{}, false
			}
			var parts []string
			for _, part := range value.Array() {
				if part.Type != gjson.String {
					return claudeHookSuccess{}, false
				}
				parts = append(parts, part.Str)
			}
			payload.content = strings.Join(parts, "\n")
			continue
		}
		if value.Type != gjson.String {
			return claudeHookSuccess{}, false
		}
		*member.target = value.Str
	}
	if payload.subtype == "hook_additional_context" {
		if payload.content == "" {
			return claudeHookSuccess{}, false
		}
		return payload, true
	}
	if payload.subtype == "hook_cancelled" {
		var cancellation struct {
			TimedOut  bool     `json:"timedOut"`
			TimeoutMs *float64 `json:"timeoutMs"`
		}
		if err := json.Unmarshal([]byte(attachment.Raw), &cancellation); err != nil {
			return claudeHookSuccess{}, false
		}
		payload.timedOut = cancellation.TimedOut
		payload.timeoutMs = cancellation.TimeoutMs
		return payload, true
	}
	if strings.TrimSpace(payload.content) == "" &&
		strings.TrimSpace(payload.stdout) == "" &&
		strings.TrimSpace(payload.stderr) == "" {
		return claudeHookSuccess{}, false
	}
	return payload, true
}

// extractClaudeHookAttachment decodes one transcript line into a
// deferred hook attachment. It reuses the strict hook output decoder,
// skipping unsupported types, malformed payloads, and empty output unless
// the hook was cancelled or has summary metadata. The record keeps its identity.
func extractClaudeHookAttachment(line string, stopSummary bool) (claudeAttachment, bool) {
	payload, ok := extractClaudeHookSuccess(line, stopSummary)
	if !ok {
		return claudeAttachment{}, false
	}
	// gjson strings can share the input buffer. Retain only the known
	// fields rather than the rest of a potentially large transcript row.
	payload.subtype = strings.Clone(payload.subtype)
	payload.hookName = strings.Clone(payload.hookName)
	payload.hookEvent = strings.Clone(payload.hookEvent)
	payload.command = strings.Clone(payload.command)
	payload.toolUseID = strings.Clone(payload.toolUseID)
	payload.content = strings.Clone(payload.content)
	payload.stdout = strings.Clone(payload.stdout)
	payload.stderr = strings.Clone(payload.stderr)
	return claudeAttachment{
		timestamp:        extractTimestamp(line),
		hook:             &payload,
		sourceUUID:       strings.Clone(gjson.Get(line, "uuid").Str),
		sourceParentUUID: strings.Clone(gjson.Get(line, "parentUuid").Str),
		isSidechain:      gjson.Get(line, "isSidechain").Bool(),
		promptSource:     strings.Clone(gjson.Get(line, "promptSource").Str),
	}, true
}

// claudeHookSuccessBody renders a decoded hook payload as a
// plain, inspectable body. Every recorded channel keeps its own
// section, the recorded text is copied verbatim, and a section appears
// only when its decoded value is not the empty string.
func claudeHookSuccessBody(payload claudeHookSuccess) string {
	sections := make([]string, 0, 7)
	if payload.hookName != "" {
		sections = append(sections, "Hook name: "+payload.hookName)
	}
	if payload.hookEvent != "" {
		sections = append(sections, "Hook event: "+payload.hookEvent)
	}
	if payload.command != "" {
		sections = append(sections, "Command: "+payload.command)
	}
	if payload.toolUseID != "" {
		sections = append(sections, "Tool-use id: "+payload.toolUseID)
	}
	if payload.timedOut {
		timeout := "Timed out"
		if payload.timeoutMs != nil {
			timeout = fmt.Sprintf("Timed out after %.0f ms", *payload.timeoutMs)
		}
		sections = append(sections, timeout)
	}
	if summary := payload.summary; summary != nil {
		if summary.HookLabel != "" {
			sections = append(sections, "Hook event: "+summary.HookLabel)
		}
		if summary.HookCount != nil {
			sections = append(sections, fmt.Sprintf("Hook count: %d", *summary.HookCount))
		}
		for _, info := range summary.HookInfos {
			if info.Command != "" {
				sections = append(sections, "Command: "+info.Command)
			}
			if info.PromptText != "" {
				sections = append(sections, "Prompt text:\n"+info.PromptText)
			}
		}
		if context := strings.Join(summary.HookAdditionalContext, "\n"); context != "" {
			sections = append(sections, "Additional context:\n"+context)
		}
		if len(summary.HookErrors) > 0 {
			sections = append(sections, "Hook errors:\n"+strings.Join(summary.HookErrors, "\n"))
		}
		if summary.PreventedContinuation != nil {
			if *summary.PreventedContinuation {
				sections = append(sections, "Assistant blocked")
			} else {
				sections = append(sections, "Assistant allowed to continue")
			}
		}
		if summary.StopReason != "" {
			sections = append(sections, "Stop reason: "+summary.StopReason)
		}
	}
	if payload.content != "" {
		sections = append(sections, "Content:\n"+payload.content)
	}
	if payload.stdout != "" {
		sections = append(sections, "Output:\n"+payload.stdout)
	}
	if payload.stderr != "" {
		sections = append(sections, "Error output:\n"+payload.stderr)
	}
	return strings.Join(sections, "\n\n")
}

// claudeHookSuccessMessage builds the row a hook output record
// becomes. The row keeps Role=user to match the project's system-row
// convention, so role-keyed analytics do not read it as an assistant
// reply, and it stays a system row so it never becomes a user prompt
// or the session preview. The body carries no tool, thinking, token
// usage, model, or billing identity.
func claudeHookSuccessMessage(a claudeAttachment) ParsedMessage {
	body := claudeHookSuccessBody(*a.hook)
	sourceType := "attachment"
	if a.hook.subtype == "stop_hook_summary" {
		sourceType = "system"
	}
	return ParsedMessage{
		Role:             RoleUser,
		Content:          body,
		Timestamp:        a.timestamp,
		IsSystem:         true,
		ContentLength:    len(body),
		SourceType:       sourceType,
		SourceSubtype:    a.hook.subtype,
		SourceUUID:       a.sourceUUID,
		SourceParentUUID: a.sourceParentUUID,
		IsSidechain:      a.isSidechain,
		PromptSource:     a.promptSource,
	}
}
