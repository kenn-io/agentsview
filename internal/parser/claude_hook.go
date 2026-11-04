package parser

import (
	"strings"

	"github.com/tidwall/gjson"
)

// claudeHookSuccess holds the text a supported Claude Code hook attachment
// carries. A missing member and an empty member are both an empty string.
type claudeHookSuccess struct {
	subtype   string
	hookName  string
	hookEvent string
	command   string
	toolUseID string
	content   string
	stdout    string
	stderr    string
}

// IsClaudeHookSubtype reports whether the parser imports this hook subtype.
func IsClaudeHookSubtype(subtype string) bool {
	return subtype == "hook_success" || subtype == "hook_additional_context" || subtype == "hook_cancelled"
}

// extractClaudeHookSuccess decodes the hook output payload of one
// transcript line. It decodes the payload only: it does not store, order,
// or display the result.
func extractClaudeHookSuccess(line string) (claudeHookSuccess, bool) {
	attachment := gjson.Get(line, "attachment")
	attachmentType := attachment.Get("type")
	if !IsClaudeHookSubtype(attachmentType.Str) {
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
		if !value.Exists() {
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
// the hook was cancelled. The record keeps its own top-level identity.
func extractClaudeHookAttachment(line string) (claudeAttachment, bool) {
	payload, ok := extractClaudeHookSuccess(line)
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
	if payload.content != "" {
		sections = append(sections, "Content:\n"+payload.content)
	}
	if payload.stdout != "" {
		sections = append(sections, "Stdout:\n"+payload.stdout)
	}
	if payload.stderr != "" {
		sections = append(sections, "Stderr:\n"+payload.stderr)
	}
	return strings.Join(sections, "\n\n")
}

// claudeHookSuccessMessage builds the row a hook output attachment
// becomes. The row keeps Role=user to match the project's system-row
// convention, so role-keyed analytics do not read it as an assistant
// reply, and it stays a system row so it never becomes a user prompt
// or the session preview. The body carries no tool, thinking, token
// usage, model, or billing identity.
func claudeHookSuccessMessage(a claudeAttachment) ParsedMessage {
	body := claudeHookSuccessBody(*a.hook)
	dialogue := ""
	return ParsedMessage{
		Role:             RoleUser,
		Content:          body,
		DialogueText:     new(dialogue),
		Timestamp:        a.timestamp,
		IsSystem:         true,
		ContentLength:    len(body),
		SourceType:       "attachment",
		SourceSubtype:    a.hook.subtype,
		SourceUUID:       a.sourceUUID,
		SourceParentUUID: a.sourceParentUUID,
		IsSidechain:      a.isSidechain,
		PromptSource:     a.promptSource,
	}
}
